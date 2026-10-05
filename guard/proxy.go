package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 64 << 20

type Guard struct {
	cfg      *Config
	arb      *Arbiter
	lem      Lemonade
	m        *Metrics
	adminKey string
	syncer   *Syncer
	c        *http.Client

	jobsMu sync.Mutex
	jobs   map[string]*Job
}

type Job struct {
	ID      string `json:"id"`
	State   string `json:"state"` // queued, running, done, failed
	Error   string `json:"error,omitempty"`
	auth    string
	file    string
	ctype   string
	created time.Time
}

func NewGuard(cfg *Config, arb *Arbiter, lem Lemonade, m *Metrics, adminKey string) *Guard {
	return &Guard{cfg: cfg, arb: arb, lem: lem, m: m, adminKey: adminKey, c: &http.Client{}, jobs: map[string]*Job{}}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, typ string, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeJSON(w, code, map[string]any{"error": map[string]string{"type": typ}})
}

func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/metrics" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		g.m.Write(w, len(g.arb.Snapshot(r.Context())))
		return
	case p == "/guard/status" && r.Method == "GET":
		g.status(w, r)
		return
	case p == "/guard/drain" && r.Method == "POST":
		g.drain(w, r)
		return
	case p == "/guard/jobs/audio" && r.Method == "POST":
		g.submitJob(w, r)
		return
	case strings.HasPrefix(p, "/guard/jobs/") && r.Method == "GET":
		g.getJob(w, r)
		return
	}
	var rt *route
	for i := range allowlist {
		if allowlist[i].method == r.Method && allowlist[i].path == p {
			rt = &allowlist[i]
			break
		}
	}
	if rt == nil {
		g.m.reject("forbidden")
		g.m.request("denied", 403)
		errJSON(w, 403, "forbidden", 0)
		return
	}
	if rt.free {
		g.forward(w, r, nil, "free")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		errJSON(w, 413, "body_too_large", 0)
		return
	}
	g.serveAdmitted(w, r, body, rt.class)
}

func modelOf(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Model
}

// cpuFallbackFor returns the fallback config when this request may run on the CPU variant now:
// text-only, small, and the GPU is owned by a batch job.
func (g *Guard) cpuFallbackFor(cls class, model string, body []byte) *CPUFallback {
	fb := g.cfg.Models[model].CPUFallback
	if cls != classLLM || fb == nil || fb.Alias == "" || !g.arb.BatchRunning() {
		return nil
	}
	if (fb.MaxPromptChars > 0 && len(body) > fb.MaxPromptChars) ||
		bytes.Contains(body, []byte("image_url")) || bytes.Contains(body, []byte("input_audio")) {
		return nil
	}
	return fb
}

func rewriteModel(body []byte, alias string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["model"], _ = json.Marshal(alias)
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

func (g *Guard) serveAdmitted(w http.ResponseWriter, r *http.Request, body []byte, cls class) {
	if fb := g.cpuFallbackFor(cls, modelOf(body), body); fb != nil {
		if release, err := g.arb.AcquireCPU(); err == nil {
			defer release()
			g.m.cpuFallback()
			g.forward(w, r, rewriteModel(body, fb.Alias), "llm_cpu")
			return
		}
	}
	actx, cancel := context.WithTimeout(r.Context(), g.cfg.MaxWait.Duration)
	defer cancel()
	release, err := g.arb.Acquire(actx, cls, modelOf(body))
	if err != nil {
		if err == errDraining {
			g.m.reject("draining")
			g.m.request(cls.String(), 503)
			errJSON(w, 503, "draining", 60)
			return
		}
		g.m.request(cls.String(), 503)
		errJSON(w, 503, "wait_limit", 30)
		return
	}
	defer release()
	g.forward(w, r, body, cls.String())
}

// forward sends the request to Lemonade; on 5xx it frees the GPU and retries (retry_on_5xx times).
func (g *Guard) forward(w http.ResponseWriter, r *http.Request, body []byte, label string) {
	for attempt := 0; ; attempt++ {
		resp, err := g.upstream(r.Context(), r.Method, r.URL.RequestURI(), r.Header, body)
		if err == nil && resp.StatusCode < 500 || attempt >= g.cfg.Retry5xx || body == nil {
			if err != nil {
				g.m.request(label, 502)
				errJSON(w, 502, "upstream_unreachable", 10)
				return
			}
			defer resp.Body.Close()
			for _, h := range []string{"Content-Type", "Content-Length", "Retry-After", "Cache-Control"} {
				if v := resp.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			g.m.request(label, resp.StatusCode)
			streamCopy(w, resp.Body)
			return
		}
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		log.Printf("upstream failed (%v, attempt %d), recovering", err, attempt+1)
		g.m.retry()
		g.arb.Recover(r.Context())
	}
}

func (g *Guard) upstream(ctx context.Context, method, uri string, h http.Header, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.cfg.Upstream+uri, rd)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"Authorization", "Content-Type", "Accept"} {
		if v := h.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	return g.c.Do(req)
}

func streamCopy(w http.ResponseWriter, r io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// isAdmin: drain is only for callers inside the pod (preStop, kubectl exec / port-forward);
// the gateway reaches the guard from another pod IP and cannot use it.
func (g *Guard) isAdmin(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (g *Guard) status(w http.ResponseWriter, r *http.Request) {
	state := "ready"
	switch {
	case g.arb.Draining():
		state = "draining"
	case !g.lem.Ready(r.Context()):
		state = "starting"
	}
	st := map[string]any{"state": state, "loaded": g.arb.Snapshot(r.Context())}
	if g.syncer != nil {
		st["sync"] = map[string]any{"missing_backends": nz(g.syncer.Missing.Backends), "missing_models": nz(g.syncer.Missing.Models)}
	}
	writeJSON(w, 200, st)
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (g *Guard) drain(w http.ResponseWriter, r *http.Request) {
	if !g.isAdmin(r) {
		errJSON(w, 403, "forbidden", 0)
		return
	}
	if err := g.arb.Drain(r.Context()); err != nil {
		errJSON(w, 504, "drain_timeout", 0)
		return
	}
	writeJSON(w, 200, map[string]string{"state": "drained"})
}

// ---- async jobs ----

func authHash(r *http.Request) string {
	s := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return hex.EncodeToString(s[:])
}

func (g *Guard) submitJob(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		errJSON(w, 413, "body_too_large", 0)
		return
	}
	if g.arb.Draining() {
		errJSON(w, 503, "draining", 60)
		return
	}
	b := make([]byte, 12)
	f, _ := os.Open("/dev/urandom")
	f.Read(b)
	f.Close()
	j := &Job{ID: hex.EncodeToString(b), State: "queued", auth: authHash(r), created: time.Now()}
	g.jobsMu.Lock()
	g.reapJobs()
	g.jobs[j.ID] = j
	g.jobsMu.Unlock()
	hdr := r.Header.Clone()
	go g.runJob(j, body, hdr)
	writeJSON(w, 202, map[string]string{"id": j.ID, "state": "queued"})
}

func (g *Guard) setJob(j *Job, f func()) { g.jobsMu.Lock(); f(); g.jobsMu.Unlock() }

func (g *Guard) runJob(j *Job, body []byte, hdr http.Header) {
	ctx := context.Background()
	actx, cancel := context.WithTimeout(ctx, g.cfg.MaxWait.Duration)
	defer cancel()
	release, err := g.arb.Acquire(actx, classBatch, modelOf(body))
	if err != nil {
		g.setJob(j, func() { j.State, j.Error = "failed", err.Error() })
		return
	}
	defer release()
	g.setJob(j, func() { j.State = "running" })
	start := time.Now()
	for attempt := 0; ; attempt++ {
		resp, err := g.upstream(ctx, "POST", "/v1/audio/generations", hdr, body)
		if err == nil && resp.StatusCode == 200 {
			f, ferr := os.CreateTemp("", "guard-job-*")
			if ferr == nil {
				_, ferr = io.Copy(f, resp.Body)
				f.Close()
			}
			ct := resp.Header.Get("Content-Type")
			resp.Body.Close()
			if ferr != nil {
				g.setJob(j, func() { j.State, j.Error = "failed", ferr.Error() })
				return
			}
			g.setJob(j, func() { j.State, j.file, j.ctype = "done", f.Name(), ct })
			g.m.job("audio", time.Since(start))
			return
		}
		msg := fmt.Sprint(err)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			resp.Body.Close()
			msg = fmt.Sprintf("%d %s", resp.StatusCode, b)
		}
		if attempt >= g.cfg.Retry5xx {
			g.setJob(j, func() { j.State, j.Error = "failed", msg })
			return
		}
		g.m.retry()
		g.arb.Recover(ctx)
	}
}

func (g *Guard) reapJobs() { // jobsMu held
	for id, j := range g.jobs {
		if time.Since(j.created) > time.Hour {
			if j.file != "" {
				os.Remove(j.file)
			}
			delete(g.jobs, id)
		}
	}
}

func (g *Guard) getJob(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/guard/jobs/")
	id, result := strings.TrimSuffix(rest, "/result"), strings.HasSuffix(rest, "/result")
	g.jobsMu.Lock()
	j, ok := g.jobs[id]
	var snap Job
	if ok {
		snap = *j
	}
	g.jobsMu.Unlock()
	if !ok || snap.auth != authHash(r) {
		errJSON(w, 404, "not_found", 0)
		return
	}
	if !result {
		writeJSON(w, 200, snap)
		return
	}
	if snap.State != "done" {
		errJSON(w, 409, "not_done", 10)
		return
	}
	f, err := os.Open(snap.file)
	if err != nil {
		errJSON(w, 410, "gone", 0)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", snap.ctype)
	io.Copy(w, f)
}
