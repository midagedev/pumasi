package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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

// Docker drives the docker CLI. Only containers carrying label pumasi=<Tag> are touched.
type Docker struct {
	Tag string
}

const (
	labelTag   = "pumasi"
	labelClass = "pumasi.class"
	labelCPUs  = "pumasi.cpus"
	labelMem   = "pumasi.mem"
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
	cmd := exec.CommandContext(ctx, "docker", append(args, s.Image, "/home/runner/run.sh")...)
	cmd.Stdin = strings.NewReader("ACTIONS_RUNNER_INPUT_JITCONFIG=" + s.JITConfig + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run: %w: %s", err, bytes.TrimSpace(out))
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
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %w: %s", err, bytes.TrimSpace(out))
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
