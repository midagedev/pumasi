//go:build darwin || windows

package main

import (
	"context"
	"net/http"
	"os"
	"time"
)

// What a host whose runners are processes needs before it joins: a way to the
// runner's releases and room to unpack one. The session a runner's jobs can
// use (keychain, windows on screen) is checked by each OS on its own.
func preflightNative(ctx context.Context, p hostPaths, say func(bool, string, ...any) bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	const releases = "https://github.com/actions/runner/releases/latest"
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, releases, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	began := time.Now()
	if resp, err := client.Do(req); err != nil {
		say(false, "runner download: github.com does not answer (%s) — the runner is fetched from its releases the first time", tcpWhy(err))
	} else {
		resp.Body.Close()
		say(true, "runner download: github.com answers (%s)", time.Since(began).Round(time.Millisecond))
	}

	// One unpacked runner is about half a GiB; every runner is a clone of it plus its job's files.
	const need = 3 << 30
	home, _ := os.UserHomeDir()
	switch free := diskFree(home); {
	case free == 0:
		say(true, "disk: free space of %s unknown", home)
	case free < need:
		say(false, "disk: %s free in %s — runners live in %s and need at least %s", gib(free), home, p.dir, gib(need))
	default:
		say(true, "disk: %s free for runners in %s", gib(free), p.dir)
	}
}
