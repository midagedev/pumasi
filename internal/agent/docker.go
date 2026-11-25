package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spec is one runner to start.
type Spec struct {
	Name      string
	Class     string
	CPUs      float64
	MemBytes  int64
	Image     string
	JITConfig string
	// Driver: docker | native. Empty = the host's own way (Linux: docker).
	Driver string
	// Mounts are host paths a container runner sees at the same path
	// ("/srv/cache", "/srv/keys:ro"). Process runners see the host anyway.
	Mounts []string
	// WorkDir: a directory inside one of Mounts for the runner's work
	// directory, made before the runner starts and removed with it.
	WorkDir string
	// InnerDocker gives a container runner a docker daemon of its own.
	InnerDocker bool
	// Env is added to the runner's environment; Path goes in front of a native
	// runner's PATH. "{runner}" in a value is the runner's own directory.
	Env  map[string]string
	Path []string
}

// The runner's own directory in the runner image.
const containerHome = "/home/runner"

// classEnv is env with the class's Path in front of PATH and its Env set,
// "{runner}" replaced by dir. Names compare the way the OS compares them.
func classEnv(env []string, s Spec, dir string) []string {
	if len(s.Env) == 0 && len(s.Path) == 0 {
		return env
	}
	same := func(a, b string) bool { return a == b }
	if runtime.GOOS == "windows" {
		same = strings.EqualFold
	}
	out := slices.Clone(env)
	set := func(k, v string) {
		out = slices.DeleteFunc(out, func(kv string) bool {
			name, _, _ := strings.Cut(kv, "=")
			return same(name, k)
		})
		out = append(out, k+"="+v)
	}
	if len(s.Path) > 0 {
		key, old := "PATH", ""
		for _, kv := range out {
			if name, v, _ := strings.Cut(kv, "="); same(name, "PATH") {
				key, old = name, v
			}
		}
		parts := make([]string, 0, len(s.Path)+1)
		for _, p := range s.Path {
			parts = append(parts, strings.ReplaceAll(p, "{runner}", dir))
		}
		if old != "" {
			parts = append(parts, old)
		}
		set(key, strings.Join(parts, string(os.PathListSeparator)))
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		set(k, strings.ReplaceAll(s.Env[k], "{runner}", dir))
	}
	return out
}

// Container is a runner container as the driver sees it.
type Container struct {
	ID         string
	Name       string
	Class      string
	CPUs       float64
	MemBytes   int64
	State      string // created | running | exited (anything else is reported as is)
	Pid        int
	ExitCode   int
	OOMKilled  bool // docker saw the container's main process killed at the memory limit
	StartedAt  time.Time
	FinishedAt time.Time
}

// Driver starts and removes runner containers. M1 has one: docker.
type Driver interface {
	Start(ctx context.Context, s Spec) error
	Remove(ctx context.Context, name string) error
	List(ctx context.Context) ([]Container, error)
	// LogEvents reports when the runner printed "Listening for Jobs" and "Running job",
	// looking only at output after since (the runner image traces verbosely to stdout).
	LogEvents(ctx context.Context, name string, since time.Time) (LogEvents, error)
	// LogTail is the end of the runner's output, kept with the job once the container is gone.
	LogTail(ctx context.Context, name string, lines int) string
	// DataDir is where the driver keeps images and containers ("" = unknown).
	DataDir(ctx context.Context) string
	// Check reports why the host cannot start a runner from image right now (nil = it can).
	Check(ctx context.Context, image string) error
}

// Leftover is Remove's error when the runner is gone but a directory of it
// could not be deleted and stays on the host at Path.
type Leftover struct {
	Path string
	Err  error
}

func (l *Leftover) Error() string { return fmt.Sprintf("%s could not be deleted: %v", l.Path, l.Err) }

// Docker drives the docker CLI. Only containers carrying label pumasi=<Tag> are touched.
type Docker struct {
	Tag string
}

const (
	labelTag   = "pumasi"
	labelClass = "pumasi.class"
	labelCPUs  = "pumasi.cpus"
	labelMem   = "pumasi.mem"
	labelWork  = "pumasi.workdir"
)

func (d Docker) Start(ctx context.Context, s Spec) error {
	cpus := strconv.FormatFloat(s.CPUs, 'f', -1, 64)
	mem := strconv.FormatInt(s.MemBytes, 10)
	args := []string{"run", "-d", "--name", s.Name,
		"--label", labelTag + "=" + d.Tag, "--label", labelClass + "=" + s.Class,
		"--label", labelCPUs + "=" + cpus, "--label", labelMem + "=" + mem,
		"--cpus", cpus, "--memory", mem, "--memory-swap", mem,
		// The JIT config goes over stdin so it never lands on disk or in argv.
		"--env-file", "/dev/stdin"}
	for _, m := range s.Mounts {
		// --mount, not -v: docker must refuse a path the host does not have, not create it as root.
		path, ro := strings.CutSuffix(m, ":ro")
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, ",\n") {
			return fmt.Errorf("bad mount %q", m)
		}
		arg := "type=bind,source=" + path + ",target=" + path
		if ro {
			arg += ",readonly"
		}
		args = append(args, "--mount", arg)
	}
	if s.WorkDir != "" {
		ok := filepath.Base(s.WorkDir) == s.Name && filepath.Clean(s.WorkDir) == s.WorkDir
		inMount := false
		for _, m := range s.Mounts {
			if p, ro := strings.CutSuffix(m, ":ro"); !ro && strings.HasPrefix(s.WorkDir, p+"/") {
				inMount = true
			}
		}
		if !ok || !inMount {
			return fmt.Errorf("bad work directory %q", s.WorkDir)
		}
		// The runner inside the container is another user than the agent.
		if err := os.MkdirAll(s.WorkDir, 0o777); err != nil {
			return fmt.Errorf("work directory: %w", err)
		}
		if err := os.Chmod(s.WorkDir, 0o777); err != nil {
			return fmt.Errorf("work directory: %w", err)
		}
		args = append(args, "--label", labelWork+"="+s.WorkDir)
	}
	image, run := s.Image, []string{"/home/runner/run.sh"}
	if s.InnerDocker {
		var err error
		if image, err = innerImage(ctx, s.Image); err != nil {
			return err
		}
		// dockerd needs a privileged container, and its data on a volume: the
		// container's own filesystem is an overlay it cannot stack another on.
		args = append(args, "--privileged", "--volume", "/var/lib/docker")
		run = []string{"sh", "-c", innerDockerScript}
	}
	cmd := exec.CommandContext(ctx, "docker", append(append(args, image), run...)...)
	env := classEnv([]string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + s.JITConfig}, Spec{Env: s.Env}, containerHome)
	cmd.Stdin = strings.NewReader(strings.Join(env, "\n") + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if s.WorkDir != "" {
			_ = os.Remove(s.WorkDir)
		}
		return fmt.Errorf("docker run: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// innerDockerScript starts a docker daemon inside the runner container, then
// becomes the runner. The daemon and everything it runs stay in the
// container's cgroup, so the runner's limits and numbers cover them.
// cgroup v2 lets a cgroup have either processes or children with controllers:
// the container's processes move to a leaf first, so the daemon can give its
// containers cgroups of their own.
const innerDockerScript = `sudo -n sh -c '
cd /sys/fs/cgroup && mkdir -p ` + innerLeaf + ` &&
for p in $(cat cgroup.procs); do echo $p > ` + innerLeaf + `/cgroup.procs 2>/dev/null; done
sed -e "s/ / +/g" -e "s/^/+/" cgroup.controllers > cgroup.subtree_control
exec dockerd >/var/log/dockerd.log 2>&1' &
i=0; until docker info >/dev/null 2>&1 || [ $i -ge 300 ]; do i=$((i+1)); sleep 0.1; done
docker info >/dev/null 2>&1 || { echo "pumasi: the docker daemon did not start in this runner:"; tail -n 5 /var/log/dockerd.log; }
exec /home/runner/run.sh`

// innerLeaf is the cgroup, inside the container's, that the runner's own
// processes sit in when the container has a docker daemon. The container is
// measured at its own cgroup, one above.
const innerLeaf = "pumasi-runner"

const innerImageRepo = "pumasi-inner-docker"

var innerImageMu sync.Mutex

// innerImage is image with what dockerd needs besides itself. The stock
// runner image carries dockerd but no iptables, without which the daemon does
// not start. Built once per host and base image.
func innerImage(ctx context.Context, image string) (string, error) {
	innerImageMu.Lock()
	defer innerImageMu.Unlock()
	// A start that gives up must not kill the build the next start needs.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}} {{.Config.User}}", image).Output()
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w", image, err)
	}
	id, user, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	tag := innerImageRepo + ":" + strings.TrimPrefix(id, "sha256:")[:12]
	if exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag).Run() == nil {
		return tag, nil
	}
	file := "FROM " + image + "\nUSER root\n" +
		"RUN command -v dockerd >/dev/null || { echo 'the image has no dockerd' >&2; exit 1; }\n" +
		"RUN command -v iptables >/dev/null || { apt-get update -qq && apt-get install -y -qq --no-install-recommends iptables && rm -rf /var/lib/apt/lists/*; }\n"
	if user != "" {
		file += "USER " + user + "\n"
	}
	build := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, "-")
	build.Stdin = strings.NewReader(file)
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("image for docker: inside: %s", lastLine(out))
	}
	return tag, nil
}

// Clean removes the images pumasi built on this host (docker: inside). The
// runner image itself was pulled, not made, and stays in docker's cache.
func (d Docker) Clean(ctx context.Context) {
	out, _ := exec.CommandContext(ctx, "docker", "images", "-q", "--filter", "reference="+innerImageRepo).Output()
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_ = exec.CommandContext(ctx, "docker", append([]string{"rmi"}, ids...)...).Run()
	}
}

// removeWorkDir deletes a runner's work directory on the host. What the job
// wrote belongs to the container's user (or to root, when the job ran
// containers of its own), so where the agent's user may not delete it a
// container does, as root.
func removeWorkDir(ctx context.Context, dir, image string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	parent, name := filepath.Split(dir)
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-u", "0", "--mount", "type=bind,source="+filepath.Clean(parent)+",target=/w",
		"--entrypoint", "rm", image, "-rf", "/w/"+name).CombinedOutput()
	if err != nil {
		return &Leftover{Path: dir, Err: errors.New(lastLine(out))}
	}
	return nil
}

func (d Docker) Remove(ctx context.Context, name string) error {
	c, err := d.inspect(ctx, []string{name})
	if err != nil {
		return err
	}
	if len(c) == 0 {
		return nil
	}
	if c[0].labels[labelTag] != d.Tag {
		return fmt.Errorf("container %s is not labeled %s=%s", name, labelTag, d.Tag)
	}
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %w: %s", err, bytes.TrimSpace(out))
	}
	if dir := c[0].labels[labelWork]; dir != "" && filepath.Base(dir) == name {
		return removeWorkDir(ctx, dir, c[0].Config.Image)
	}
	return nil
}

func (d Docker) List(ctx context.Context) ([]Container, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "-q", "--no-trunc",
		"--filter", "label="+labelTag+"="+d.Tag).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	ins, err := d.inspect(ctx, ids)
	if err != nil {
		return nil, err
	}
	cs := make([]Container, 0, len(ins))
	for _, i := range ins {
		cs = append(cs, i.container())
	}
	return cs, nil
}

func (d Docker) DataDir(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.DockerRootDir}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (d Docker) Check(ctx context.Context, image string) error {
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fmt.Errorf("docker does not answer: %s", lastLine(out))
	}
	if exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Run() == nil {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "docker", "pull", "-q", image).CombinedOutput(); err != nil {
		return fmt.Errorf("cannot pull %s: %s", image, lastLine(out))
	}
	return nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// LogEvents are times read from a runner's output; zero when not seen.
type LogEvents struct {
	Listening, JobStarted time.Time
}

func (d Docker) LogTail(ctx context.Context, name string, lines int) string {
	out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(lines), name).CombinedOutput()
	if len(out) > 8<<10 {
		out = out[len(out)-8<<10:]
	}
	return string(out)
}

func (d Docker) LogEvents(ctx context.Context, name string, since time.Time) (LogEvents, error) {
	args := []string{"logs", "-t"}
	if !since.IsZero() {
		args = append(args, "--since", since.UTC().Format(time.RFC3339Nano))
	}
	out, err := exec.CommandContext(ctx, "docker", append(args, name)...).CombinedOutput()
	if err != nil {
		return LogEvents{}, fmt.Errorf("docker logs: %w", err)
	}
	return parseLogEvents(string(out))
}

// parseLogEvents finds the first "Listening for Jobs" and "Running job" lines in
// `docker logs -t` output and returns docker's timestamps for them. The runner
// prints "Running job" well before GitHub's JobStarted message arrives.
//
// A marker line may lack its timestamp: docker writes the container's stdout
// and stderr to two pipes, and read as one they can run into each other
// mid-line. The event still counts — a runner whose job is never seen looks
// idle and gets removed — with the time of the nearest stamped line before it.
func parseLogEvents(logs string) (LogEvents, error) {
	var ev LogEvents
	var last time.Time
	for _, line := range strings.Split(logs, "\n") {
		ts, _, _ := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err == nil {
			last = t
		}
		var dst *time.Time
		switch {
		case ev.Listening.IsZero() && strings.Contains(line, "Listening for Jobs"):
			dst = &ev.Listening
		case ev.JobStarted.IsZero() && strings.Contains(line, "Running job"):
			dst = &ev.JobStarted
		default:
			continue
		}
		if *dst = last; last.IsZero() {
			*dst = time.Now()
		}
	}
	return ev, nil
}

type inspected struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
	labels map[string]string
}

func (d Docker) inspect(ctx context.Context, ids []string) ([]inspected, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"inspect"}, ids...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// docker inspect exits 1 when any id is missing but still prints the rest.
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		if strings.Contains(stderr.String(), "No such") {
			return nil, nil
		}
		return nil, fmt.Errorf("docker inspect: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return parseInspect(out)
}

func parseInspect(b []byte) ([]inspected, error) {
	var ins []inspected
	if err := json.Unmarshal(b, &ins); err != nil {
		return nil, fmt.Errorf("docker inspect json: %w", err)
	}
	for i := range ins {
		ins[i].labels = ins[i].Config.Labels
	}
	return ins, nil
}

func (i inspected) container() Container {
	c := Container{
		ID:        i.ID,
		Name:      strings.TrimPrefix(i.Name, "/"),
		Class:     i.labels[labelClass],
		State:     i.State.Status,
		Pid:       i.State.Pid,
		ExitCode:  i.State.ExitCode,
		OOMKilled: i.State.OOMKilled,
	}
	c.CPUs, _ = strconv.ParseFloat(i.labels[labelCPUs], 64)
	c.MemBytes, _ = strconv.ParseInt(i.labels[labelMem], 10, 64)
	c.StartedAt = dockerTime(i.State.StartedAt)
	c.FinishedAt = dockerTime(i.State.FinishedAt)
	return c
}

// dockerTime parses docker's timestamps; the zero value "0001-01-01T00:00:00Z" stays zero.
func dockerTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() < 2000 {
		return time.Time{}
	}
	return t
}
