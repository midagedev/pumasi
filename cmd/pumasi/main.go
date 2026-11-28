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
	"strings"
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
                 (--demo: a simulated shop to look at, no controller needed)
  pumasi down    remove everything pumasi created on the hosts and on GitHub
                 (refuses while jobs run, unless --force)
  pumasi invite  print the line that joins a host to this controller
                 (one host within the hour; --uses N and --ttl 24h say otherwise)
  pumasi service install|remove|status
                 keep up running: started again when it dies and after a reboot
  pumasi trend   how long each job takes day by day, and where it changed:
                 the commit, the pull request and the step that grew (--json; --demo)
  pumasi why-slow <run URL | pull request URL | --pr N | commit>
                 did it make CI slower, and where: each job and step against main's usual
  pumasi forecast  how fast CI grows, and when its busiest hours fill the cores
                 and the wait for room doubles at that rate (--cores N; --json; --demo)
  pumasi priority  the mode, expedited jobs, rules and where they put the jobs waiting now
  pumasi priority add workflow='deploy-*.yml' --level high [--for 2h]
  pumasi priority rm r-1
                 rules for now, before the config's (the config's: edit the file;
                 a running up reads it again by itself)
  pumasi expedite <job>
                 put a waiting job before every other one (--clear takes it out);
                 <job> is its URL, GitHub's job id or pumasi's (status --json)
  pumasi mode high-only --for 30m | pumasi mode normal
                 for a while, only expedited and high jobs get new runners

expedite, mode and priority add|rm ask the running up (its dashboard address).

On a host that joined:
  pumasi doctor  check the way to the controller, what runners need here, and the agent
  pumasi leave   remove this host from the controller and pumasi from this host

Flags: -config pumasi.yaml (up, status, open, down, invite, priority, expedite, mode), -v (up: verbose log),
open --demo [-seed N] [-listen 127.0.0.1:0] [-browser=false].
up and down are a GitHub App when the config has githubApp: {id, installationId,
privateKeyFile} (or PUMASI_GITHUB_APP_ID, _INSTALLATION_ID, _PRIVATE_KEY_FILE);
otherwise they read a token from PUMASI_GITHUB_TOKEN, or from the file
"pumasi service install" keeps (<state dir>/github-token, mode 600).
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
	case "priority":
		err = runPriority(args)
	case "trend":
		err = runTrend(args)
	case "why-slow":
		err = runWhySlow(args)
	case "forecast":
		err = runForecast(args)
	case "expedite":
		err = runExpedite(args)
	case "mode":
		err = runMode(args)
	case "service":
		err = runService(args)
	case "join":
		err = runJoin(args)
	case "doctor":
		err = runDoctor(args)
	case "leave":
		err = runLeave(args)
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

func runController(args []string, down bool) (err error) {
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
	auth, err := githubAuth(cfg)
	if err != nil {
		return err
	}
	os.Unsetenv("PUMASI_GITHUB_TOKEN") // what this process starts (ssh, notify commands) does not inherit it
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return err
	}
	unlock, err := controller.Lock(cfg.StateDir())
	if err != nil {
		return fmt.Errorf("%w (`pumasi service status` tells whether it is the service; `pumasi service remove` stops that)", err)
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
	c, err := controller.New(cfg, auth, l, ev, log)
	if err != nil {
		return err
	}
	defer c.Close()
	c.LedgerMoved = moved
	if n := niceness(); n > 0 && !down {
		// A shell lowers its background jobs (zsh: nice 5); the listener and the
		// hosts' keepalives then wait behind everything else on a busy machine.
		log.Warn(fmt.Sprintf("this controller runs at nice %d (started with & from a shell?): on a busy machine it falls behind; run it in the foreground or with `pumasi service install`", n))
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
	if auth.App != nil {
		// The library's own words for a refused App are a chain of five causes.
		defer func() {
			if err != nil && strings.Contains(err.Error(), "GitHub App auth") {
				err = fmt.Errorf("%w\nGitHub did not take %s (%s): check the id, the installation id (…/settings/installations/<id>) and that the key file is this App's", err, auth.What(), auth.From)
			}
		}()
	}
	if down {
		return c.Down(ctx, *force)
	}
	dist := filepath.Dir(exe)
	if m, _ := filepath.Glob(filepath.Join(dist, "pumasi-*-*")); len(m) == 0 {
		return fmt.Errorf("no host builds next to %s: hosts fetch pumasi-<os>-<arch> from there when they join; run `make` in the pumasi source tree and use ./dist/pumasi", exe)
	}
	return c.Run(ctx, dist)
}
