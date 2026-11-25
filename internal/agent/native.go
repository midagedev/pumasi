//go:build darwin || linux || windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Native runs each runner as a process tree of this OS, as the user the agent
// runs as. It is what macOS and Windows hosts use: their jobs come because
// they need that OS itself. Isolation is a fresh runner directory, nothing more.
//
//	<Root>/dist/actions-runner-<os>-<arch>-<version>/   the runner, downloaded once
//	<Root>/runners/<name>/runner/                       a clone of it, one per runner
//	<Root>/runners/<name>/{pumasi.json,log,exit}        what pumasi keeps about it
//
// The runner must outlive an agent restart, so nothing about it lives only in
// memory: List rebuilds its view from the directories and the process table.
type Native struct {
	Root string

	mu        sync.Mutex
	trees     map[string]*tree
	refreshed time.Time // when the trees were last read from the process table
	dists     map[string]*distFetch
}

// meta is pumasi.json.
type meta struct {
	Name       string    `json:"name"`
	Class      string    `json:"class"`
	CPUs       float64   `json:"cpus"`
	MemBytes   int64     `json:"memBytes"`
	Pid        int       `json:"pid"`
	Born       int64     `json:"born"` // the process's start time: tells it from a reused pid
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	ExitCode   *int      `json:"exitCode,omitempty"`
}

var runnerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (n *Native) dir(name string) string { return filepath.Join(n.Root, "runners", name) }

func (n *Native) Start(ctx context.Context, s Spec) error {
	if !runnerNameRE.MatchString(s.Name) {
		return fmt.Errorf("bad runner name %q", s.Name)
	}
	dist, err := n.dist(ctx, s.Image)
	if err != nil {
		return err
	}
	dir := n.dir(s.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(dir)
		return err
	}
	if err := cloneTree(dist, filepath.Join(dir, "runner")); err != nil {
		return fail(fmt.Errorf("clone runner: %w", err))
	}
	log, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer log.Close()
	// The JIT config goes through the environment so it never lands on disk or in argv.
	env := append(runnerEnv(os.Environ()), "ACTIONS_RUNNER_INPUT_JITCONFIG="+s.JITConfig)
	t, err := startTree(dir, s, env, log)
	if err != nil {
		return fail(fmt.Errorf("start runner: %w", err))
	}
	m := meta{Name: s.Name, Class: s.Class, CPUs: s.CPUs, MemBytes: s.MemBytes, Pid: t.pid, Born: t.born, StartedAt: time.Now()}
	if err := writeMeta(dir, m); err != nil {
		t.kill()
		return fail(err)
	}
	n.mu.Lock()
	if n.trees == nil {
		n.trees = map[string]*tree{}
	}
	n.trees[s.Name] = t
	n.mu.Unlock()
	return nil
}

func (n *Native) List(context.Context) ([]Container, error) {
	entries, err := os.ReadDir(filepath.Join(n.Root, "runners"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.trees == nil {
		n.trees = map[string]*tree{}
	}
	var metas []meta
	for _, e := range entries {
		dir := n.dir(e.Name())
		m, err := readMeta(dir)
		if err != nil {
			continue // being created, or not ours
		}
		if n.trees[m.Name] == nil {
			n.trees[m.Name] = adoptTree(dir, m) // left by an earlier agent
		}
		metas = append(metas, m)
	}
	refreshTrees(n.trees)
	n.refreshed = time.Now()
	cs := make([]Container, 0, len(metas))
	for _, m := range metas {
		t := n.trees[m.Name]
		c := Container{ID: m.Name, Name: m.Name, Class: m.Class, CPUs: m.CPUs, MemBytes: m.MemBytes,
			State: "running", Pid: m.Pid, StartedAt: m.StartedAt}
		if !t.alive() {
			if m.FinishedAt.IsZero() {
				m.FinishedAt = time.Now()
				code := -1 // gone without leaving an exit code: killed
				if x, ok := t.exitCode(); ok {
					code = x
					if fi, err := os.Stat(filepath.Join(n.dir(m.Name), "exit")); err == nil {
						m.FinishedAt = fi.ModTime() // when it really ended, not when it was noticed
					}
				}
				m.ExitCode = &code
				_ = writeMeta(n.dir(m.Name), m)
			}
			c.State, c.FinishedAt, c.ExitCode = "exited", m.FinishedAt, *m.ExitCode
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// Remove stops whatever is left of the runner's process tree and deletes its directory.
func (n *Native) Remove(_ context.Context, name string) error {
	if !runnerNameRE.MatchString(name) {
		return fmt.Errorf("bad runner name %q", name)
	}
	dir := n.dir(name)
	n.mu.Lock()
	t := n.trees[name]
	if t == nil {
		if m, err := readMeta(dir); err == nil {
			t = adoptTree(dir, m)
		}
	}
	delete(n.trees, name)
	n.mu.Unlock()
	if t != nil {
		t.kill()
	}
	var err error
	for i := 0; i < 20; i++ { // a dying process may still hold or write files
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("remove %s: %w", dir, err)
}

// Runner is the Meter side. During a sample List has just refreshed every tree.
func (n *Native) Runner(c Container) (Usage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.trees[c.Name]
	if t == nil {
		return Usage{}, fmt.Errorf("no process tree for %s", c.Name)
	}
	if time.Since(n.refreshed) > time.Second {
		refreshTrees(n.trees)
		n.refreshed = time.Now()
	}
	return t.usage(), nil
}

func (n *Native) Host() (HostUsage, error) { return hostUsage() }

// NativeProblem is why this host cannot run runners as processes (classes
// with driver: native). Empty = it can. macOS and Windows always can; Linux
// needs a systemd user session that may limit cpu and memory.
func NativeProblem() string { return nativeProblem() }

func (n *Native) DataDir(context.Context) string { return n.Root }

func (n *Native) Check(ctx context.Context, image string) error {
	_, err := n.dist(ctx, image)
	return err
}

func (n *Native) LogEvents(_ context.Context, name string, _ time.Time) (LogEvents, error) {
	b, err := os.ReadFile(filepath.Join(n.dir(name), "log"))
	if err != nil {
		return LogEvents{}, err
	}
	return parseRunnerLog(string(b), time.Now()), nil
}

func (n *Native) LogTail(_ context.Context, name string, lines int) string {
	b, _ := os.ReadFile(filepath.Join(n.dir(name), "log"))
	if len(b) > 8<<10 {
		b = b[len(b)-8<<10:]
	}
	ls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n")
}

// parseRunnerLog finds the runner's own "Listening for Jobs" and "Running job"
// lines, which it prefixes with a UTC time ("2026-11-25 01:02:03Z: Listening
// for Jobs"). A line without a readable time counts as seen at now.
func parseRunnerLog(log string, now time.Time) LogEvents {
	var ev LogEvents
	for _, line := range strings.Split(log, "\n") {
		var dst *time.Time
		switch {
		case ev.Listening.IsZero() && strings.Contains(line, "Listening for Jobs"):
			dst = &ev.Listening
		case ev.JobStarted.IsZero() && strings.Contains(line, "Running job"):
			dst = &ev.JobStarted
		default:
			continue
		}
		*dst = now
		if ts, _, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
			if t, err := time.Parse("2006-01-02 15:04:05Z", ts); err == nil {
				*dst = t
			}
		}
	}
	return ev
}

// runnerEnv is the agent's environment without what looks like a credential:
// an agent started from a shell that holds a GitHub token must not hand it to jobs.
func runnerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if strings.HasPrefix(u, "PUMASI_") || strings.HasPrefix(u, "ACTIONS_RUNNER_INPUT_") || containsAny(u, "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "PRIVATE_KEY", "ACCESS_KEY") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func readMeta(dir string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(dir, "pumasi.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func writeMeta(dir string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "pumasi.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "pumasi.json"))
}

// --- the runner distribution ---

type distFetch struct {
	done chan struct{}
	err  error
}

var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// runnerVersion takes the runner version from the image name pumasi is
// configured with (ghcr.io/actions/actions-runner:2.338.2 -> 2.338.2), so
// Linux containers and native runners follow the same one setting.
func runnerVersion(image string) (string, error) {
	i := strings.LastIndexByte(image, ':')
	if i < 0 || !versionRE.MatchString(image[i+1:]) {
		return "", fmt.Errorf("image %q has no runner version tag (like :2.338.2); a %s host needs it to fetch the runner", image, hostOS())
	}
	return image[i+1:], nil
}

func distName(version string) string {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	return "actions-runner-" + distOS + "-" + arch + "-" + version
}

// dist returns the directory of the unpacked runner, downloading it the first
// time. The download is not tied to ctx: a caller that gives up leaves it
// running for the next one.
func (n *Native) dist(ctx context.Context, image string) (string, error) {
	version, err := runnerVersion(image)
	if err != nil {
		return "", err
	}
	name := distName(version)
	dir := filepath.Join(n.Root, "dist", name)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	n.mu.Lock()
	if n.dists == nil {
		n.dists = map[string]*distFetch{}
	}
	f := n.dists[name]
	if f == nil {
		f = &distFetch{done: make(chan struct{})}
		n.dists[name] = f
		go func() {
			f.err = fetchDist(version, name+distExt, dir)
			n.mu.Lock()
			delete(n.dists, name)
			n.mu.Unlock()
			close(f.done)
		}()
	}
	n.mu.Unlock()
	select {
	case <-f.done:
		if f.err != nil {
			return "", fmt.Errorf("fetch runner %s: %w", version, f.err)
		}
		return dir, nil
	case <-ctx.Done():
		return "", fmt.Errorf("runner %s is still downloading: %w", version, ctx.Err())
	}
}

// releaseAPI is where GitHub lists a runner release's files with their digests.
var releaseAPI = "https://api.github.com/repos/actions/runner/releases/tags/v"

// assetDigest asks GitHub for the sha256 of one file of a runner release.
// The runner is run as the host's user, so a file that cannot be checked is
// not run.
func assetDigest(version, file string) (string, error) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(releaseAPI + version)
	if err != nil {
		return "", fmt.Errorf("its sha256 could not be fetched: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		why := resp.Status
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			why += ": GitHub's hourly limit for requests without a token from this address is used up, try again later"
		}
		return "", fmt.Errorf("its sha256 could not be fetched from %s%s (%s)", releaseAPI, version, why)
	}
	var rel struct {
		Assets []struct{ Name, Digest string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("its sha256 could not be read: %w", err)
	}
	for _, a := range rel.Assets {
		if a.Name == file {
			if sum, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && len(sum) == 64 {
				return sum, nil
			}
			return "", fmt.Errorf("GitHub lists %s without a sha256 (digest %q)", file, a.Digest)
		}
	}
	return "", fmt.Errorf("release v%s has no file %s", version, file)
}

func fetchDist(version, file, dir string) error {
	want, err := assetDigest(version, file)
	if err != nil {
		return err
	}
	url := "https://github.com/actions/runner/releases/download/v" + version + "/" + file
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	arc, err := os.CreateTemp(filepath.Dir(dir), "download-*")
	if err != nil {
		return err
	}
	defer os.Remove(arc.Name())
	defer arc.Close()
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(arc, sum), resp.Body)
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("%s has sha256 %s, GitHub lists %s: not unpacked", url, got, want)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := unpack(arc, size, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// inside joins an archive member's name to dir, refusing names that leave it.
func inside(dir, name string) (string, error) {
	p := filepath.Join(dir, filepath.FromSlash(name))
	if rel, err := filepath.Rel(dir, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive member %q leaves the directory", name)
	}
	return p, nil
}
