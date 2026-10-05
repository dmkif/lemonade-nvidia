package main

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

type Metrics struct {
	mu        sync.Mutex
	requests  map[string]int // "class|code"
	rejected  map[string]int
	unloads   map[string]int
	retries   int
	leaks     int
	waitSum   float64
	waitCount int
	vram      int
	vramOK    bool
	missing   int
	jobSum    map[string]float64
	jobCount  map[string]int
}

func NewMetrics() *Metrics {
	return &Metrics{requests: map[string]int{}, rejected: map[string]int{}, unloads: map[string]int{},
		jobSum: map[string]float64{}, jobCount: map[string]int{}}
}

func (m *Metrics) request(c string, code int) {
	m.mu.Lock()
	m.requests[fmt.Sprintf("%s|%d", c, code)]++
	m.mu.Unlock()
}
func (m *Metrics) reject(r string)        { m.mu.Lock(); m.rejected[r]++; m.mu.Unlock() }
func (m *Metrics) unload(r string)        { m.mu.Lock(); m.unloads[r]++; m.mu.Unlock() }
func (m *Metrics) retry()                 { m.mu.Lock(); m.retries++; m.mu.Unlock() }
func (m *Metrics) leak()                  { m.mu.Lock(); m.leaks++; m.mu.Unlock() }
func (m *Metrics) setMissing(n int)       { m.mu.Lock(); m.missing = n; m.mu.Unlock() }
func (m *Metrics) setVRAM(v int, ok bool) { m.mu.Lock(); m.vram, m.vramOK = v, ok; m.mu.Unlock() }
func (m *Metrics) observeWait(d time.Duration) {
	m.mu.Lock()
	m.waitSum += d.Seconds()
	m.waitCount++
	m.mu.Unlock()
}
func (m *Metrics) job(kind string, d time.Duration) {
	m.mu.Lock()
	m.jobSum[kind] += d.Seconds()
	m.jobCount[kind]++
	m.mu.Unlock()
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func (m *Metrics) Write(w io.Writer, loaded int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range sortedKeys(m.requests) {
		var c string
		var code int
		fmt.Sscanf(replacePipe(k), "%s %d", &c, &code)
		fmt.Fprintf(w, "guard_requests_total{class=%q,code=\"%d\"} %d\n", c, code, m.requests[k])
	}
	for _, k := range sortedKeys(m.rejected) {
		fmt.Fprintf(w, "guard_rejected_total{reason=%q} %d\n", k, m.rejected[k])
	}
	for _, k := range sortedKeys(m.unloads) {
		fmt.Fprintf(w, "guard_unloads_total{reason=%q} %d\n", k, m.unloads[k])
	}
	fmt.Fprintf(w, "guard_retries_total %d\nguard_vram_leak_total %d\n", m.retries, m.leaks)
	fmt.Fprintf(w, "guard_queue_wait_seconds_sum %g\nguard_queue_wait_seconds_count %d\n", m.waitSum, m.waitCount)
	if m.vramOK {
		fmt.Fprintf(w, "guard_vram_used_mib %d\n", m.vram)
	}
	fmt.Fprintf(w, "guard_loaded_models %d\nguard_sync_missing %d\n", loaded, m.missing)
	for _, k := range sortedKeys(m.jobSum) {
		fmt.Fprintf(w, "guard_job_seconds_sum{kind=%q} %g\nguard_job_seconds_count{kind=%q} %d\n", k, m.jobSum[k], k, m.jobCount[k])
	}
}

func replacePipe(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c == '|' {
			b[i] = ' '
		}
	}
	return string(b)
}
