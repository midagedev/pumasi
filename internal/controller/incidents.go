package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

const (
	remindEvery = 30 * time.Minute
	// Opening and resolving within flapWindow more than flapLimit times in a row
	// is reported once as flapping, then kept quiet until it holds still.
	flapWindow = 5 * time.Minute
	flapLimit  = 3
)

// notice is what a webhook or command receives. text alone is enough for a
// Slack-style incoming webhook.
type notice struct {
	Text    string `json:"text"`
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	State   string `json:"state"` // problem | reminder | flapping | resolved | recovered | info
	Detail  string `json:"detail"`
}

// incidentBook opens and resolves incidents in the ledger and queues their
// notifications. Sending happens elsewhere (deliver) and never holds this up.
type incidentBook struct {
	c  *Controller
	mu sync.Mutex
	// When each open incident was last announced, and its recent short cycles.
	announced map[string]time.Time
	flaps     map[string]int
	quiet     map[string]bool // flapping: say nothing until it lasts longer than flapWindow
	wake      chan struct{}
}

func newIncidentBook(c *Controller) *incidentBook {
	b := &incidentBook{c: c, announced: map[string]time.Time{}, flaps: map[string]int{}, quiet: map[string]bool{}, wake: make(chan struct{}, 1)}
	// Incidents left open by an earlier run: remind after the usual interval, not at once.
	if open, err := c.ledger.OpenIncidents(); err == nil {
		for _, in := range open {
			b.announced[in.Key] = c.now()
		}
	}
	return b
}

// open records a lasting problem. Calling it again with the same key only
// refreshes the detail line.
func (b *incidentBook) open(key, kind, subject, detail string) {
	now := b.c.now()
	in := ledger.Incident{Key: key, Kind: kind, Subject: subject, OpenedAt: now, Detail: detail}
	opened, err := b.c.ledger.OpenIncident(in, false)
	if err != nil {
		b.c.log.Error("ledger incident", "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !opened {
		if last := b.announced[key]; !b.quiet[key] && now.Sub(last) >= remindEvery {
			b.announced[key] = now
			b.queue(in, "reminder", "still: "+detail)
		}
		return
	}
	b.c.log.Warn("problem: " + detail)
	b.announced[key] = now
	if b.quiet[key] {
		return
	}
	b.queue(in, "problem", detail)
}

// resolve closes the incident with this key, if one is open.
func (b *incidentBook) resolve(key string) {
	now := b.c.now()
	in, ok, err := b.c.ledger.ResolveIncident(key, now)
	if err != nil {
		b.c.log.Error("ledger incident", "err", err)
	}
	if !ok {
		return
	}
	lasted := now.Sub(in.OpenedAt)
	text := fmt.Sprintf("%s: %s resolved after %s", in.Subject, strings.ReplaceAll(in.Kind, "-", " "), lasted.Round(time.Second))
	b.c.log.Info("resolved: " + text)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.announced, key)
	if lasted >= flapWindow {
		b.flaps[key], b.quiet[key] = 0, false
		b.queue(in, "resolved", text)
		return
	}
	b.flaps[key]++
	switch {
	case b.quiet[key]:
	case b.flaps[key] > flapLimit:
		b.quiet[key] = true
		b.queue(in, "flapping", fmt.Sprintf("%s: %s keeps coming and going (%d times); quiet until it holds for %s",
			in.Subject, strings.ReplaceAll(in.Kind, "-", " "), b.flaps[key], flapWindow))
	default:
		b.queue(in, "resolved", text)
	}
}

// report records something that happened and is already over (a runner died,
// the controller came back). state is "problem", "recovered" or "info".
func (b *incidentBook) report(state, key, kind, subject, detail, action string) {
	in := ledger.Incident{Key: key, Kind: kind, Subject: subject, OpenedAt: b.c.now(), Detail: detail, Action: action}
	if _, err := b.c.ledger.OpenIncident(in, true); err != nil {
		b.c.log.Error("ledger incident", "err", err)
	}
	if state == "problem" {
		b.c.log.Warn("problem: " + detail)
	} else {
		b.c.log.Info(detail)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queue(in, state, detail)
}

// queue stores a notification if the configured level wants it. Callers hold b.mu.
func (b *incidentBook) queue(in ledger.Incident, state, text string) {
	n := b.c.cfg.Notify
	if n.Webhook == "" && n.Command == "" {
		return
	}
	// problem: problems, their reminders and their end. recovered: also what
	// pumasi fixed by itself. all: also plain information.
	switch state {
	case "recovered":
		if n.Level != "recovered" && n.Level != "all" {
			return
		}
	case "info":
		if n.Level != "all" {
			return
		}
	}
	p, _ := json.Marshal(notice{Text: "[pumasi] " + text, Key: in.Key, Kind: in.Kind, Subject: in.Subject, State: state, Detail: in.Detail})
	if err := b.c.ledger.Enqueue(string(p), b.c.now()); err != nil {
		b.c.log.Error("ledger outbox", "err", err)
		return
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// deliver sends queued notifications until ctx ends: at once when one is
// queued, and every minute for those that failed before.
func (b *incidentBook) deliver(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		b.flush(ctx)
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
		case <-t.C:
		}
	}
}

// flush tries every unsent notification once, oldest first, and stops at the
// first failure so the order holds.
func (b *incidentBook) flush(ctx context.Context) {
	out, err := b.c.ledger.Unsent()
	if err != nil {
		return
	}
	for _, o := range out {
		err := b.send(ctx, o.Payload)
		_ = b.c.ledger.Sent(o.ID, b.c.now(), err)
		if err != nil {
			b.c.log.Debug("notification not delivered, will retry", "err", err)
			return
		}
	}
}

func (b *incidentBook) send(ctx context.Context, payload string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	n := b.c.cfg.Notify
	if n.Webhook != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Webhook, strings.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("webhook: %s", resp.Status)
		}
	}
	if n.Command != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", n.Command)
		cmd.Stdin = strings.NewReader(payload + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("notify command: %w: %s", err, bytes.TrimSpace(out))
		}
	}
	return nil
}
