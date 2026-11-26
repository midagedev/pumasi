package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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

func TestGithubAuthIsAnAppOrAToken(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "app.pem"), pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"PUMASI_GITHUB_TOKEN", "PUMASI_GITHUB_APP_ID", "PUMASI_GITHUB_APP_INSTALLATION_ID", "PUMASI_GITHUB_APP_PRIVATE_KEY_FILE"} {
		t.Setenv(k, "")
	}
	cfg := &config.Config{Path: filepath.Join(dir, "pumasi.yaml"), Ledger: filepath.Join(dir, "state", "pumasi.db")}

	// Nothing given: the token's own message.
	if _, err := githubAuth(cfg); err == nil || !strings.Contains(err.Error(), "PUMASI_GITHUB_TOKEN is empty") {
		t.Fatalf("nothing given: %v", err)
	}
	// The config's App; the key file is relative to the config.
	cfg.GitHubApp = config.GitHubApp{ID: "Iv23abc", InstallationID: 42, PrivateKeyFile: "app.pem"}
	a, err := githubAuth(cfg)
	if err != nil || a.App == nil || a.Token != "" || a.App.ID != "Iv23abc" || a.App.InstallationID != 42 || !strings.Contains(a.From, "pumasi.yaml") {
		t.Fatalf("config App: %+v, %v", a, err)
	}
	// The environment names another one.
	t.Setenv("PUMASI_GITHUB_APP_ID", "999")
	t.Setenv("PUMASI_GITHUB_APP_INSTALLATION_ID", "7")
	if a, err = githubAuth(cfg); err != nil || a.App.ID != "999" || a.App.InstallationID != 7 || a.From != "PUMASI_GITHUB_APP_*" {
		t.Fatalf("env App: %+v, %v", a, err)
	}
	t.Setenv("PUMASI_GITHUB_APP_INSTALLATION_ID", "seven")
	if _, err = githubAuth(cfg); err == nil {
		t.Fatal("an installation id that is no number was taken")
	}
	t.Setenv("PUMASI_GITHUB_APP_INSTALLATION_ID", "")
	// Both an App and a token: pumasi does not choose.
	t.Setenv("PUMASI_GITHUB_TOKEN", "ghp_x")
	if _, err = githubAuth(cfg); err == nil || !strings.Contains(err.Error(), "both a GitHub App") {
		t.Fatalf("both: %v", err)
	}
	// No App: the token.
	cfg.GitHubApp = config.GitHubApp{}
	t.Setenv("PUMASI_GITHUB_APP_ID", "")
	if a, err = githubAuth(cfg); err != nil || a.App != nil || a.Token != "ghp_x" {
		t.Fatalf("token: %+v, %v", a, err)
	}
	// Nothing of the key is in what is said about it.
	t.Setenv("PUMASI_GITHUB_TOKEN", "")
	cfg.GitHubApp = config.GitHubApp{ID: "1", InstallationID: 1, PrivateKeyFile: filepath.Join(dir, "missing.pem")}
	if _, err = githubAuth(cfg); err == nil || strings.Contains(err.Error(), "BEGIN") {
		t.Fatalf("missing key: %v", err)
	}
}
