// pumasi places self-hosted GitHub Actions runners across the machines you
// already have. One binary: `pumasi up` runs the controller on your laptop and
// starts `pumasi agent` on every host over ssh.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/ledger"
)

const usage = `pumasi places self-hosted GitHub Actions runners across your machines.

  pumasi up      start agents on the hosts and serve jobs until Ctrl-C
                 (running jobs survive Ctrl-C; the next up picks them up)
  pumasi status  problems, hosts, runners and waiting jobs (--json for machines)
  pumasi open    the same in a browser, with recent jobs and the last 24 hours
  pumasi down    remove everything pumasi created on the hosts and on GitHub
                 (refuses while jobs run, unless --force)

Flags: -config pumasi.yaml (all commands), -v (up: verbose log).
up and down read the GitHub token from PUMASI_GITHUB_TOKEN.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "up":
		err = runController(args, false)
	case "down":
		err = runController(args, true)
	case "status":
		err = runStatus(args)
	case "open":
		err = runOpen(args)
	case "agent":
		err = runAgent(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "pumasi: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "pumasi:", err)
		os.Exit(1)
	}
}

func runController(args []string, down bool) error {
	fs := flag.NewFlagSet("pumasi", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	verbose := fs.Bool("v", false, "verbose log")
	force := fs.Bool("force", false, "down: also stop runners that are running jobs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	token := os.Getenv("PUMASI_GITHUB_TOKEN")
	if token == "" {
		return errors.New("PUMASI_GITHUB_TOKEN is empty; e.g. PUMASI_GITHUB_TOKEN=$(gh auth token) pumasi up")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return err
	}
	unlock, err := controller.Lock(cfg.StateDir())
	if err != nil {
		return err
	}
	defer unlock()
	l, moved, err := ledger.OpenOrRebuild(cfg.Ledger, time.Now())
	if err != nil {
		return err
	}
	defer l.Close()
	ev, err := controller.OpenEventLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer ev.Close()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(newLineHandler(os.Stdout, level))
	c, err := controller.New(cfg, token, l, ev, log)
	if err != nil {
		return err
	}
	defer c.Close()
	c.LedgerMoved = moved
	if !*verbose {
		// The listener logs every long poll at info level.
		c.ListenerLog = slog.New(newLineHandler(os.Stdout, slog.LevelWarn))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
		// A second Ctrl-C skips the tidy shutdown but still takes the ssh tunnels along.
		<-sig
		c.Close()
		fmt.Fprintln(os.Stderr, "pumasi: interrupted again, quitting now")
		os.Exit(130)
	}()
	if down {
		return c.Down(ctx, *force)
	}
	dist := filepath.Dir(exe)
	if m, _ := filepath.Glob(filepath.Join(dist, "pumasi-linux-*")); len(m) == 0 {
		return fmt.Errorf("no linux build next to %s: the hosts need pumasi-linux-amd64 or pumasi-linux-arm64 there; run `make` in the pumasi source tree and use ./dist/pumasi", exe)
	}
	return c.Run(ctx, dist)
}
