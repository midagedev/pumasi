package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/join"
)

// runAgent is the host side. `pumasi up` starts it over ssh; then it listens on
// loopback only and the controller reaches it through an ssh tunnel. With
// -join (a host that joined by itself) it listens nowhere and dials the controller.
// How runners run follows the host's OS: docker containers on Linux, processes
// on macOS and Windows.
func runAgent(args []string) error {
	hostname, _ := os.Hostname()
	fs := flag.NewFlagSet("pumasi agent", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:7391", "listen address (keep it on loopback)")
	host := fs.String("host", hostname, "host name reported to the controller")
	tag := fs.String("tag", "pumasi", "only containers labeled pumasi=<tag> are managed (Linux)")
	budgetCPUs := fs.Float64("budget-cpus", 0, "CPUs pumasi may lend on this host (0 = no agent-side limit)")
	budgetMem := fs.String("budget-mem", "", "memory pumasi may lend, e.g. 6G (empty = no agent-side limit)")
	interval := fs.Duration("interval", 5*time.Second, "sampling interval")
	pidfile := fs.String("pidfile", "", "write the agent's pid here")
	joined := fs.Bool("join", false, "dial the controller this host joined (see `pumasi join`) instead of listening")
	supervise := fs.Bool("supervise", false, "with -join: stay in the background and start the agent again whenever it ends (how Windows runs it at logon)")
	state := fs.String("state", filepath.Join(cacheDir(), "pumasi", "runners.json"),
		"runner records kept across agent restarts (macOS, Windows: the runners themselves live beside it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mem, err := config.ParseBytes(*budgetMem)
	if err != nil {
		return fmt.Errorf("-budget-mem: %w", err)
	}
	if *supervise {
		return superviseAgent(paths())
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	exe, _ := os.Executable()
	version := join.FileVersion(exe)
	cfg := agent.Config{Host: *host, BudgetCPUs: *budgetCPUs, BudgetMem: mem, Interval: *interval, StateFile: *state, Version: shortVersion(version)}
	var hc join.HostConfig
	if *joined {
		if hc, err = join.LoadHostConfig(paths().conf); err != nil {
			return fmt.Errorf("this host has not joined a controller: %w", err)
		}
		cfg.Host, cfg.Kind, cfg.BudgetCPUs, cfg.BudgetMem = hc.Name, hc.Kind, hc.CPUs, hc.Mem
	}
	cfg.BootID = agent.BootID()
	drv, meter := agent.Platform(*tag, filepath.Dir(*state))
	a := agent.New(cfg, drv, meter, log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go a.Run(ctx)
	if *joined {
		return serveJoined(ctx, hc, a.Handler(), exe, version, log)
	}

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

func shortVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// serveJoined keeps this host connected to its controller and serves the
// agent API over that connection. It returns nil only after a leave: any other
// end makes the service start the agent again.
func serveJoined(ctx context.Context, hc join.HostConfig, agentAPI http.Handler, exe, version string, log *slog.Logger) error {
	p := paths()
	left := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("/", agentAPI)
	// The controller's `down`: remove everything pumasi put here, then end.
	mux.HandleFunc("POST /v1/leave", func(w http.ResponseWriter, r *http.Request) {
		n, err := removeRunners(r.Context(), p)
		out := map[string]any{"containers": n}
		if err != nil {
			out["problem"] = err.Error()
		} else {
			stopService(p, false)
			removeFiles(p)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		if err == nil {
			log.Info("the controller told this host to leave; everything is removed")
			close(left)
		}
	})
	watch := &join.Watchdog{Log: log}
	go watch.Run(ctx)
	h := &join.Host{Config: hc, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Handler: mux, Log: log, Watch: watch,
		State: func(connected bool, why string) {
			b, _ := json.Marshal(linkState{Connected: connected, At: time.Now(), Why: why})
			_ = os.WriteFile(p.state, b, 0o600)
		},
		Upgrade: func(ctx context.Context, want string) (string, error) {
			return version, switchBuild(ctx, hc, exe, want)
		}}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-left:
			time.Sleep(300 * time.Millisecond) // let the answer reach the controller
			cancel()
		case <-hctx.Done():
		}
	}()
	h.Run(hctx)
	select {
	case <-left:
		return nil
	default:
		return errors.New("agent stopped")
	}
}

// switchBuild replaces this binary with the controller's build and runs it in
// place of this process. Runners are not children the agent takes with it and
// their records are on disk, so they do not notice. It returns only on failure.
func switchBuild(ctx context.Context, hc join.HostConfig, exe, want string) error {
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	err = hc.Download(ctx, runtime.GOOS, runtime.GOARCH, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && join.FileVersion(tmp) != want {
		err = errors.New("the download does not match the build the controller named")
	}
	if err == nil {
		err = replaceSelf(exe, tmp)
	}
	_ = os.Remove(tmp)
	return err
}
