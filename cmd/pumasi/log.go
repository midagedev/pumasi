package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// lineHandler prints one line per event, written for a person:
//
//	12:00:03 start linux-l on server1  free 3.0cpu/5.0G
//
// Attributes, when a line has any, follow as key=value.
type lineHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	attrs string
}

func newLineHandler(w io.Writer, level slog.Level) *lineHandler {
	return &lineHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *lineHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05 "))
	switch {
	case r.Level >= slog.LevelError:
		b.WriteString("error: ")
	case r.Level >= slog.LevelWarn:
		b.WriteString("warning: ")
	}
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(formatAttr(a))
		return true
	})
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func formatAttr(a slog.Attr) string {
	v := a.Value.Resolve().String()
	if strings.ContainsAny(v, " \t\n") {
		v = fmt.Sprintf("%q", v)
	}
	return "  " + a.Key + "=" + v
}

func (h *lineHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	for _, a := range as {
		n.attrs += formatAttr(a)
	}
	return &n
}

func (h *lineHandler) WithGroup(string) slog.Handler { return h }
