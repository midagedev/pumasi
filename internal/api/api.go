// Package api is the JSON contract between pumasi-controller and pumasi-agent,
// plus a small HTTP client for it.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Snapshot is GET /v1/snapshot.
type Snapshot struct {
	Host string `json:"host"`
	// What the host says of itself: server | desktop ("" = the controller's config says),
	// and the build of its agent.
	Kind    string `json:"kind,omitempty"`
	Version string `json:"version,omitempty"`
	// linux | macos | windows. Empty (an older agent) means linux.
	OS string `json:"os,omitempty"`
	// The host's own reason to take no new runners right now ("memory pressure"). Empty = none.
	Closed string `json:"closed,omitempty"`
	// Seconds since the last keyboard or mouse input at the machine. Absent
	// where that cannot be known (no screen, an agent outside the user's session).
	InputIdleSec *int64 `json:"inputIdleSec,omitempty"`
	// Why the host cannot run runners as processes (classes with driver: native):
	// "no systemd user session for ci". Empty = it can.
	NoNative string `json:"noNative,omitempty"`
	// Of the paths the controller asked about (GET /v1/snapshot?path=...), those the host has.
	Paths      []string `json:"paths,omitempty"`
	CPUs       float64  `json:"cpus"`
	MemBytes   int64    `json:"memBytes"`
	BudgetCPUs float64  `json:"budgetCpus"` // 0 = no agent-side budget
	BudgetMem  int64    `json:"budgetMem"`
	// avg10 of /proc/pressure "some", percent. Linux only; absent elsewhere.
	PSI          map[string]float64 `json:"psi"`
	NonRunnerCPU float64            `json:"nonRunnerCpu"` // cores used by everything but runners
	NonRunnerMem int64              `json:"nonRunnerMem"` // MemTotal - MemAvailable - runner memory.current
	SampledAt    time.Time          `json:"sampledAt"`
	// Free and total bytes of the filesystem holding docker's data (0 = unknown).
	DiskFree  int64    `json:"diskFree"`
	DiskTotal int64    `json:"diskTotal"`
	Runners   []Runner `json:"runners"`
}

// Runner is one runner as the agent last sampled it. Resource values survive
// the runner's exit until the controller deletes it. Values the host's OS
// cannot measure are absent, not zero.
type Runner struct {
	Name     string  `json:"name"`
	Class    string  `json:"class"`
	CPUs     float64 `json:"cpus"`
	MemBytes int64   `json:"memBytes"`
	State    string  `json:"state"` // created | running | exited | gone
	// Why the runner cannot have finished its work: "host rebooted". Empty otherwise.
	Lost        string     `json:"lost,omitempty"`
	ListeningAt *time.Time `json:"listeningAt,omitempty"`
	// Agent clock: from POST /v1/runners received to "Listening for Jobs".
	ProvisionMs int64 `json:"provisionMs,omitempty"`
	// From the runner's "Running job" line: earlier than GitHub's JobStarted message.
	JobStartedAt *time.Time         `json:"jobStartedAt,omitempty"`
	CPUSec       float64            `json:"cpuSec"`
	MemPeak      int64              `json:"memPeak"`
	MemCurrent   int64              `json:"memCurrent"`
	IOBytes      *int64             `json:"ioBytes,omitempty"`
	OOMKills     int                `json:"oomKills,omitempty"`    // processes killed at the memory limit
	PressureSec  map[string]float64 `json:"pressureSec,omitempty"` // some-stall seconds
	StartedAt    *time.Time         `json:"startedAt,omitempty"`
	ExitedAt     *time.Time         `json:"exitedAt,omitempty"`
	ExitCode     *int               `json:"exitCode,omitempty"`
	// Only in the answer to DELETE: the last lines the runner printed.
	LogTail string `json:"logTail,omitempty"`
	// Only in the answer to DELETE: what of the runner the host could not
	// delete, and why ("/var/tmp/…/.trash/x could not be deleted: permission denied").
	Leftover string `json:"leftover,omitempty"`
}

// LostRebooted is Runner.Lost for a runner whose host rebooted under it.
const LostRebooted = "host rebooted"

// PersonAway: a machine whose last input is more recent than this has someone
// at it. A desktop then takes no new runners; running jobs are never touched.
const PersonAway = 15 * time.Minute

// StartRequest is POST /v1/runners.
type StartRequest struct {
	Name      string  `json:"name"`
	Class     string  `json:"class"`
	CPUs      float64 `json:"cpus"`
	MemBytes  int64   `json:"memBytes"`
	Image     string  `json:"image"`
	JITConfig string  `json:"jitConfig"`
	// docker | native. Empty = the host's own way (Linux: docker; macOS, Windows: native).
	Driver string `json:"driver,omitempty"`
	// Host paths a container runner gets at the same path; ":ro" = read only.
	Mounts []string `json:"mounts,omitempty"`
	// WorkDir: a host directory inside one of Mounts, named after the runner,
	// that the host makes for the runner's work directory and removes with it.
	WorkDir string `json:"workDir,omitempty"`
	// InnerDocker: a container runner with a docker daemon of its own.
	InnerDocker bool `json:"innerDocker,omitempty"`
}

// Client talks to one agent. BaseURL is resolved per call so a transport can
// move (an ssh tunnel restarting on a new local port).
type Client struct {
	BaseURL func(ctx context.Context) (string, error)
	HTTP    *http.Client
}

// StatusError carries a non-2xx agent response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("agent: %d %s", e.Code, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	base, err := c.BaseURL(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// Snapshot asks for the host's state. The host also says which of paths it has.
func (c *Client) Snapshot(ctx context.Context, paths ...string) (Snapshot, error) {
	var s Snapshot
	q := url.Values{"path": paths}
	err := c.do(ctx, http.MethodGet, "/v1/snapshot?"+q.Encode(), nil, &s)
	return s, err
}

func (c *Client) Start(ctx context.Context, r StartRequest) (Runner, error) {
	var out Runner
	err := c.do(ctx, http.MethodPost, "/v1/runners", r, &out)
	return out, err
}

// Check asks the agent whether the host can start runners (docker answers, the
// image is there or can be pulled). A nil error with a non-empty string is the
// host's own reason why not.
func (c *Client) Check(ctx context.Context, image string) (string, error) {
	var out struct {
		Problem string `json:"problem"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/check?image="+url.QueryEscape(image), nil, &out)
	return out.Problem, err
}

// Delete stops and removes a runner container and returns its last sampled values.
func (c *Client) Delete(ctx context.Context, name string) (Runner, error) {
	var out Runner
	err := c.do(ctx, http.MethodDelete, "/v1/runners/"+url.PathEscape(name), nil, &out)
	return out, err
}
