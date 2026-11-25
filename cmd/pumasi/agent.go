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
	"strconv"
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/cgroup"
	"github.com/midagedev/pumasi/internal/config"
)

// runAgent is the host side. `pumasi up` starts it over ssh; it listens on
// loopback only and the controller reaches it through an ssh tunnel.
func runAgent(args []string) error {
	hostname, _ := os.Hostname()
	fs := flag.NewFlagSet("pumasi agent", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:7391", "listen address (keep it on loopback)")
	host := fs.String("host", hostname, "host name reported to the controller")
	tag := fs.String("tag", "pumasi", "only containers labeled pumasi=<tag> are managed")
	budgetCPUs := fs.Float64("budget-cpus", 0, "CPUs pumasi may lend on this host (0 = no agent-side limit)")
	budgetMem := fs.String("budget-mem", "", "memory pumasi may lend, e.g. 6G (empty = no agent-side limit)")
	interval := fs.Duration("interval", 5*time.Second, "cgroup sampling interval")
	pidfile := fs.String("pidfile", "", "write the agent's pid here")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mem, err := config.ParseBytes(*budgetMem)
	if err != nil {
		return fmt.Errorf("-budget-mem: %w", err)
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
	if *pidfile != "" {
		if err := os.WriteFile(*pidfile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			return err
		}
	}
	log.Info("pumasi agent listening", "addr", *listen, "host", *host, "tag", *tag, "budgetCpus", *budgetCPUs, "budgetMem", mem)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
