package main

import (
	"context"
	"log"
	"strings"
	"time"
)

// Syncer brings Lemonade to the configured state: backends installed, models pulled.
type Syncer struct {
	cfg *Config
	lem Lemonade
	arb *Arbiter
	m   *Metrics

	Missing struct{ Backends, Models []string }
}

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
	return len(mb)+len(mm) == 0
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
