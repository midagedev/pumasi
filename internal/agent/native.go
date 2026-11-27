//go:build darwin || linux || windows

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

// Native runs each runner as a process tree of this OS, as the user the agent
// runs as. It is what macOS and Windows hosts use: their jobs come because
// they need that OS itself. Isolation is a fresh runner directory, nothing more.
//
//	<Root>/dist/actions-runner-<os>-<arch>-<version>/   the runner, downloaded once
//	<Runners>/<name>/runner/                            a clone of it, one per runner
//	<Runners>/<name>/{pumasi.json,log,exit}             what pumasi keeps about it
//
// The runner must outlive an agent restart, so nothing about it lives only in
// memory: List rebuilds its view from the directories and the process table.
type Native struct {
	Root    string
	Runners string // "" = <Root>/runners

	mu        sync.Mutex
	trees     map[string]*tree
	refreshed time.Time // when the trees were last read from the process table
	dists     map[string]*distFetch
	distOnce  sync.Once
	distAt    string // where unpacked runners are kept (distRoot)
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
	// Dirs: the work and slot directories it writes to outside its own (Spec.diskDirs).
	Dirs []string `json:"dirs,omitempty"`
}

var runnerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (n *Native) runners() string {
	if n.Runners != "" {
		return n.Runners
	}
	return filepath.Join(n.Root, "runners")
}

func (n *Native) dir(name string) string { return filepath.Join(n.runners(), name) }

// RunnerRoot is where runners get their directories. Not under the user's
// home: its path would be in every job's log, and on Windows a deep root eats
// into the 260 characters a path may have. Where that place cannot be had (or
// is not this user's alone) it is cache/runners.
func RunnerRoot(cache string) string {
	if p := runnerRoot(); p != "" && ownDir(p) == nil {
		return p
	}
	return filepath.Join(cache, "runners")
}

// distRoot is where the unpacked runner is kept: beside the cache, unless a
// file there cannot be linked into a runner's directory (the cache on another
// filesystem). Every runner would then be a copy of hundreds of megabytes,
// one after the other on a busy disk; so it is kept with the runners instead.
func (n *Native) distRoot() string {
	n.distOnce.Do(func() {
		n.distAt = filepath.Join(n.Root, "dist")
		if !linkable(n.Root, n.runners()) {
			n.distAt = filepath.Join(n.runners(), distDir)
		}
	})
	return n.distAt
}

// distDir is the unpacked runners' place in the runners' directory. It has no
// pumasi.json, so it is no runner.
const distDir = ".dist"

// linkable reports whether a file in one directory can be hard-linked into
// the other. Where that cannot be tried, it says yes: nothing is moved.
func linkable(from, to string) bool {
	if os.MkdirAll(from, 0o700) != nil || os.MkdirAll(to, 0o700) != nil {
		return true
	}
	f, err := os.CreateTemp(from, ".link-*")
	if err != nil {
		return true
	}
	f.Close()
	defer os.Remove(f.Name())
	probe := filepath.Join(to, filepath.Base(f.Name()))
	if os.Link(f.Name(), probe) != nil {
		return false
	}
	os.Remove(probe)
	return true
}

// trashDir holds what could not be deleted of removed runners.
const trashDir = ".trash"

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
	err = cloneTree(dist, filepath.Join(dir, "runner"))
	if errors.Is(err, errDistChanged) {
		// Not run as it is: fetched anew. Runners already up keep their links.
		_ = os.RemoveAll(filepath.Join(dir, "runner"))
		_ = removeTree(dist)
		if dist, err = n.dist(ctx, s.Image); err == nil {
			err = cloneTree(dist, filepath.Join(dir, "runner"))
		}
	}
	if err != nil {
		return fail(fmt.Errorf("clone runner: %w", err))
	}
	jit, err := setWork(dir, s)
	if err != nil {
		return fail(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer log.Close()
	// The slot's directories are made when they are not there and never
	// removed: the operator names the place, the host decides how many
	// runners there are at once.
	for _, p := range s.SlotDirs() {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fail(fmt.Errorf("slot %d of class %s: %w (the class names it with {slot}; make it writable for this user)", s.Slot, s.Class, err))
		}
	}
	// The JIT config goes through the environment so it never lands on disk or in argv.
	env := append(classEnv(runnerEnv(os.Environ()), s, dir), "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit)
	t, err := startTree(dir, s, env, log)
	if err != nil {
		return fail(fmt.Errorf("start runner: %w", err))
	}
	m := meta{Name: s.Name, Class: s.Class, CPUs: s.CPUs, MemBytes: s.MemBytes, Pid: t.pid, Born: t.born, StartedAt: time.Now(), Dirs: s.diskDirs()}
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
	entries, err := os.ReadDir(n.runners())
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
			State: "running", Pid: m.Pid, StartedAt: m.StartedAt, Dirs: m.Dirs}
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

// Remove stops whatever is left of the runner's process tree and deletes its
// directory. What the agent's user may not delete (files a job wrote as root
// through a container) is moved out of the runners' way and told as Leftover.
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
		if err = removeTree(dir); err == nil {
			return nil
		}
		if runtime.GOOS != "windows" && errors.Is(err, fs.ErrPermission) {
			break // waiting does not change whose file it is
		}
		time.Sleep(250 * time.Millisecond)
	}
	trash := filepath.Join(n.runners(), trashDir, name+"-"+strconv.FormatInt(time.Now().Unix(), 36))
	if os.MkdirAll(filepath.Dir(trash), 0o700) == nil && os.Rename(dir, trash) == nil {
		return &Leftover{Path: trash, Err: err}
	}
	return fmt.Errorf("remove %s: %w", dir, err)
}

// setWork puts the runner's work directory in the class's work directory of
// its slot, which stays when the runner goes: the next runner of the slot
// finds the checkout and node_modules the last one left, as a fixed runner's
// next job does. The runner is told the real path (WorkFolder in the JIT
// config's .runner), so the paths it hands the job — GITHUB_WORKSPACE,
// RUNNER_TEMP — are where the files are: a docker VM that mounts only some
// host directories (colima mounts $HOME) sees what the job bind-mounts.
// If the JIT config is not what it expects, the runner's _work becomes a
// link instead. Returns the JIT config to start the runner with.
func setWork(dir string, s Spec) (string, error) {
	if s.WorkDir == "" {
		return s.JITConfig, nil
	}
	w := s.expand(s.WorkDir, "")
	if s.Slot == 0 || strings.Contains(w, "{slot}") || !filepath.IsAbs(w) {
		return "", fmt.Errorf("work %q of class %s: no slot number to put in it", s.WorkDir, s.Class)
	}
	if err := os.MkdirAll(w, 0o755); err != nil {
		return "", fmt.Errorf("work of class %s, slot %d: %w (make it writable for this user)", s.Class, s.Slot, err)
	}
	if real, err := filepath.EvalSymlinks(w); err == nil {
		w = real
	}
	if jit, err := jitWithWork(s.JITConfig, w); err == nil {
		return jit, nil
	}
	if err := os.Symlink(w, filepath.Join(dir, "runner", "_work")); err != nil {
		return "", fmt.Errorf("work of class %s, slot %d: link the runner's _work to %s: %w", s.Class, s.Slot, w, err)
	}
	return s.JITConfig, nil
}

// jitWithWork is the JIT config with the runner's WorkFolder set to work. The
// config is base64 of a JSON object of file name to base64 content; the
// runner writes those files and reads .runner, whose WorkFolder is "_work".
func jitWithWork(jit, work string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(jit)
	if err != nil {
		return "", err
	}
	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		return "", err
	}
	enc, ok := files[".runner"]
	if !ok {
		return "", errors.New("no .runner in the JIT config")
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	var runner map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &runner); err != nil {
		return "", err
	}
	key := ""
	for k := range runner {
		if strings.EqualFold(k, "WorkFolder") {
			key = k
		}
	}
	if key == "" {
		return "", errors.New("no WorkFolder in .runner")
	}
	runner[key] = work
	if b, err = json.Marshal(runner); err != nil {
		return "", err
	}
	files[".runner"] = base64.StdEncoding.EncodeToString(b)
	if raw, err = json.Marshal(files); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// removeTree is os.RemoveAll that also gets through directories the job left
// without write permission (a Go module cache).
func removeTree(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
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

// Host measures the machine. On the way the runners step back while someone
// is at it (macOS: they run in the background band until the person is away).
func (n *Native) Host() (HostUsage, error) {
	h, err := hostUsage()
	n.mu.Lock()
	yield(n.trees, h.InputIdle != nil && *h.InputIdle < api.PersonAway)
	n.mu.Unlock()
	return h, err
}

// NativeProblem is why this host cannot run runners as processes (classes
// with driver: native). Empty = it can. macOS and Windows always can; Linux
// needs a systemd user session that may limit cpu and memory.
func NativeProblem() string { return nativeProblem() }

func (n *Native) DataDir(context.Context) string { return n.Root }

// DiskDirs: the agent's directory and the runners' (often another filesystem:
// RunnerRoot). The host's free disk is the fullest of these and the runners' Dirs.
func (n *Native) DiskDirs() []string { return []string{n.Root, n.runners()} }

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

// errDistChanged: a file of the unpacked runner that every runner shares is
// no longer read-only, so something has been at it since it was unpacked.
var errDistChanged = errors.New("the unpacked runner was changed")

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
	dir := filepath.Join(n.distRoot(), name)
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
	if err := sealDist(tmp); err != nil {
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

// runnerLibs is what the runner (a .NET program) loads at start and what its
// own bin/installdependencies.sh installs: without any one of them it exits
// before it listens. Each is known by the file name the loader looks for.
var runnerLibs = []struct{ name, file string }{
	{"libicu", "libicuuc.so."},
	{"libssl", "libssl.so."},
	{"libkrb5", "libgssapi_krb5.so."},
	{"zlib", "libz.so."},
	{"libstdc++", "libstdc++.so."},
}

func missingLibs(cache string) []string {
	var miss []string
	for _, l := range runnerLibs {
		if !strings.Contains(cache, l.file) {
			miss = append(miss, l.name)
		}
	}
	return miss
}
