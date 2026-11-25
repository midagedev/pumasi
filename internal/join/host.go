package join

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// HostConfig is what a joined host keeps (join.json): where its controller is,
// how to recognise it, who the host is and what it lends.
type HostConfig struct {
	Controller string  `json:"controller"` // host:port
	Pin        string  `json:"pin"`
	ID         string  `json:"id"`
	Secret     string  `json:"secret"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	CPUs       float64 `json:"cpus"`
	Mem        int64   `json:"mem"`
}

// LoadHostConfig reads join.json.
func LoadHostConfig(path string) (HostConfig, error) {
	var c HostConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// Save writes join.json; it holds the host's secret.
func (c HostConfig) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// NewHostID makes the id and secret of a host that joins for the first time.
func NewHostID() (id, secret string) { return randomHex(8), randomHex(24) }

// ErrPin: the server answering at the controller's address has another key.
var ErrPin = errors.New("the controller's certificate does not match the invite")

// tlsConfig trusts exactly the key the invite named, and nothing else about the certificate.
func (c HostConfig) tlsConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // the pin below is the whole check
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"http/1.1"},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return ErrPin
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil || !same(pinOf(leaf), c.Pin) {
				return ErrPin
			}
			return nil
		},
	}
}

func (c HostConfig) dial(ctx context.Context) (net.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}, Config: c.tlsConfig()}
	return d.DialContext(ctx, "tcp", c.Controller)
}

func (c HostConfig) client() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{
		DialTLSContext:    func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
		DisableKeepAlives: true}}
}

// CheckPin connects once and reports whether the controller shows the invited key.
func (c HostConfig) CheckPin(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Refused is the controller saying no, in its own words.
type Refused struct {
	Code int
	Why  string
}

func (r *Refused) Error() string { return r.Why }

func (c HostConfig) post(ctx context.Context, path string, reg Registration) error {
	b, _ := json.Marshal(reg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.Controller+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return &Refused{Code: resp.StatusCode, Why: strings.TrimSpace(string(body))}
	}
	return nil
}

// Register joins the host with the invite's token. probe only asks whether it
// would work. An empty token asks as an already joined host (by its secret).
func (c HostConfig) Register(ctx context.Context, token string, probe bool) error {
	return c.post(ctx, "/v1/join", Registration{Token: token, Probe: probe, ID: c.ID, Secret: c.Secret,
		Name: c.Name, Kind: c.Kind, CPUs: c.CPUs, Mem: c.Mem})
}

// Leave tells the controller this host is gone.
func (c HostConfig) Leave(ctx context.Context) error {
	return c.post(ctx, "/v1/leave", Registration{ID: c.ID, Secret: c.Secret})
}

// Download fetches the controller's build for a system into w.
func (c HostConfig) Download(ctx context.Context, goos, arch string, w io.Writer) error {
	name := DistName(goos, arch)
	if name == "" {
		return fmt.Errorf("download: there is no pumasi build for %s/%s", goos, arch)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.Controller+"/dl/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: the controller answered %s", resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Host keeps a joined host connected: it dials, serves Handler over the
// session and dials again whenever the session ends.
type Host struct {
	Config  HostConfig
	Version string // this build
	OS      string // GOOS
	Arch    string
	Handler http.Handler
	// Upgrade replaces this process with the controller's build want. It only
	// returns when that did not happen: with the build now running, or an error.
	Upgrade func(ctx context.Context, want string) (string, error)
	// State is told of every change: connected, or not and why.
	State func(connected bool, why string)
	Log   *slog.Logger

	stay string
	last string // why the previous session of this process ended
}

const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// Run returns when ctx ends.
func (h *Host) Run(ctx context.Context) {
	if h.State == nil {
		h.State = func(bool, string) {}
	}
	wait := backoffMin
	for ctx.Err() == nil {
		began := time.Now()
		connected, why := h.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		h.State(false, why)
		if connected {
			h.last = why
			h.Log.Warn("disconnected from the controller; dialing again", "why", why, "after", time.Since(began).Round(time.Second))
			if time.Since(began) > backoffMax {
				wait = backoffMin
			}
		} else {
			h.Log.Warn("cannot connect to the controller", "controller", h.Config.Controller, "why", why, "retryIn", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, backoffMax)
	}
}

// connect holds one session until it ends. It reports whether there was one
// and why it ended (or why there was none).
func (h *Host) connect(ctx context.Context) (bool, string) {
	conn, err := h.Config.dial(ctx)
	if err != nil {
		return false, dialWhy(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://"+h.Config.Controller+"/v1/session", nil)
	req.Header.Set(hdrID, h.Config.ID)
	req.Header.Set(hdrSecret, h.Config.Secret)
	req.Header.Set(hdrVersion, h.Version)
	req.Header.Set(hdrArch, h.Arch)
	if h.OS != "" {
		req.Header.Set(hdrOS, h.OS)
	}
	if h.last != "" {
		req.Header.Set(hdrWhy, h.last)
	}
	if h.stay != "" {
		req.Header.Set(hdrStay, h.stay)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return false, dialWhy(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return false, dialWhy(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		conn.Close()
		if want := resp.Header.Get(hdrVersion); resp.StatusCode == http.StatusUpgradeRequired && want != "" {
			return false, h.upgrade(ctx, want)
		}
		return false, "the controller refused: " + strings.TrimSpace(string(body))
	}
	_ = conn.SetDeadline(time.Time{})
	wc := watch(conn, br)
	sess, err := yamux.Server(wc, muxConfig())
	if err != nil {
		conn.Close()
		return false, err.Error()
	}
	h.State(true, "")
	h.Log.Info("connected to the controller", "controller", h.Config.Controller)
	srv := &http.Server{Handler: h.Handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(sess) }()
	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}
	_ = srv.Close()
	_ = sess.Close()
	return true, wc.why("the controller closed the connection")
}

// upgrade switches to the controller's build. When that fails the host stays
// on its own build and says so on the next connection.
func (h *Host) upgrade(ctx context.Context, want string) string {
	if h.Upgrade == nil {
		h.stay = "this host cannot replace its own binary"
		return h.stay
	}
	h.Log.Info("the controller serves another build; switching", "from", short(h.Version), "to", short(want))
	now, err := h.Upgrade(ctx, want)
	switch {
	case err != nil:
		h.stay = "could not switch to build " + short(want) + ": " + err.Error()
	case now != want:
		h.stay = "still runs build " + short(now) + " after switching to " + short(want)
	default:
		h.Version, h.stay = now, ""
		return "switched to build " + short(want)
	}
	return h.stay
}

func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// dialWhy names why no connection came about, in words that tell the cases apart.
func dialWhy(err error) string {
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.Is(err, ErrPin):
		return ErrPin.Error()
	case errors.As(err, &dns):
		return "cannot resolve " + dns.Name
	case errors.As(err, &ne) && ne.Timeout():
		return "no answer (timeout)"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:] // "dial tcp 10.0.0.5:7411: connect: connection refused" -> "connection refused"
	}
	return s
}
