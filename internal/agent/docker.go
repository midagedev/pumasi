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
	StartedAt  time.Time
	FinishedAt time.Time
}

// Driver starts and removes runner containers. M1 has one: docker.
type Driver interface {
	Start(ctx context.Context, s Spec) error
	Remove(ctx context.Context, name string) error
	List(ctx context.Context) ([]Container, error)
	// ListeningAt reports when the runner printed "Listening for Jobs".
	ListeningAt(ctx context.Context, name string) (time.Time, bool, error)
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
	cmd := exec.CommandContext(ctx, "docker", "run", "-d", "--name", s.Name,
		"--label", labelTag+"="+d.Tag, "--label", labelClass+"="+s.Class,
		"--label", labelCPUs+"="+cpus, "--label", labelMem+"="+mem,
		"--cpus", cpus, "--memory", mem, "--memory-swap", mem,
		// The JIT config goes over stdin so it never lands on disk or in argv.
		"--env-file", "/dev/stdin",
		s.Image, "/home/runner/run.sh")
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

func (d Docker) ListeningAt(ctx context.Context, name string) (time.Time, bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "logs", "-t", "--tail", "40", name).CombinedOutput()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("docker logs: %w", err)
	}
	return parseListening(string(out))
}

// parseListening finds the "Listening for Jobs" line in `docker logs -t` output
// and returns docker's timestamp for it.
func parseListening(logs string) (time.Time, bool, error) {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "Listening for Jobs") {
			continue
		}
		ts, _, _ := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("log timestamp %q: %w", ts, err)
		}
		return t, true, nil
	}
	return time.Time{}, false, nil
}

type inspected struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
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
		ID:       i.ID,
		Name:     strings.TrimPrefix(i.Name, "/"),
		Class:    i.labels[labelClass],
		State:    i.State.Status,
		Pid:      i.State.Pid,
		ExitCode: i.State.ExitCode,
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
