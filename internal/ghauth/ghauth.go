// Package ghauth is how the controller proves to GitHub who it is: a
// personal access token, or a GitHub App's installation.
package ghauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Auth is one of the two. The zero value is no credentials.
type Auth struct {
	Token string // a personal access token
	App   *App
	From  string // where it came from, for people ("PUMASI_GITHUB_TOKEN", "githubApp in pumasi.yaml")
}

// App is a GitHub App installed on the repository or organization. Its
// private key stays in memory: it is read from a file and written nowhere.
type App struct {
	ID             string // the App's client id or app id
	InstallationID int64
	KeyPEM         string

	API  string // "" = https://api.github.com
	HTTP *http.Client
	Now  func() time.Time

	key *rsa.PrivateKey
	mu  sync.Mutex
	tok string
	exp time.Time
}

// LoadApp reads an App's private key file. A key others can read is not
// used: whoever holds it is the App.
func LoadApp(id string, installation int64, keyFile string) (*App, error) {
	switch {
	case id == "":
		return nil, errors.New("the App's id (client id or app id) is missing")
	case installation <= 0:
		return nil, errors.New("the App's installation id is missing")
	case keyFile == "":
		return nil, errors.New("the App's private key file is missing")
	}
	st, err := os.Stat(keyFile)
	if err != nil {
		return nil, fmt.Errorf("the App's private key: %w", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is the App's private key and others can read it (mode %o); chmod 600 it", keyFile, st.Mode().Perm())
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("the App's private key: %w", err)
	}
	a := &App{ID: id, InstallationID: installation, KeyPEM: string(b)}
	if a.key, err = parseKey(b); err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	return a, nil
}

func parseKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("not a PEM file (GitHub gives the App's private key as a .pem)")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

// jwt is the App's own short-lived proof (RS256), as GitHub asks for it.
func (a *App) jwt(now time.Time) (string, error) {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	// Issued a minute ago: the clocks of this machine and GitHub differ.
	head := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iss": a.ID, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	sum := sha256.Sum256([]byte(head))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return head + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Bearer is a token for api.github.com: the installation's, asked for anew
// before it runs out (GitHub gives it an hour).
func (a *App) Bearer(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if a.tok != "" && now.Before(a.exp.Add(-5*time.Minute)) {
		return a.tok, nil
	}
	jwt, err := a.jwt(now)
	if err != nil {
		return "", err
	}
	api := a.API
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", api, a.InstallationID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("GitHub did not give the App an installation token (%s: %s) — check the App's id, the installation id and that the key is this App's", resp.Status, strings.TrimSpace(e.Message))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", errors.New("GitHub's answer holds no installation token")
	}
	a.tok, a.exp = out.Token, out.ExpiresAt
	return a.tok, nil
}

// Bearer is what goes after "Authorization: Bearer" at api.github.com.
func (a Auth) Bearer(ctx context.Context) (string, error) {
	if a.App != nil {
		return a.App.Bearer(ctx)
	}
	if a.Token == "" {
		return "", errors.New("no GitHub credentials")
	}
	return a.Token, nil
}

// What names the credentials for people, without any of their content.
func (a Auth) What() string {
	if a.App != nil {
		return fmt.Sprintf("the GitHub App %s (installation %d)", a.App.ID, a.App.InstallationID)
	}
	return "the token"
}
