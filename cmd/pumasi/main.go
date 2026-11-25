// pumasi places self-hosted GitHub Actions runners across the machines you
// already have. One binary: `pumasi up` runs the controller on your laptop;
// every host runs `pumasi agent`, which dials that controller.
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

  pumasi up      make the hosts of the config join and serve jobs until Ctrl-C
                 (running jobs survive Ctrl-C; the next up picks them up)
  pumasi status  problems, hosts, runners and waiting jobs (--json for machines)
  pumasi open    the same in a browser, with recent jobs and the last 24 hours
  pumasi down    remove everything pumasi created on the hosts and on GitHub
                 (refuses while jobs run, unless --force)
  pumasi invite  print the line that joins a host to this controller
                 (one host within the hour; --uses N and --ttl 24h say otherwise)
  pumasi register  classes with slots: — register runners on the connected hosts with a
                 one-time registration token (in PUMASI_RUNNER_TOKEN or on stdin)

On a host that joined:
  pumasi doctor  check the way to the controller, what runners need here, and the agent
  pumasi adopt   classes with slots: — take over runners that are registered already
                 (pumasi adopt [-class NAME] <runner directory>...; nothing changes on GitHub)
  pumasi release give such a runner up again (its listener is stopped when no job is lost)
  pumasi leave   remove this host from the controller and pumasi from this host

Flags: -config pumasi.yaml (up, status, open, down, invite, register), -v (up: verbose log).
up and down read the GitHub token from PUMASI_GITHUB_TOKEN; it is not needed when
every class has slots: (registered runners listen with their own credentials).
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
	case "invite":
		err = runInvite(args)
	case "join":
		err = runJoin(args)
	case "doctor":
		err = runDoctor(args)
	case "leave":
		err = runLeave(args)
	case "register":
		err = runRegister(args)
	case "adopt":
		err = runAdopt(args)
	case "release":
		err = runRelease(args)
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
	if token == "" && cfg.NeedsToken() {
		return errors.New("PUMASI_GITHUB_TOKEN is empty; e.g. PUMASI_GITHUB_TOKEN=$(gh auth token) pumasi up (a class without slots: is a scale set, which needs the token; classes with slots: do not)")
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
	if n := niceness(); n > 0 && !down {
		// A shell lowers its background jobs (zsh: nice 5); the listener and the
		// hosts' keepalives then wait behind everything else on a busy machine.
		log.Warn(fmt.Sprintf("this controller runs at nice %d (started with & from a shell?): on a busy machine it falls behind; run it in the foreground or as a service (docs/design.md)", n))
	}
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
		// A second Ctrl-C skips the tidy shutdown.
		<-sig
		c.Close()
		fmt.Fprintln(os.Stderr, "pumasi: interrupted again, quitting now")
		os.Exit(130)
	}()
	if down {
		return c.Down(ctx, *force)
	}
	dist := filepath.Dir(exe)
	if m, _ := filepath.Glob(filepath.Join(dist, "pumasi-*-*")); len(m) == 0 {
		return fmt.Errorf("no host builds next to %s: hosts fetch pumasi-<os>-<arch> from there when they join; run `make` in the pumasi source tree and use ./dist/pumasi", exe)
	}
	return c.Run(ctx, dist)
}
