// pumasi-controller listens on one scale set per class, keeps the runner ledger
// and places ephemeral runners on hosts through their agents.
//
// The GitHub token is read from PUMASI_GITHUB_TOKEN only and never written anywhere.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/ledger"
)

func main() {
	cfgPath := flag.String("config", "pumasi.yaml", "config file")
	teardown := flag.Bool("teardown", false, "remove all runner containers, registrations and the classes' scale sets, then exit")
	flag.Parse()
	if err := run(*cfgPath, *teardown); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cfgPath string, teardown bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	token := os.Getenv("PUMASI_GITHUB_TOKEN")
	if token == "" {
		return errors.New("PUMASI_GITHUB_TOKEN is empty")
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	l, err := ledger.Open(cfg.Ledger)
	if err != nil {
		return err
	}
	defer l.Close()
	ev, err := controller.OpenEventLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer ev.Close()

	c, err := controller.New(cfg, token, l, ev, log)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if teardown {
		return c.Teardown(ctx)
	}
	log.Info("pumasi-controller starting", "url", cfg.URL, "classes", len(cfg.Classes), "hosts", len(cfg.Hosts))
	return c.Run(ctx)
}
