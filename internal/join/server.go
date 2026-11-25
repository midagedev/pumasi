package join

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

//go:embed join.sh
var joinScript string

// Member is a host that joined, as the controller remembers it across restarts.
type Member struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	CPUs       float64   `json:"cpus"`
	Mem        int64     `json:"mem"`
	SecretHash string    `json:"secretHash"`
	JoinedAt   time.Time `json:"joinedAt"`
	From       string    `json:"from"`
}

func membersPath(dir string) string { return filepath.Join(dir, "hosts.json") }

// ReadMembers lists the hosts that joined the controller keeping its state in dir.
func ReadMembers(dir string) []Member {
	var ms []Member
	if b, err := os.ReadFile(membersPath(dir)); err == nil {
		_ = json.Unmarshal(b, &ms)
	}
	return ms
}

// Server is the controller's side: the listener hosts connect to.
type Server struct {
	Dir  string // the controller's state directory: key pair, token, members
	Dist string // holds pumasi-linux-<arch>, handed to hosts
	// Reserved reports a name a joining host may not take (a host of the config).
	Reserved func(name string) bool
	// Changed is called after a host joined, left, connected or dropped.
	Changed func()
	Log     *slog.Logger

	mu      sync.Mutex
	members []Member
	peers   map[string]*Peer // by member id
	id      Identity
	srv     *http.Server
	version map[string]versionOf // by file path
}

type versionOf struct {
	mod  time.Time
	size int64
	sha  string
}

// NewServer loads the members of dir. It does not listen yet.
func NewServer(dir, dist string, log *slog.Logger) *Server {
	return &Server{Dir: dir, Dist: dist, Log: log, members: ReadMembers(dir), peers: map[string]*Peer{},
		version: map[string]versionOf{}, Reserved: func(string) bool { return false }, Changed: func() {}}
}

// Listen starts answering hosts on addr and returns the address it bound.
func (s *Server) Listen(addr string) (net.Addr, error) {
	id, err := LoadIdentity(s.Dir)
	if err != nil {
		return nil, err
	}
	if _, err := Token(s.Dir); err != nil {
		return nil, err
	}
	// http/1.1 only: the session takes the connection over, which h2 cannot give.
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{id.Cert},
		MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /join.sh", s.script)
	mux.HandleFunc("GET /dl/{file}", s.download)
	mux.HandleFunc("POST /v1/join", s.register)
	mux.HandleFunc("POST /v1/leave", s.leave)
	mux.HandleFunc("POST /v1/session", s.session)
	s.mu.Lock()
	s.id = id
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(discard{}, slog.LevelError)}
	s.mu.Unlock()
	go func() { _ = s.srv.Serve(ln) }()
	return ln.Addr(), nil
}

// discard drops the http server's own lines (TLS handshake noise from port scans).
type discard struct{}

func (discard) Enabled(context.Context, slog.Level) bool  { return false }
func (discard) Handle(context.Context, slog.Record) error { return nil }
func (d discard) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discard) WithGroup(string) slog.Handler           { return d }

// Close stops listening and drops every session.
func (s *Server) Close() {
	s.mu.Lock()
	srv := s.srv
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	for _, p := range peers {
		p.drop(nil, "controller stopped")
	}
}

// Members is every host that joined, connected or not.
func (s *Server) Members() []Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.members)
}

// Peer is the live side of a member: its session when it has one.
func (s *Server) Peer(id string) *Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[id]
	if p == nil {
		p = &Peer{up: make(chan struct{})}
		s.peers[id] = p
	}
	return p
}

// Forget removes a member: its secret no longer opens a session.
func (s *Server) Forget(id string) {
	s.mu.Lock()
	s.members = slices.DeleteFunc(s.members, func(m Member) bool { return m.ID == id })
	p := s.peers[id]
	delete(s.peers, id)
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		s.Log.Error("join: save members", "err", err)
	}
	if p != nil {
		p.drop(nil, "host left")
	}
	s.Changed()
}

func (s *Server) save() error {
	b, err := json.MarshalIndent(s.members, "", "  ")
	if err != nil {
		return err
	}
	tmp := membersPath(s.Dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, membersPath(s.Dir))
}

func (s *Server) member(id string) (Member, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.members, func(m Member) bool { return m.ID == id })
	if i < 0 {
		return Member{}, false
	}
	return s.members[i], true
}

func (s *Server) script(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	// r.Host is the address this host reached the controller at; it keeps using it.
	_, _ = strings.NewReplacer("{{controller}}", r.Host, "{{pin}}", s.id.Pin).WriteString(w, joinScript)
}

var distFile = regexp.MustCompile(`^pumasi-linux-(amd64|arm64)(\.sha256)?$`)

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	m := distFile.FindStringSubmatch(r.PathValue("file"))
	if m == nil {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.Dist, "pumasi-linux-"+m[1])
	if m[2] == "" {
		http.ServeFile(w, r, path)
		return
	}
	v := s.wanted(m[1])
	if v == "" {
		http.NotFound(w, r)
		return
	}
	fmt.Fprintln(w, v)
}

// wanted is the build the controller hands to hosts of arch ("" = it has none).
func (s *Server) wanted(arch string) string {
	if !slices.Contains([]string{"amd64", "arm64"}, arch) {
		return ""
	}
	path := filepath.Join(s.Dist, "pumasi-linux-"+arch)
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	v, ok := s.version[path]
	s.mu.Unlock()
	if ok && v.mod.Equal(fi.ModTime()) && v.size == fi.Size() {
		return v.sha
	}
	v = versionOf{mod: fi.ModTime(), size: fi.Size(), sha: FileVersion(path)}
	s.mu.Lock()
	s.version[path] = v
	s.mu.Unlock()
	return v.sha
}

// Names end up in runner names and container names (the same rule as the config's).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
var idRE = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var reg Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&reg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch {
	case !idRE.MatchString(reg.ID) || len(reg.Secret) < 16:
		http.Error(w, "bad host id or secret", http.StatusBadRequest)
		return
	case !nameRE.MatchString(reg.Name):
		http.Error(w, fmt.Sprintf("name %q: use letters, digits, '.', '_' or '-' (pass --name)", reg.Name), http.StatusBadRequest)
		return
	case reg.Kind != "server" && reg.Kind != "desktop":
		http.Error(w, "kind must be server or desktop", http.StatusBadRequest)
		return
	case reg.CPUs <= 0 || reg.Mem <= 0:
		http.Error(w, "the budget needs cpus and memory above zero (pass --cpus and --mem)", http.StatusBadRequest)
		return
	}
	known, isMember := s.member(reg.ID)
	token, _ := Token(s.Dir)
	byToken := reg.Token != "" && token != "" && same(reg.Token, token)
	bySecret := isMember && same(hashSecret(reg.Secret), known.SecretHash)
	if !byToken && !bySecret {
		why := "this host is not known to the controller"
		if reg.Token != "" {
			why = "wrong token"
			if fi, err := os.Stat(tokenPath(s.Dir)); err == nil {
				why = "rotated on " + fi.ModTime().Format("01-02 15:04")
			}
		}
		http.Error(w, why, http.StatusUnauthorized)
		return
	}
	if s.Reserved(reg.Name) {
		http.Error(w, fmt.Sprintf("name %s is a host of the controller's pumasi.yaml; pass --name", reg.Name), http.StatusConflict)
		return
	}
	from, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	for _, m := range s.members {
		if m.Name == reg.Name && m.ID != reg.ID {
			s.mu.Unlock()
			http.Error(w, fmt.Sprintf("name %s is taken by another host (joined %s from %s); pass --name",
				reg.Name, m.JoinedAt.Format("01-02 15:04"), m.From), http.StatusConflict)
			return
		}
	}
	if reg.Probe {
		s.mu.Unlock()
		return
	}
	m := Member{ID: reg.ID, Name: reg.Name, Kind: reg.Kind, CPUs: reg.CPUs, Mem: reg.Mem,
		SecretHash: hashSecret(reg.Secret), JoinedAt: time.Now(), From: from}
	if i := slices.IndexFunc(s.members, func(x Member) bool { return x.ID == reg.ID }); i >= 0 {
		s.members[i] = m
	} else {
		s.members = append(s.members, m)
	}
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "the controller could not save the host: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Changed()
}

// leave is a host saying goodbye by itself (`pumasi leave`).
func (s *Server) leave(w http.ResponseWriter, r *http.Request) {
	var reg Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&reg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if m, ok := s.member(reg.ID); !ok || !same(hashSecret(reg.Secret), m.SecretHash) {
		http.Error(w, "this host is not known to the controller", http.StatusUnauthorized)
		return
	}
	s.Forget(reg.ID)
}

// session turns the request's connection into the host's session.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(hdrID)
	m, ok := s.member(id)
	if !ok || !same(hashSecret(r.Header.Get(hdrSecret)), m.SecretHash) {
		http.Error(w, "this host is not known to the controller; run the invite line again (`pumasi invite`)", http.StatusUnauthorized)
		return
	}
	version, stay := r.Header.Get(hdrVersion), r.Header.Get(hdrStay)
	if want := s.wanted(r.Header.Get(hdrArch)); want != "" && want != version && stay == "" {
		w.Header().Set(hdrVersion, want)
		http.Error(w, "the controller serves another build", http.StatusUpgradeRequired)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot take over the connection", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: pumasi\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	wc := watch(conn, buf.Reader)
	// The host serves HTTP, so the controller is the side that opens streams.
	sess, err := yamux.Client(wc, muxConfig())
	if err != nil {
		conn.Close()
		return
	}
	p := s.Peer(id)
	p.attach(sess, version, stay)
	s.Changed()
	go func() {
		<-sess.CloseChan()
		if p.drop(sess, wc.why("agent")) {
			s.Changed()
		}
	}()
}

// Peer holds the session of one member, if it has one right now.
type Peer struct {
	mu        sync.Mutex
	sess      *yamux.Session
	up        chan struct{} // closed while connected
	since     time.Time
	version   string
	stay      string
	why       string // why the last session ended
	droppedAt time.Time
}

func (p *Peer) attach(sess *yamux.Session, version, stay string) {
	p.mu.Lock()
	old := p.sess
	p.sess, p.version, p.stay, p.since = sess, version, stay, time.Now()
	if old == nil {
		close(p.up)
	}
	p.mu.Unlock()
	if old != nil {
		// The host dialed again before this side noticed the old connection was dead.
		_ = old.Close()
	}
}

// drop ends sess (nil = whichever is current) and records why. It reports
// whether that was the current session.
func (p *Peer) drop(sess *yamux.Session, why string) bool {
	p.mu.Lock()
	if p.sess == nil || (sess != nil && sess != p.sess) {
		p.mu.Unlock()
		return false
	}
	cur := p.sess
	p.sess, p.up, p.why, p.droppedAt = nil, make(chan struct{}), why, time.Now()
	p.mu.Unlock()
	_ = cur.Close()
	return true
}

// Dial opens a stream to the host; it fits http.Transport.DialContext.
func (p *Peer) Dial(context.Context, string, string) (net.Conn, error) {
	p.mu.Lock()
	sess, why := p.sess, p.why
	p.mu.Unlock()
	if sess == nil {
		if why == "" {
			return nil, errors.New("the host has not connected")
		}
		return nil, fmt.Errorf("the host is not connected (%s)", why)
	}
	return sess.Open()
}

// Wait blocks until the host is connected or ctx ends, and reports which.
func (p *Peer) Wait(ctx context.Context) bool {
	p.mu.Lock()
	up := p.up
	p.mu.Unlock()
	select {
	case <-up:
		return true
	case <-ctx.Done():
		return false
	}
}

// Link is what a peer's connection looks like from outside.
type Link struct {
	Connected bool
	Since     time.Time // connected since
	Version   string    // the host's build
	Stay      string    // why the host could not switch to the controller's build
	Why       string    // why the last session ended
	DroppedAt time.Time
}

func (p *Peer) Link() Link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Link{Connected: p.sess != nil, Since: p.since, Version: p.version, Stay: p.stay, Why: p.why, DroppedAt: p.droppedAt}
}
