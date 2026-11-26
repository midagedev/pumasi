package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

// A controller that is gone cannot say so. The hosts notice: their connection
// to it stays down. The controller names two of them (api.Away) and those
// tell the webhook, once each, and once more when the controller is back.

// awayNotice is the controller's notification format, so one receiver reads both.
type awayNotice struct {
	Text    string `json:"text"`
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	State   string `json:"state"` // problem | resolved
	Detail  string `json:"detail"`
}

// awayState is kept on disk: an agent that restarts while the controller is
// away neither forgets since when nor says it twice.
type awayState struct {
	api.Away
	Since     time.Time `json:"since,omitzero"`      // not connected since (zero = connected)
	Why       string    `json:"why,omitempty"`       // why the connection last ended or failed
	AlertedTo string    `json:"alertedTo,omitempty"` // the webhook that heard "away" and is owed "back"
	AwaySince time.Time `json:"awaySince,omitzero"`  // since when it was away, as that webhook heard
}

type awayWatch struct {
	path       string // "" = keep nothing
	host, addr string // this host's name, the controller's address
	log        *slog.Logger
	now        func() time.Time
	post       func(ctx context.Context, url string, n awayNotice) error

	mu      sync.Mutex
	st      awayState
	stopped bool
}

func newAwayWatch(path, host, addr string, log *slog.Logger) *awayWatch {
	w := &awayWatch{path: path, host: host, addr: addr, log: log, now: time.Now, post: postNotice}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &w.st)
	}
	if w.st.Since.IsZero() {
		w.st.Since = w.now() // an agent starts without a connection
	}
	return w
}

func postNotice(ctx context.Context, url string, n awayNotice) error {
	b, _ := json.Marshal(n)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
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
	return nil
}

// save writes the state. Callers hold w.mu.
func (w *awayWatch) save() {
	if w.path == "" || w.stopped {
		return
	}
	b, _ := json.Marshal(w.st)
	tmp := w.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, w.path)
	}
}

// Stop ends the keeping of state: the host is leaving.
func (w *awayWatch) Stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

// Set takes what the controller says this host is to do.
func (w *awayWatch) Set(a api.Away) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st.Away = a
	w.save()
}

// Link follows the connection to the controller.
func (w *awayWatch) Link(connected bool, why string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case connected:
		w.st.Since, w.st.Why = time.Time{}, ""
	case w.st.Since.IsZero():
		w.st.Since, w.st.Why = w.now(), why
	default:
		w.st.Why = why // the latest reason is the one worth telling
		return
	}
	w.save()
}

// Run looks every ten seconds whether there is something to say.
func (w *awayWatch) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick says "away" once when the controller has been out of reach for long
// enough, and "back" once to whoever heard that. What cannot be delivered
// (this host's own network is down) is tried again on the next tick; an
// "away" that was never delivered is not sent after the controller is back.
func (w *awayWatch) tick(ctx context.Context) {
	w.mu.Lock()
	st, stopped := w.st, w.stopped
	w.mu.Unlock()
	if stopped {
		return
	}
	now := w.now()
	key := "controller-away/" + w.host
	if st.Since.IsZero() {
		if st.AlertedTo == "" {
			return
		}
		text := fmt.Sprintf("%s reaches the pumasi controller at %s again; it was out of reach for %s", w.host, w.addr, now.Sub(st.AwaySince).Round(time.Second))
		if err := w.post(ctx, st.AlertedTo, awayNotice{Text: "[pumasi] " + text, Key: key, Kind: "controller-away", Subject: w.host, State: "resolved", Detail: text}); err != nil {
			w.log.Warn("the controller is back, but the webhook could not be told; trying again", "err", err)
			return
		}
		w.log.Info("told the webhook that the controller is back")
		w.mu.Lock()
		w.st.AlertedTo, w.st.AwaySince = "", time.Time{}
		w.save()
		w.mu.Unlock()
		return
	}
	if st.AlertedTo != "" || st.Rank < 1 || st.Webhook == "" || st.AfterSec <= 0 || now.Sub(st.Since) < w.wait(st) {
		return
	}
	jobs := "new jobs"
	if len(st.Classes) > 0 {
		jobs = "new jobs of " + strings.Join(st.Classes, ", ")
	}
	text := fmt.Sprintf("%s has not reached the pumasi controller%s at %s for %s (%s): %s wait until it is back, running jobs go on",
		w.host, cond(st.Controller != "", " "+st.Controller, ""), w.addr, now.Sub(st.Since).Round(time.Second), cmpOr(st.Why, "no reason known"), jobs)
	if st.Rank > 1 {
		text += fmt.Sprintf(" (host %d to say so)", st.Rank)
	}
	if err := w.post(ctx, st.Webhook, awayNotice{Text: "[pumasi] " + text, Key: key, Kind: "controller-away", Subject: w.host, State: "problem", Detail: text}); err != nil {
		w.log.Warn("the controller is out of reach, and so is the webhook; trying again", "err", err)
		return
	}
	w.log.Warn("told the webhook that the controller is out of reach", "for", now.Sub(st.Since).Round(time.Second))
	w.mu.Lock()
	w.st.AlertedTo, w.st.AwaySince = st.Webhook, st.Since
	w.save()
	w.mu.Unlock()
}

// wait is how long this host waits before it speaks: the second host twice as long.
func (w *awayWatch) wait(st awayState) time.Duration {
	return time.Duration(st.AfterSec) * time.Second * time.Duration(max(st.Rank, 1))
}
