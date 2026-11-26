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
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/join"
)

// runAgent is the host side: the process `pumasi join` leaves running. It
// listens nowhere; it dials the controller the host joined and serves the
// agent API over that connection. How runners run follows the host's OS:
// docker containers on Linux, processes on macOS and Windows.
func runAgent(args []string) error {
	fs := flag.NewFlagSet("pumasi agent", flag.ContinueOnError)
	tag := fs.String("tag", "pumasi", "only containers labeled pumasi=<tag> are managed (Linux)")
	interval := fs.Duration("interval", 5*time.Second, "sampling interval")
	fs.Bool("join", true, "dial the controller this host joined (the only way; kept for the services of earlier builds)")
	supervise := fs.Bool("supervise", false, "stay in the background and start the agent again whenever it ends (how Windows runs it at logon)")
	state := fs.String("state", filepath.Join(cacheDir(), "pumasi", "runners.json"),
		"runner records kept across agent restarts (macOS, Windows: the runners themselves live beside it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *supervise {
		return superviseAgent(paths())
	}
	hc, err := join.LoadHostConfig(paths().conf)
	if err != nil {
		return fmt.Errorf("this host has not joined a controller (`pumasi invite` there prints the line to run here): %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	exe, _ := os.Executable()
	version := join.FileVersion(exe)
	cfg := agent.Config{Host: hc.Name, Kind: hc.Kind, BudgetCPUs: hc.CPUs, BudgetMem: hc.Mem, Interval: *interval, StateFile: *state,
		Version: shortVersion(version), BootID: agent.BootID()}
	drv, meter := agent.Platform(*tag, filepath.Dir(*state))
	a := agent.New(cfg, drv, meter, log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go a.Run(ctx)
	return serveJoined(ctx, hc, a.Handler(), exe, version, log)
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
