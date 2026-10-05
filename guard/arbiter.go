package main

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

var (
	errWaitLimit = errors.New("wait limit exceeded")
	errDraining  = errors.New("draining")
)

// Arbiter admits requests so that the loaded models always fit into the VRAM budget and a
// batch job (music, images) owns the GPU alone.
type Arbiter struct {
	cfg *Config
	lem Lemonade
	gpu VRAM
	// kill terminates backend processes when VRAM stays occupied without any model loaded.
	kill func()
	now  func() time.Time

	mu           sync.Mutex
	inflight     map[string]int
	total        int
	batchRunning bool
	lastUse      map[string]time.Time
	draining     bool
	wake         chan struct{}
	leakSince    time.Time
	m            *Metrics

	opMu sync.Mutex // serialises unload decisions
}

func NewArbiter(cfg *Config, lem Lemonade, gpu VRAM, m *Metrics) *Arbiter {
	return &Arbiter{cfg: cfg, lem: lem, gpu: gpu, kill: func() {}, now: time.Now, m: m,
		inflight: map[string]int{}, lastUse: map[string]time.Time{}, wake: make(chan struct{})}
}

func (a *Arbiter) broadcast() { close(a.wake); a.wake = make(chan struct{}) }

// Acquire blocks until the request may run. The returned func releases the reservation.
func (a *Arbiter) Acquire(ctx context.Context, cls class, model string) (func(), error) {
	start := a.now()
	for {
		a.mu.Lock()
		if a.draining {
			a.mu.Unlock()
			return nil, errDraining
		}
		ok := !a.batchRunning
		if cls == classBatch {
			ok = ok && a.total == 0
		}
		if ok {
			a.inflight[model]++
			a.total++
			if cls == classBatch {
				a.batchRunning = true
			}
			a.mu.Unlock()
			break
		}
		ch := a.wake
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			a.m.reject("wait_limit")
			return nil, errWaitLimit
		}
	}
	a.m.observeWait(a.now().Sub(start))
	release := func() {
		a.mu.Lock()
		a.inflight[model]--
		a.total--
		if cls == classBatch {
			a.batchRunning = false
		}
		a.lastUse[model] = a.now()
		a.broadcast()
		a.mu.Unlock()
	}
	if err := a.makeRoom(ctx, model); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (a *Arbiter) resident(name string) int { return a.cfg.Models[name].ResidentMiB }

// makeRoom unloads idle models until resident(others) + peak(model) fits into the budget.
func (a *Arbiter) makeRoom(ctx context.Context, model string) error {
	if model == "" {
		return nil
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	loaded, err := a.lem.Loaded(ctx)
	if err != nil {
		return nil // Lemonade will decide; do not block on a health glitch
	}
	need := a.cfg.Models[model].PeakMiB
	used := 0
	var cand []Loaded
	for _, l := range loaded {
		if l.Name == model {
			continue
		}
		used += a.resident(l.Name)
		cand = append(cand, l)
	}
	if used+need <= a.cfg.BudgetMiB {
		return nil
	}
	a.mu.Lock()
	sort.SliceStable(cand, func(i, j int) bool {
		pi, pj := a.cfg.Models[cand[i].Name].Pinned, a.cfg.Models[cand[j].Name].Pinned
		if pi != pj {
			return !pi
		}
		return a.lastUse[cand[i].Name].Before(a.lastUse[cand[j].Name])
	})
	busy := map[string]bool{}
	for _, l := range cand {
		busy[l.Name] = a.inflight[l.Name] > 0 || l.Busy
	}
	a.mu.Unlock()
	for _, l := range cand {
		if used+need <= a.cfg.BudgetMiB {
			break
		}
		if busy[l.Name] || a.resident(l.Name) == 0 {
			continue
		}
		if err := a.lem.Unload(ctx, l.Name); err != nil {
			log.Printf("unload %s: %v", l.Name, err)
			continue
		}
		a.m.unload("budget")
		log.Printf("budget: unloaded %s for %s", l.Name, model)
		used -= a.resident(l.Name)
	}
	return nil
}

// Recover frees the GPU after a failed job and waits until VRAM is released.
func (a *Arbiter) Recover(ctx context.Context) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if err := a.lem.Unload(ctx, ""); err != nil {
		log.Printf("recover unload: %v", err)
	}
	a.m.unload("retry")
	if a.gpu == nil {
		return
	}
	deadline := a.now().Add(30 * time.Second)
	for a.now().Before(deadline) {
		if u, err := a.gpu.UsedMiB(ctx); err != nil || u <= a.cfg.LeakMiB {
			return
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// Tick runs the idle unload and the leak check once.
func (a *Arbiter) Tick(ctx context.Context) {
	loaded, err := a.lem.Loaded(ctx)
	if err != nil {
		return
	}
	a.opMu.Lock()
	for _, l := range loaded {
		a.mu.Lock()
		last, seen := a.lastUse[l.Name]
		if !seen {
			a.lastUse[l.Name] = a.now()
			last = a.now()
		}
		idle := a.cfg.IdleUnload.Duration
		if mc := a.cfg.Models[l.Name]; mc.IdleUnload != nil {
			idle = mc.IdleUnload.Duration
		}
		quiet := a.inflight[l.Name] == 0 && !l.Busy
		expired := a.now().Sub(last) > idle
		a.mu.Unlock()
		if quiet && expired && a.resident(l.Name) > 0 {
			if err := a.lem.Unload(ctx, l.Name); err == nil {
				a.m.unload("idle")
				log.Printf("idle: unloaded %s", l.Name)
			}
		}
	}
	a.opMu.Unlock()
	a.checkLeak(ctx, loaded)
}

func (a *Arbiter) checkLeak(ctx context.Context, loaded []Loaded) {
	if a.gpu == nil {
		return
	}
	u, err := a.gpu.UsedMiB(ctx)
	expected := 0
	for _, l := range loaded {
		expected += a.resident(l.Name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m.setVRAM(u, err == nil)
	if err != nil || a.total > 0 || u <= expected+a.cfg.LeakMiB {
		a.leakSince = time.Time{}
		return
	}
	if a.leakSince.IsZero() {
		a.leakSince = a.now()
		return
	}
	if a.now().Sub(a.leakSince) > a.cfg.LeakAfter.Duration {
		log.Printf("leak: %d MiB used with nothing in flight, killing backends", u)
		a.m.leak()
		a.leakSince = time.Time{}
		go a.kill()
	}
}

func (a *Arbiter) Drain(ctx context.Context) error {
	a.mu.Lock()
	a.draining = true
	a.mu.Unlock()
	deadline := a.now().Add(a.cfg.DrainWait.Duration)
	for {
		a.mu.Lock()
		n, ch := a.total, a.wake
		a.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(deadline)):
			return errWaitLimit
		}
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.m.unload("drain")
	return a.lem.Unload(ctx, "")
}

func (a *Arbiter) Draining() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.draining }

type ModelState struct {
	Model    string `json:"model"`
	Type     string `json:"type"`
	InFlight int    `json:"in_flight"`
	IdleS    int    `json:"idle_s"`
}

func (a *Arbiter) Snapshot(ctx context.Context) []ModelState {
	loaded, _ := a.lem.Loaded(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	var r []ModelState
	for _, l := range loaded {
		idle := 0
		if t, ok := a.lastUse[l.Name]; ok {
			idle = int(a.now().Sub(t).Seconds())
		}
		r = append(r, ModelState{l.Name, l.Type, a.inflight[l.Name], idle})
	}
	return r
}
