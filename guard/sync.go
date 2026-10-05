package main

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Syncer brings Lemonade to the configured state: backends installed, models pulled.
type Syncer struct {
	cfg *Config
	lem Lemonade
	arb *Arbiter
	m   *Metrics

	Missing struct{ Backends, Models []string }

	mu   sync.Mutex
	done bool // at least one full pass found nothing missing
}

// Complete reports whether a pass has finished with everything present (before the first pass the
// missing lists are empty but unknown).
func (s *Syncer) Complete() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.done }

// Once runs one pass and reports whether everything is present.
func (s *Syncer) Once(ctx context.Context) bool {
	var mb, mm []string
	for _, b := range s.cfg.Backends {
		recipe, backend, _ := strings.Cut(b, ":")
		ok, err := s.lem.BackendInstalled(ctx, recipe, backend)
		if err != nil {
			mb = append(mb, b)
			continue
		}
		if !ok {
			log.Printf("sync: install backend %s", b)
			if err := s.lem.Install(ctx, recipe, backend); err != nil {
				log.Printf("sync: install %s: %v", b, err)
				mb = append(mb, b)
			}
		}
	}
	for _, f := range s.cfg.RequiredFiles {
		if _, err := os.Stat(f); err != nil {
			log.Printf("sync: required file missing: %s", f)
			mb = append(mb, "file:"+f)
		}
	}
	have, err := s.lem.Downloaded(ctx)
	for _, m := range s.cfg.SyncModels {
		if err == nil && have[m] {
			continue
		}
		if err != nil {
			mm = append(mm, m)
			continue
		}
		log.Printf("sync: pull %s", m)
		if err := s.lem.Pull(ctx, m); err != nil {
			log.Printf("sync: pull %s: %v", m, err)
			mm = append(mm, m)
		}
	}
	s.Missing.Backends, s.Missing.Models = mb, mm
	s.m.setMissing(len(mb) + len(mm))
	ok := len(mb)+len(mm) == 0
	s.mu.Lock()
	s.done = ok
	s.mu.Unlock()
	return ok
}

// Run retries until complete, then warms the default model.
func (s *Syncer) Run(ctx context.Context, retry time.Duration) {
	for !s.lem.Ready(ctx) || !s.Once(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
	log.Printf("sync: complete")
	if s.cfg.WarmModel == "" {
		return
	}
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.arb.Acquire(actx, classLLM, s.cfg.WarmModel)
	if err != nil {
		return // someone else is using the GPU; they load what they need
	}
	defer release()
	if err := s.lem.Load(ctx, s.cfg.WarmModel); err != nil {
		log.Printf("warm %s: %v", s.cfg.WarmModel, err)
	}
}
