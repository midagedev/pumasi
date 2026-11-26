package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/midagedev/pumasi/internal/config"
)

func TestGithubTokenFromEnvOrKeptFile(t *testing.T) {
	cfg := &config.Config{Ledger: filepath.Join(t.TempDir(), "pumasi.db")}
	t.Setenv("PUMASI_GITHUB_TOKEN", "")
	if _, _, err := githubToken(cfg); err == nil || !strings.Contains(err.Error(), "PUMASI_GITHUB_TOKEN is empty") {
		t.Fatalf("no token anywhere: %v", err)
	}
	if err := keepToken(cfg, "kept"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(tokenFile(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("the kept token has mode %o", st.Mode().Perm())
	}
	if tok, from, err := githubToken(cfg); err != nil || tok != "kept" || from != tokenFile(cfg) {
		t.Fatalf("kept file: %q from %q, %v", tok, from, err)
	}
	t.Setenv("PUMASI_GITHUB_TOKEN", "env")
	if tok, from, _ := githubToken(cfg); tok != "env" || from != "PUMASI_GITHUB_TOKEN" {
		t.Fatalf("the environment goes first: %q from %q", tok, from)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// A token others can read is not used: the file may have been made by hand.
	t.Setenv("PUMASI_GITHUB_TOKEN", "")
	if err := os.Chmod(tokenFile(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := githubToken(cfg); err == nil || !strings.Contains(err.Error(), "others can read it") {
		t.Fatalf("a readable token file: %v", err)
	}
}
