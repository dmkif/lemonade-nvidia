package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLem struct {
	mu         sync.Mutex
	loaded     []Loaded
	unloaded   []string
	downloaded map[string]bool
	backends   map[string]bool
	pulled     []string
	installed  []string
}

func (f *fakeLem) Ready(context.Context) bool { return true }
func (f *fakeLem) Loaded(context.Context) ([]Loaded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Loaded(nil), f.loaded...), nil
}
func (f *fakeLem) Load(_ context.Context, m string) error {
	f.mu.Lock()
	f.loaded = append(f.loaded, Loaded{Name: m})
	f.mu.Unlock()
	return nil
}
func (f *fakeLem) Unload(_ context.Context, m string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unloaded = append(f.unloaded, m)
	if m == "" {
		f.loaded = nil
		return nil
	}
	var r []Loaded
	for _, l := range f.loaded {
		if l.Name != m {
			r = append(r, l)
		}
	}
	f.loaded = r
	return nil
}
func (f *fakeLem) Downloaded(context.Context) (map[string]bool, error) { return f.downloaded, nil }
func (f *fakeLem) BackendInstalled(_ context.Context, r, b string) (bool, error) {
	return f.backends[r+":"+b], nil
}
func (f *fakeLem) Install(_ context.Context, r, b string) error {
	f.installed = append(f.installed, r+":"+b)
	return nil
}
func (f *fakeLem) Pull(_ context.Context, m string) error { f.pulled = append(f.pulled, m); return nil }

type fakeGPU struct{ used atomic.Int64 }

func (g *fakeGPU) UsedMiB(context.Context) (int, error) { return int(g.used.Load()), nil }

func testCfg(upstream string) *Config {
	c := &Config{Listen: ":0", Upstream: upstream, BudgetMiB: 11000, Retry5xx: 1, LeakMiB: 1500,
		Models: map[string]ModelCfg{
			"qwen":  {ResidentMiB: 7400, PeakMiB: 7400},
			"embed": {ResidentMiB: 500, PeakMiB: 500, Pinned: true},
			"music": {ResidentMiB: 0, PeakMiB: 9800},
		}}
	c.IdleUnload.Duration = 10 * time.Minute
	c.MaxWait.Duration = 2 * time.Second
	c.LeakAfter.Duration = time.Minute
	c.DrainWait.Duration = 2 * time.Second
	return c
}

type env struct {
	g   *Guard
	arb *Arbiter
	lem *fakeLem
	gpu *fakeGPU
	up  *httptest.Server
	srv *httptest.Server
}

func newEnv(t *testing.T, h http.HandlerFunc) *env {
	t.Helper()
	up := httptest.NewServer(h)
	cfg := testCfg(up.URL)
	lem := &fakeLem{}
	gpu := &fakeGPU{}
	m := NewMetrics()
	arb := NewArbiter(cfg, lem, gpu, m)
	g := NewGuard(cfg, arb, lem, m, "admin")
	srv := httptest.NewServer(g)
	t.Cleanup(func() { srv.Close(); up.Close() })
	return &env{g, arb, lem, gpu, up, srv}
}

func (e *env) do(method, path, body string, hdr ...string) *http.Response {
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer user")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func ok200(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }

func TestAllowlist(t *testing.T) {
	e := newEnv(t, ok200)
	for _, c := range []struct {
		m, p string
		code int
	}{
		{"GET", "/v1/models", 200},
		{"POST", "/v1/chat/completions", 200},
		{"POST", "/v1/embeddings", 200},
		{"POST", "/v1/pull", 403},
		{"POST", "/v1/delete", 403},
		{"POST", "/v1/load", 403},
		{"POST", "/v1/unload", 403},
		{"POST", "/v1/install", 403},
		{"GET", "/internal/config", 403},
		{"GET", "/v1/chat/completions", 403},
	} {
		if got := e.do(c.m, c.p, `{"model":"qwen"}`).StatusCode; got != c.code {
			t.Errorf("%s %s = %d, want %d", c.m, c.p, got, c.code)
		}
	}
}

func TestBearerPassedThrough(t *testing.T) {
	var got atomic.Value
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) { got.Store(r.Header.Get("Authorization")) })
	e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).Body.Close()
	if got.Load() != "Bearer user" {
		t.Fatalf("auth header = %v", got.Load())
	}
}

func TestBatchBlocksChatAndWaits(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/audio/generations" {
			<-release
		}
		io.WriteString(w, "ok")
	})
	done := make(chan int, 1)
	go func() { done <- e.do("POST", "/v1/audio/generations", `{"model":"music"}`).StatusCode }()
	time.Sleep(100 * time.Millisecond)
	chat := make(chan int, 1)
	go func() { chat <- e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).StatusCode }()
	select {
	case <-chat:
		t.Fatal("chat ran during music")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if c := <-done; c != 200 {
		t.Fatalf("music = %d", c)
	}
	if c := <-chat; c != 200 {
		t.Fatalf("chat after music = %d", c)
	}
}

func TestBatchWaitsForRunningChat(t *testing.T) {
	release := make(chan struct{})
	var musicStarted atomic.Bool
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			<-release
		} else {
			musicStarted.Store(true)
		}
		io.WriteString(w, "ok")
	})
	chat := make(chan int, 1)
	go func() { chat <- e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).StatusCode }()
	time.Sleep(100 * time.Millisecond)
	music := make(chan int, 1)
	go func() { music <- e.do("POST", "/v1/audio/generations", `{"model":"music"}`).StatusCode }()
	time.Sleep(300 * time.Millisecond)
	if musicStarted.Load() {
		t.Fatal("music started while chat running")
	}
	close(release)
	<-chat
	if c := <-music; c != 200 {
		t.Fatalf("music = %d", c)
	}
}

func TestTwoMusicSequential(t *testing.T) {
	var cur, max atomic.Int32
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		if n > max.Load() {
			max.Store(n)
		}
		time.Sleep(150 * time.Millisecond)
		cur.Add(-1)
		io.WriteString(w, "ok")
	})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.do("POST", "/v1/audio/generations", `{"model":"music"}`).Body.Close() }()
	}
	wg.Wait()
	if max.Load() != 1 {
		t.Fatalf("parallel music jobs: %d", max.Load())
	}
}

func TestWaitLimit503(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	e.g.cfg.MaxWait.Duration = 150 * time.Millisecond
	go e.do("POST", "/v1/audio/generations", `{"model":"music"}`)
	time.Sleep(80 * time.Millisecond)
	resp := e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestBudgetUnloadsChatNotPinnedEmbedding(t *testing.T) {
	e := newEnv(t, ok200)
	e.lem.loaded = []Loaded{{Name: "qwen", Type: "llm"}, {Name: "embed", Type: "embedding"}}
	if c := e.do("POST", "/v1/audio/generations", `{"model":"music"}`).StatusCode; c != 200 {
		t.Fatalf("music = %d", c)
	}
	if len(e.lem.unloaded) != 1 || e.lem.unloaded[0] != "qwen" {
		t.Fatalf("unloaded = %v, want [qwen]", e.lem.unloaded)
	}
}

func TestBudgetKeepsEverythingWhenFits(t *testing.T) {
	e := newEnv(t, ok200)
	e.lem.loaded = []Loaded{{Name: "music", Type: "audio-generation"}, {Name: "embed"}}
	e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).Body.Close()
	if len(e.lem.unloaded) != 0 {
		t.Fatalf("unloaded = %v", e.lem.unloaded)
	}
}

func TestRetryAfter5xx(t *testing.T) {
	var calls atomic.Int32
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "synth failed", 500)
			return
		}
		io.WriteString(w, "wav")
	})
	e.lem.loaded = []Loaded{{Name: "music"}}
	resp := e.do("POST", "/v1/audio/generations", `{"model":"music"}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "wav" || calls.Load() != 2 {
		t.Fatalf("code %d body %q calls %d", resp.StatusCode, b, calls.Load())
	}
	if len(e.lem.unloaded) == 0 || e.lem.unloaded[0] != "" {
		t.Fatalf("recover did not unload all: %v", e.lem.unloaded)
	}
}

func TestRetryOnlyOnce(t *testing.T) {
	var calls atomic.Int32
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "x", 500) })
	if c := e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).StatusCode; c != 500 {
		t.Fatalf("code %d", c)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestSyncPullsOnlyMissing(t *testing.T) {
	e := newEnv(t, ok200)
	e.lem.downloaded = map[string]bool{"qwen": true}
	e.lem.backends = map[string]bool{"llamacpp:vulkan": true}
	e.g.cfg.Backends = []string{"llamacpp:vulkan", "acestep:vulkan"}
	e.g.cfg.SyncModels = []string{"qwen", "embed", "music"}
	s := &Syncer{cfg: e.g.cfg, lem: e.lem, arb: e.arb, m: e.g.m}
	if !s.Once(context.Background()) {
		t.Fatal("sync reported missing")
	}
	if strings.Join(e.lem.pulled, ",") != "embed,music" || strings.Join(e.lem.installed, ",") != "acestep:vulkan" {
		t.Fatalf("pulled %v installed %v", e.lem.pulled, e.lem.installed)
	}
}

func TestIdleUnload(t *testing.T) {
	e := newEnv(t, ok200)
	now := time.Now()
	e.arb.now = func() time.Time { return now }
	e.lem.loaded = []Loaded{{Name: "qwen"}, {Name: "music"}}
	e.arb.Tick(context.Background()) // first sight starts the idle clock
	if len(e.lem.unloaded) != 0 {
		t.Fatalf("unloaded too early: %v", e.lem.unloaded)
	}
	now = now.Add(11 * time.Minute)
	e.arb.Tick(context.Background())
	if len(e.lem.unloaded) != 1 || e.lem.unloaded[0] != "qwen" { // music has resident 0: nothing to free
		t.Fatalf("unloaded = %v", e.lem.unloaded)
	}
}

func TestLeakKillsBackends(t *testing.T) {
	e := newEnv(t, ok200)
	now := time.Now()
	e.arb.now = func() time.Time { return now }
	killed := make(chan struct{}, 1)
	e.arb.kill = func() { killed <- struct{}{} }
	e.gpu.used.Store(8700) // nothing loaded, nothing in flight
	e.arb.Tick(context.Background())
	now = now.Add(2 * time.Minute)
	e.arb.Tick(context.Background())
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("no kill")
	}
}

func TestNoLeakWhenModelResident(t *testing.T) {
	e := newEnv(t, ok200)
	now := time.Now()
	e.arb.now = func() time.Time { return now }
	e.arb.kill = func() { t.Error("killed") }
	e.lem.loaded = []Loaded{{Name: "qwen"}}
	e.gpu.used.Store(7500)
	e.arb.Tick(context.Background())
	now = now.Add(2 * time.Minute)
	e.arb.Tick(context.Background())
}

func TestAsyncJob(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "audio/wav")
		io.WriteString(w, "RIFFdata")
	})
	resp := e.do("POST", "/guard/jobs/audio", `{"model":"music"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("submit = %d", resp.StatusCode)
	}
	var id string
	b, _ := io.ReadAll(resp.Body)
	id = strings.Split(strings.Split(string(b), `"id":"`)[1], `"`)[0]
	for i := 0; i < 50; i++ {
		r := e.do("GET", "/guard/jobs/"+id+"/result", "")
		if r.StatusCode == 200 {
			d, _ := io.ReadAll(r.Body)
			if string(d) != "RIFFdata" || r.Header.Get("Content-Type") != "audio/wav" {
				t.Fatalf("result %q %q", d, r.Header.Get("Content-Type"))
			}
			// another caller must not see the job
			if c := e.do("GET", "/guard/jobs/"+id, "", "Authorization", "Bearer other").StatusCode; c != 404 {
				t.Fatalf("foreign job access = %d", c)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not finish")
}

func TestDrainOnlyFromLoopbackAndRefusesNew(t *testing.T) {
	e := newEnv(t, ok200)
	req := httptest.NewRequest("POST", "/guard/drain", nil)
	req.RemoteAddr = "10.42.1.7:5555" // gateway pod
	rec := httptest.NewRecorder()
	e.g.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("drain from pod network = %d", rec.Code)
	}
	if c := e.do("POST", "/guard/drain", "").StatusCode; c != 200 { // test client is loopback
		t.Fatalf("drain from loopback = %d", c)
	}
	resp := e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("after drain %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestMetricsAndStatus(t *testing.T) {
	e := newEnv(t, ok200)
	e.do("POST", "/v1/pull", "{}").Body.Close()
	e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).Body.Close()
	b, _ := io.ReadAll(e.do("GET", "/metrics", "").Body)
	for _, want := range []string{`guard_rejected_total{reason="forbidden"} 1`, `guard_requests_total{class="llm",code="200"} 1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %q in:\n%s", want, b)
		}
	}
	s, _ := io.ReadAll(e.do("GET", "/guard/status", "").Body)
	if !strings.Contains(string(s), `"state":"ready"`) {
		t.Errorf("status %s", s)
	}
}

func cpuCfg(e *env) {
	mc := e.g.cfg.Models["qwen"]
	mc.CPUFallback = &CPUFallback{Alias: "qwen-cpu", MaxPromptChars: 500}
	e.g.cfg.Models["qwen"] = mc
}

// musicWithModelLog starts a blocked music job and records the model each chat request carries.
func musicEnv(t *testing.T) (*env, chan struct{}, *atomic.Value) {
	release := make(chan struct{})
	var lastModel atomic.Value
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/audio/generations" {
			<-release
		} else {
			b, _ := io.ReadAll(r.Body)
			lastModel.Store(modelOf(b))
		}
		io.WriteString(w, "ok")
	})
	cpuCfg(e)
	e.g.cfg.MaxWait.Duration = 300 * time.Millisecond
	go e.do("POST", "/v1/audio/generations", `{"model":"music"}`)
	time.Sleep(100 * time.Millisecond)
	return e, release, &lastModel
}

func TestCPUFallbackDuringMusic(t *testing.T) {
	e, release, last := musicEnv(t)
	defer close(release)
	resp := e.do("POST", "/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || last.Load() != "qwen-cpu" {
		t.Fatalf("code %d model %v", resp.StatusCode, last.Load())
	}
	b, _ := io.ReadAll(e.do("GET", "/metrics", "").Body)
	if !strings.Contains(string(b), "guard_cpu_fallback_total 1") {
		t.Fatalf("metric missing:\n%s", b)
	}
}

func TestNoCPUFallbackForImageOrBigOrIdleGPU(t *testing.T) {
	e, release, last := musicEnv(t)
	for name, body := range map[string]string{
		"image": `{"model":"qwen","messages":[{"content":[{"type":"image_url"}]}]}`,
		"big":   `{"model":"qwen","x":"` + strings.Repeat("a", 600) + `"}`,
	} {
		if c := e.do("POST", "/v1/chat/completions", body).StatusCode; c != 503 {
			t.Errorf("%s: want 503 wait limit, got %d", name, c)
		}
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	// GPU free again: the original model is used
	e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).Body.Close()
	if last.Load() != "qwen" {
		t.Fatalf("model after music = %v", last.Load())
	}
}

func TestGPUChatWaitsForCPUVariantAndMusicDoesNot(t *testing.T) {
	e := newEnv(t, ok200)
	cpuCfg(e)
	rel, err := e.arb.AcquireCPU() // CPU request in flight
	if err != nil {
		t.Fatal(err)
	}
	// music must start despite the CPU request
	done := make(chan int, 1)
	go func() { done <- e.do("POST", "/v1/audio/generations", `{"model":"music"}`).StatusCode }()
	select {
	case c := <-done:
		if c != 200 {
			t.Fatalf("music = %d", c)
		}
	case <-time.After(time.Second):
		t.Fatal("music blocked by CPU request")
	}
	// a GPU chat request waits until the CPU variant is done (shared llm slot)
	chat := make(chan int, 1)
	go func() { chat <- e.do("POST", "/v1/chat/completions", `{"model":"qwen"}`).StatusCode }()
	select {
	case <-chat:
		t.Fatal("GPU chat ran while CPU variant in flight")
	case <-time.After(300 * time.Millisecond):
	}
	rel()
	if c := <-chat; c != 200 {
		t.Fatalf("chat = %d", c)
	}
}

func TestClientCancelIsNotABackendFailure(t *testing.T) {
	hang := make(chan struct{})
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) { <-hang })
	defer close(hang)
	e.lem.loaded = []Loaded{{Name: "qwen"}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected client timeout")
	}
	time.Sleep(200 * time.Millisecond)
	if len(e.lem.unloaded) != 0 {
		t.Fatalf("client cancel unloaded models: %v", e.lem.unloaded)
	}
	b, _ := io.ReadAll(e.do("GET", "/metrics", "").Body)
	if !strings.Contains(string(b), "guard_retries_total 0") {
		t.Fatalf("retry counted:\n%s", b)
	}
}

func TestStatusSyncCompleteFlag(t *testing.T) {
	e := newEnv(t, ok200)
	e.lem.downloaded = map[string]bool{"qwen": true}
	e.g.cfg.SyncModels = []string{"qwen"}
	s := &Syncer{cfg: e.g.cfg, lem: e.lem, arb: e.arb, m: e.g.m}
	e.g.syncer = s
	b, _ := io.ReadAll(e.do("GET", "/guard/status", "").Body)
	if !strings.Contains(string(b), `"complete":false`) {
		t.Fatalf("before first pass: %s", b)
	}
	s.Once(context.Background())
	b, _ = io.ReadAll(e.do("GET", "/guard/status", "").Body)
	if !strings.Contains(string(b), `"complete":true`) {
		t.Fatalf("after pass: %s", b)
	}
}
