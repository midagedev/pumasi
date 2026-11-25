package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/midagedev/pumasi/internal/join"
)

func TestInviteHasALinePerShell(t *testing.T) {
	text := inviteText("sha256//abc=", "10.0.0.5:7411", "tok")
	for _, want := range []string{
		"curl -fsSk --pinnedpubkey 'sha256//abc=' https://10.0.0.5:7411/join.sh | sh -s -- tok\n",
		"& ([scriptblock]::Create((curl.exe -fsSk --pinnedpubkey 'sha256//abc=' https://10.0.0.5:7411/join.ps1 | Out-String))) tok\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestPathsAreTheSameOnEveryOS(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	p := paths()
	if filepath.Base(p.dir) != "pumasi" || filepath.Base(filepath.Dir(p.dir)) != ".cache" {
		t.Fatalf("dir %s", p.dir)
	}
	if b := installedBin(); !strings.Contains(filepath.ToSlash(b), "/.local/bin/pumasi") {
		t.Fatalf("bin %s", b)
	}
}

// A controller nobody listens at: every network line fails by name and the
// host's own checks still run.
func TestPreflightNamesWhatFailed(t *testing.T) {
	var out bytes.Buffer
	ok := preflight(context.Background(), &out, join.HostConfig{Controller: "127.0.0.1:1", Pin: "sha256//x", Name: "h"}, "tok")
	if ok || !strings.Contains(out.String(), "FAIL  tcp 127.0.0.1:1") {
		t.Fatalf("ok=%v\n%s", ok, out.String())
	}
	if n := strings.Count(out.String(), "\n"); n < 3 {
		t.Fatalf("the host's own checks did not run:\n%s", out.String())
	}
}
