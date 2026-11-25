// pumasi-agent runs on each Linux host (WSL2 included). It listens on loopback
// only; the controller reaches it through an ssh tunnel.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/cgroup"
	"github.com/midagedev/pumasi/internal/config"
)

func main() {
	hostname, _ := os.Hostname()
	listen := flag.String("listen", "127.0.0.1:7391", "listen address (keep it on loopback)")
	host := flag.String("host", hostname, "host name reported to the controller")
	tag := flag.String("tag", "m1", "only containers labeled pumasi=<tag> are managed")
	budgetCPUs := flag.Float64("budget-cpus", 0, "CPUs pumasi may lend on this host (0 = no agent-side limit)")
	budgetMem := flag.String("budget-mem", "", "memory pumasi may lend, e.g. 6G (empty = no agent-side limit)")
	interval := flag.Duration("interval", 5*time.Second, "cgroup sampling interval")
	flag.Parse()

	mem, err := config.ParseBytes(*budgetMem)
	if err != nil {
		fmt.Fprintln(os.Stderr, "-budget-mem:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	a := agent.New(agent.Config{Host: *host, BudgetCPUs: *budgetCPUs, BudgetMem: mem, Interval: *interval},
		agent.Docker{Tag: *tag}, cgroup.DefaultReader(), log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go a.Run(ctx)

	srv := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	log.Info("pumasi-agent listening", "addr", *listen, "host", *host, "tag", *tag, "budgetCpus", *budgetCPUs, "budgetMem", mem)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
