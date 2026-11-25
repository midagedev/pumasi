// Package join is the other way to attach a host: the host dials the
// controller. The controller listens on one TLS port; a host connects, proves
// who it is and keeps one connection open. Over that connection (yamux) the
// controller calls the same HTTP contract an ssh tunnel would carry, so
// nothing above the transport knows which side placed the call.
package join

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
)

// Request headers of POST /v1/session, the call that becomes the session.
const (
	hdrID      = "Pumasi-Id"
	hdrSecret  = "Pumasi-Secret"
	hdrVersion = "Pumasi-Version" // also on the answer: the build the controller serves
	hdrArch    = "Pumasi-Arch"
	hdrOS      = "Pumasi-Os"   // linux when absent (hosts from before other systems could join)
	hdrWhy     = "Pumasi-Why"  // why the host's previous session ended, as the host saw it
	hdrNote    = "Pumasi-Note" // what the host itself had to do with that end (it was stalled, asleep)
	hdrStay    = "Pumasi-Stay" // why the host could not switch to the controller's build
)

// Why a connection ended, as both sides name it.
const (
	WhyKeepalive = "keepalive timeout"
	WhyReset     = "connection reset"
	WhyReplaced  = "replaced by a new connection"
	// WhyExited is the controller's reading of a connection the host closed:
	// the agent ended. A host that closed it for another reason says so when it returns.
	WhyExited = "agent exited"
)

var distRE = regexp.MustCompile(`^pumasi-(linux|darwin|windows)-(amd64|arm64)$`)

// DistName is the file name of the build for a system, as `make` leaves it in
// dist and as hosts ask for it ("" = no such build).
func DistName(goos, arch string) string {
	name := "pumasi-" + goos + "-" + arch
	if !distRE.MatchString(name) {
		return ""
	}
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Registration is what a host says about itself when it joins.
type Registration struct {
	Token  string  `json:"token,omitempty"`
	Probe  bool    `json:"probe,omitempty"` // only check; register nothing
	ID     string  `json:"id"`
	Secret string  `json:"secret"`
	Name   string  `json:"name"`
	Kind   string  `json:"kind"` // server | desktop
	CPUs   float64 `json:"cpus"`
	Mem    int64   `json:"mem"`
}

// Identity is the controller's TLS key pair. Hosts trust it by Pin alone.
type Identity struct {
	Cert tls.Certificate
	Pin  string // curl's --pinnedpubkey form: sha256//<base64 of the SPKI hash>
}

func pinOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

// LoadIdentity reads the controller's key pair from dir, creating it once.
func LoadIdentity(dir string) (Identity, error) {
	keyPath, crtPath := filepath.Join(dir, "join.key"), filepath.Join(dir, "join.crt")
	cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := newIdentity(keyPath, crtPath); err != nil {
			return Identity{}, err
		}
		cert, err = tls.LoadX509KeyPair(crtPath, keyPath)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("join identity: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return Identity{}, err
	}
	return Identity{Cert: cert, Pin: pinOf(leaf)}, nil
}

func newIdentity(keyPath, crtPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "pumasi controller"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(20, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
}

func tokenPath(dir string) string { return filepath.Join(dir, "join.token") }

// Token is the secret a new host shows to join, created once. The file's
// modification time is when it was last rotated.
func Token(dir string) (string, error) {
	b, err := os.ReadFile(tokenPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return RotateToken(dir)
	}
	return strings.TrimSpace(string(b)), err
}

// RotateToken replaces the token. Hosts that joined before keep their own secret.
func RotateToken(dir string) (string, error) {
	t := randomHex(16)
	tmp := tokenPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, os.Rename(tmp, tokenPath(dir))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// FileVersion names a build by the sha256 of its binary ("" = unreadable).
func FileVersion(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// muxConfig: a ping every 10s, and a connection that does not answer one
// within another 10s is dropped by whichever side notices first.
func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 10 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamOpenTimeout = 20 * time.Second
	c.LogOutput = io.Discard
	return c
}

// watched remembers the first read error of a connection: yamux closes a
// session without saying why, and why is what the operator needs.
type watched struct {
	net.Conn
	r  io.Reader
	mu sync.Mutex
	e  error
}

func watch(c net.Conn, buffered *bufio.Reader) *watched {
	w := &watched{Conn: c, r: c}
	if buffered != nil {
		w.r = buffered
	}
	return w
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if err != nil {
		w.mu.Lock()
		if w.e == nil {
			w.e = err
		}
		w.mu.Unlock()
	}
	return n, err
}

// why turns the read error into the words shown to people. closed is what a
// clean end from the other side means here ("agent exited").
func (w *watched) why(closed string) string {
	w.mu.Lock()
	err := w.e
	w.mu.Unlock()
	switch {
	case err == nil || errors.Is(err, net.ErrClosed):
		// Nothing arrived, and it was this side's keepalive that gave up.
		return WhyKeepalive
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return closed
	case errors.Is(err, syscall.ECONNRESET):
		return WhyReset
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return WhyKeepalive
	}
	return err.Error()
}
