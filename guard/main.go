// lemonade-guard: reverse proxy in front of Lemonade. Allowlist, VRAM-budget admission,
// idle unload, self-healing retry, async jobs, start-up sync. See specs/008 contracts/guard-api.md.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfgPath := flag.String("config", "/etc/guard/guard.json", "config file")
	flag.Parse()
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	adminKey := os.Getenv("LEMONADE_ADMIN_API_KEY")
	if adminKey == "" {
		adminKey = os.Getenv("LEMONADE_API_KEY")
	}
	m := NewMetrics()
	lem := newHTTPLemonade(cfg.Upstream, adminKey)
	var gpu VRAM = nvidiaSMI{}
	arb := NewArbiter(cfg, lem, gpu, m)
	arb.kill = killBackends
	g := NewGuard(cfg, arb, lem, m, adminKey)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	sy := &Syncer{cfg: cfg, lem: lem, arb: arb, m: m}
	g.syncer = sy
	go sy.Run(ctx, 15*time.Second)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				arb.Tick(ctx)
			}
		}
	}()
	srv := &http.Server{Addr: cfg.Listen, Handler: g}
	go func() {
		<-ctx.Done()
		dctx, c := context.WithTimeout(context.Background(), cfg.DrainWait.Duration)
		defer c()
		_ = arb.Drain(dctx)
		_ = srv.Shutdown(dctx)
	}()
	log.Printf("guard listening on %s, upstream %s", cfg.Listen, cfg.Upstream)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// killBackends SIGKILLs Lemonade's backend processes (needs shareProcessNamespace); Lemonade
// notices and starts a fresh one on the next request.
func killBackends() {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid := e.Name()
		b, err := os.ReadFile("/proc/" + pid + "/comm")
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(b))
		if name == "ace-server" || name == "llama-server" {
			p, err := strconv.Atoi(pid)
			if err != nil {
				continue
			}
			_ = syscall.Kill(p, syscall.SIGKILL)
			log.Printf("killed %s (pid %d)", name, p)
		}
	}
}
