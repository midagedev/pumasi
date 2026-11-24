// pumasi-m0 is the M0 spike: one scale set, one docker host reached over ssh.
// It records every listener message as JSON lines so the open questions in
// docs/design.md can be answered from evidence.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

type config struct {
	url        string
	name       string
	labels     string
	sshHost    string
	image      string
	maxRunners int
	holdFile   string
	logPath    string
	keep       bool
}

func main() {
	var c config
	flag.StringVar(&c.url, "url", "", "registration URL (https://github.com/owner/repo)")
	flag.StringVar(&c.name, "name", "pumasi-m0", "scale set name")
	flag.StringVar(&c.labels, "labels", "", "comma separated labels (default: name)")
	flag.StringVar(&c.sshHost, "ssh-host", "", "docker host reached as `ssh <host> docker ...`")
	flag.StringVar(&c.image, "image", "ghcr.io/actions/actions-runner:2.338.2", "runner image")
	flag.IntVar(&c.maxRunners, "max-runners", 2, "max runners")
	flag.StringVar(&c.holdFile, "hold-file", "", "while this file exists, do not start runners")
	flag.StringVar(&c.logPath, "log", "m0-events.jsonl", "event log (JSON lines)")
	flag.BoolVar(&c.keep, "keep-scale-set", false, "do not delete the scale set on exit")
	flag.Parse()
	if c.url == "" || c.sshHost == "" {
		fmt.Fprintln(os.Stderr, "-url and -ssh-host are required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, c); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type eventLog struct {
	mu sync.Mutex
	f  *os.File
}

func (l *eventLog) write(kind string, v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind, "data": v})
	l.f.Write(append(b, '\n'))
}

func run(ctx context.Context, c config) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	ev := &eventLog{f: f}

	token := os.Getenv("PUMASI_GITHUB_TOKEN")
	if token == "" {
		return errors.New("PUMASI_GITHUB_TOKEN is empty")
	}
	info := scaleset.SystemInfo{System: "pumasi", Subsystem: "m0", CommitSHA: "NA", Version: "0.0.0"}
	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: c.url, PersonalAccessToken: token, SystemInfo: info,
	})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}

	labelNames := []string{c.name}
	if c.labels != "" {
		labelNames = strings.Split(c.labels, ",")
	}
	labels := make([]scaleset.Label, 0, len(labelNames))
	for _, n := range labelNames {
		labels = append(labels, scaleset.Label{Name: strings.TrimSpace(n)})
	}

	set, err := client.GetRunnerScaleSet(ctx, 1, c.name)
	if err != nil || set == nil {
		set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: c.name, RunnerGroupID: 1, Labels: labels,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
		ev.write("scale-set-created", set)
	} else {
		ev.write("scale-set-reused", set)
	}
	info.ScaleSetID = set.ID
	client.SetSystemInfo(info)
	logger.Info("scale set", "id", set.ID, "name", set.Name, "labels", set.Labels)
	if !c.keep {
		defer func() {
			if err := client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), set.ID); err != nil {
				logger.Error("delete scale set", "err", err)
			}
			ev.write("scale-set-deleted", set.ID)
		}()
	}

	hostname, _ := os.Hostname()
	session, err := client.MessageSessionClient(ctx, set.ID, "pumasi-m0-"+hostname)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer session.Close(context.Background())

	l, err := listener.New(session, listener.Config{ScaleSetID: set.ID, MaxRunners: c.maxRunners, Logger: logger.WithGroup("listener")})
	if err != nil {
		return err
	}
	s := &scaler{cfg: c, client: client, session: session, setID: set.ID, log: logger, ev: ev, runners: map[string]*runner{}}
	defer s.shutdown(context.WithoutCancel(ctx))
	go s.watch(ctx)
	logger.Info("listening")
	return l.Run(ctx, s)
}

type runner struct {
	name    string
	state   string // starting | idle | busy | done
	created time.Time
}

type scaler struct {
	cfg     config
	client  *scaleset.Client
	session listener.Client
	setID   int
	log     *slog.Logger
	ev      *eventLog

	mu      sync.Mutex
	runners map[string]*runner
}

func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		return nil
	}
	s.ev.write("message", msg)

	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		acquired, err := s.session.AcquireJobs(ctx, ids)
		s.ev.write("acquire", map[string]any{"requested": ids, "acquired": acquired, "err": errString(err)})
		if err != nil {
			return fmt.Errorf("acquire: %w", err)
		}
	}
	for _, j := range msg.JobStartedMessages {
		s.mark(j.RunnerName, "busy")
	}
	for _, j := range msg.JobCompletedMessages {
		s.mark(j.RunnerName, "done")
		s.removeContainer(ctx, j.RunnerName)
	}
	if msg.Statistics == nil {
		return nil
	}
	return s.reconcile(ctx, msg.Statistics.TotalAssignedJobs)
}

func (s *scaler) reconcile(ctx context.Context, assigned int) error {
	s.mu.Lock()
	live := 0
	for _, r := range s.runners {
		if r.state != "done" {
			live++
		}
	}
	s.mu.Unlock()
	target := min(s.cfg.maxRunners, assigned)
	if target <= live {
		return nil
	}
	if s.cfg.holdFile != "" {
		if _, err := os.Stat(s.cfg.holdFile); err == nil {
			s.ev.write("hold", map[string]any{"assigned": assigned, "live": live})
			s.log.Info("hold: not starting runners", "assigned", assigned, "live", live)
			return nil
		}
	}
	for range target - live {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *scaler) start(ctx context.Context) error {
	name := "pumasi-m0-" + uuid.NewString()[:8]
	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.setID)
	if err != nil {
		return fmt.Errorf("jit: %w", err)
	}
	s.mu.Lock()
	s.runners[name] = &runner{name: name, state: "starting", created: time.Now()}
	s.mu.Unlock()

	// The JIT config goes over stdin so it never lands on the remote disk or in argv.
	cmd := s.remote(ctx, "docker", "run", "-d", "--name", name, "--label", "pumasi=m0",
		"--cpus", "2", "--memory", "2g", "--env-file", "/dev/stdin",
		s.cfg.image, "/home/runner/run.sh")
	cmd.Stdin = strings.NewReader("ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit.EncodedJITConfig + "\n")
	out, err := cmd.CombinedOutput()
	s.ev.write("runner-start", map[string]any{"name": name, "out": strings.TrimSpace(string(out)), "err": errString(err)})
	if err != nil {
		s.mark(name, "done")
		return fmt.Errorf("docker run %s: %w: %s", name, err, out)
	}
	s.mark(name, "idle")
	s.log.Info("runner started", "name", name)
	return nil
}

func (s *scaler) mark(name, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runners[name]
	if !ok {
		// Messages about runners we did not start are recorded, not fatal (upstream example panics here).
		s.ev.write("unknown-runner", map[string]any{"name": name, "state": state})
		return
	}
	r.state = state
}

func (s *scaler) removeContainer(ctx context.Context, name string) {
	out, err := s.remote(ctx, "docker", "rm", "-f", name).CombinedOutput()
	s.ev.write("runner-remove", map[string]any{"name": name, "out": strings.TrimSpace(string(out)), "err": errString(err)})
}

// watch records container state every 30s so a killed container is visible next to GitHub's messages.
func (s *scaler) watch(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			out, err := s.remote(ctx, "docker", "ps", "-a",
				"--filter", "label=pumasi=m0", "--format", "{{.Names}} {{.Status}}").CombinedOutput()
			s.ev.write("containers", map[string]any{"ps": strings.TrimSpace(string(out)), "err": errString(err)})
		}
	}
}

func (s *scaler) shutdown(ctx context.Context) {
	s.mu.Lock()
	names := make([]string, 0, len(s.runners))
	for n, r := range s.runners {
		if r.state != "done" {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	for _, n := range names {
		s.removeContainer(ctx, n)
	}
}

// remote runs a command on the docker host. ssh joins argv into one shell string,
// so every argument is single-quoted here.
func (s *scaler) remote(ctx context.Context, args ...string) *exec.Cmd {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", s.cfg.sshHost, strings.Join(quoted, " "))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ listener.Scaler = (*scaler)(nil)
