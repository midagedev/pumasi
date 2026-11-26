package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ghauth"
	"github.com/midagedev/pumasi/internal/ledger"
)

// The scale set client is made from the App as it is made from a token.
func TestControllerTakesAGitHubApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := ghauth.LoadApp("Iv23abc", 42, path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{URL: "https://github.com/example/repo", Ledger: filepath.Join(t.TempDir(), "pumasi.db"), Classes: []config.Class{{Name: "build"}}}
	l, err := ledger.Open(cfg.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := New(cfg, ghauth.Auth{App: app}, l, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.auth.What() != "the GitHub App Iv23abc (installation 42)" {
		t.Fatalf("%q", c.auth.What())
	}
}
