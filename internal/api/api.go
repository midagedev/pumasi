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
	Host       string  `json:"host"`
	CPUs       float64 `json:"cpus"`
	MemBytes   int64   `json:"memBytes"`
	BudgetCPUs float64 `json:"budgetCpus"` // 0 = no agent-side budget
	BudgetMem  int64   `json:"budgetMem"`
	// avg10 of /proc/pressure "some", percent.
	PSI          map[string]float64 `json:"psi"`
	NonRunnerCPU float64            `json:"nonRunnerCpu"` // cores used by everything but runners
	NonRunnerMem int64              `json:"nonRunnerMem"` // MemTotal - MemAvailable - runner memory.current
	SampledAt    time.Time          `json:"sampledAt"`
	Runners      []Runner           `json:"runners"`
}

// Runner is one runner container as the agent last sampled it. Resource values
// survive the container's exit until the controller deletes it.
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
	IOBytes      int64              `json:"ioBytes"`
	PressureSec  map[string]float64 `json:"pressureSec,omitempty"` // some-stall seconds
	StartedAt    *time.Time         `json:"startedAt,omitempty"`
	ExitedAt     *time.Time         `json:"exitedAt,omitempty"`
	ExitCode     *int               `json:"exitCode,omitempty"`
	// Only in the answer to DELETE: the last lines the runner printed.
	LogTail string `json:"logTail,omitempty"`
}

// StartRequest is POST /v1/runners.
type StartRequest struct {
	Name      string  `json:"name"`
	Class     string  `json:"class"`
	CPUs      float64 `json:"cpus"`
	MemBytes  int64   `json:"memBytes"`
	Image     string  `json:"image"`
	JITConfig string  `json:"jitConfig"`
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

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	err := c.do(ctx, http.MethodGet, "/v1/snapshot", nil, &s)
	return s, err
}

func (c *Client) Start(ctx context.Context, r StartRequest) (Runner, error) {
	var out Runner
	err := c.do(ctx, http.MethodPost, "/v1/runners", r, &out)
	return out, err
}

// Delete stops and removes a runner container and returns its last sampled values.
func (c *Client) Delete(ctx context.Context, name string) (Runner, error) {
	var out Runner
	err := c.do(ctx, http.MethodDelete, "/v1/runners/"+url.PathEscape(name), nil, &out)
	return out, err
}
