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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeKey(t *testing.T, pkcs8 bool, mode os.FileMode) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	if pkcs8 {
		b, _ := x509.MarshalPKCS8PrivateKey(key)
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: b}
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	return path, key
}

// The App proves itself with a JWT signed by its key and gets the
// installation's token, which is used until shortly before it runs out.
func TestAppGetsAndRenewsItsInstallationToken(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		path, key := writeKey(t, pkcs8, 0o600)
		now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
		asked := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/app/installations/777/access_tokens" {
				t.Errorf("asked %s %s", r.Method, r.URL.Path)
			}
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				t.Fatalf("no JWT: %q", r.Header.Get("Authorization"))
			}
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
				t.Errorf("the JWT is not signed by the App's key: %v", err)
			}
			var claims struct {
				Iss      string
				Iat, Exp int64
			}
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			_ = json.Unmarshal(b, &claims)
			if claims.Iss != "Iv23abc" || claims.Iat > now.Unix() || claims.Exp-claims.Iat > 600 || claims.Exp <= now.Unix() {
				t.Errorf("claims %+v at %d", claims, now.Unix())
			}
			asked++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + strings.Repeat("x", asked), "expires_at": now.Add(time.Hour)})
		}))
		app, err := LoadApp("Iv23abc", 777, path)
		if err != nil {
			t.Fatal(err)
		}
		app.API, app.Now = srv.URL, func() time.Time { return now }
		auth := Auth{App: app}
		ctx := context.Background()
		for range 3 {
			if tok, err := auth.Bearer(ctx); err != nil || tok != "ghs_x" {
				t.Fatalf("token %q, %v", tok, err)
			}
		}
		now = now.Add(54 * time.Minute)
		if tok, _ := auth.Bearer(ctx); tok != "ghs_x" || asked != 1 {
			t.Fatalf("asked again %d times while the token was good for 6 more minutes", asked-1)
		}
		now = now.Add(2 * time.Minute)
		if tok, err := auth.Bearer(ctx); err != nil || tok != "ghs_xx" {
			t.Fatalf("4 minutes before the end: %q, %v", tok, err)
		}
		if strings.Contains(auth.What(), "PRIVATE") || !strings.Contains(auth.What(), "Iv23abc") {
			t.Fatalf("What: %q", auth.What())
		}
		srv.Close()
	}
}

func TestAppRefusalsNameWhatIsWrong(t *testing.T) {
	path, _ := writeKey(t, false, 0o600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
	}))
	defer srv.Close()
	app, err := LoadApp("123", 1, path)
	if err != nil {
		t.Fatal(err)
	}
	app.API = srv.URL
	if _, err := app.Bearer(context.Background()); err == nil || !strings.Contains(err.Error(), "could not be decoded") || !strings.Contains(err.Error(), "installation id") {
		t.Fatalf("%v", err)
	}

	for what, try := range map[string]func() error{
		"no id":           func() error { _, err := LoadApp("", 1, path); return err },
		"no installation": func() error { _, err := LoadApp("1", 0, path); return err },
		"no key file":     func() error { _, err := LoadApp("1", 1, filepath.Join(t.TempDir(), "none.pem")); return err },
		"not a key": func() error {
			p := filepath.Join(t.TempDir(), "x.pem")
			_ = os.WriteFile(p, []byte("hello"), 0o600)
			_, err := LoadApp("1", 1, p)
			return err
		},
	} {
		if err := try(); err == nil {
			t.Errorf("%s: accepted", what)
		} else if strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("%s: the error quotes the key: %v", what, err)
		}
	}
	if runtime.GOOS != "windows" {
		open, _ := writeKey(t, false, 0o644)
		if _, err := LoadApp("1", 1, open); err == nil || !strings.Contains(err.Error(), "others can read it") {
			t.Fatalf("a key others can read: %v", err)
		}
	}
}
