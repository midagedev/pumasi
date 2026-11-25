package join

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// controller starts a listener on loopback and returns it with a host config
// that points at it (not yet registered).
func controller(t *testing.T) (*Server, HostConfig, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewServer(dir, t.TempDir(), quiet)
	addr, err := s.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	token, _ := Token(dir)
	id, secret := NewHostID()
	return s, HostConfig{Controller: addr.String(), Pin: s.id.Pin, ID: id, Secret: secret,
		Name: "desk1", Kind: "desktop", CPUs: 6, Mem: 9 << 30}, token
}

// run keeps the host connected until the returned stop is called.
func run(t *testing.T, h *Host) (stop func()) {
	t.Helper()
	if h.Handler == nil {
		h.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello from "+h.Config.Name) })
	}
	h.Log = quiet
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func get(t *testing.T, p *Peer) (string, error) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: p.Dial, DisableKeepAlives: true}}
	resp, err := c.Get("http://host/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func waitUp(t *testing.T, p *Peer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !p.Wait(ctx) {
		t.Fatalf("host did not connect: %+v", p.Link())
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("never: " + what)
}

func TestHostJoinsAndTheControllerCallsItOverTheSession(t *testing.T) {
	s, hc, token := controller(t)
	changed := make(chan struct{}, 16)
	s.Changed = func() { changed <- struct{}{} }
	if err := hc.Register(context.Background(), token, false); err != nil {
		t.Fatal(err)
	}
	ms := s.Members()
	if len(ms) != 1 || ms[0].Name != "desk1" || ms[0].Kind != "desktop" || ms[0].CPUs != 6 || ms[0].From != "127.0.0.1" {
		t.Fatalf("members %+v", ms)
	}
	if got := ReadMembers(s.Dir); len(got) != 1 || got[0].SecretHash == hc.Secret {
		t.Fatalf("saved members %+v", got)
	}
	p := s.Peer(hc.ID)
	if _, err := get(t, p); err == nil || !strings.Contains(err.Error(), "has not connected") {
		t.Fatalf("before the host dials: %v", err)
	}
	stop := run(t, &Host{Config: hc})
	waitUp(t, p)
	if got, err := get(t, p); err != nil || got != "hello from desk1" {
		t.Fatalf("over the session: %q %v", got, err)
	}

	// The host goes away: the controller names why, and calls fail with that reason.
	stop()
	eventually(t, "drop noticed", func() bool { return !p.Link().Connected })
	if why := p.Link().Why; why != "agent exited" {
		t.Fatalf("why %q", why)
	}
	if _, err := get(t, p); err == nil || !strings.Contains(err.Error(), "not connected (agent exited)") {
		t.Fatalf("after the drop: %v", err)
	}
	// It dials again and is reachable again.
	run(t, &Host{Config: hc})
	waitUp(t, p)
	if _, err := get(t, p); err != nil {
		t.Fatal(err)
	}
	if len(changed) < 4 { // joined, connected, dropped, connected
		t.Fatalf("Changed called %d times", len(changed))
	}
}

func TestControllerRestartHostDialsAgain(t *testing.T) {
	s, hc, token := controller(t)
	if err := hc.Register(context.Background(), token, false); err != nil {
		t.Fatal(err)
	}
	var states []string
	h := &Host{Config: hc, State: func(up bool, why string) {
		if !up {
			states = append(states, why)
		}
	}}
	run(t, h)
	waitUp(t, s.Peer(hc.ID))
	s.Close()

	// A new controller on the same state directory and port knows the host.
	s2 := NewServer(s.Dir, s.Dist, quiet)
	var err error
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err = s2.Listen(hc.Controller); err == nil || time.Now().After(end) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	waitUp(t, s2.Peer(hc.ID))
	if got, err := get(t, s2.Peer(hc.ID)); err != nil || got != "hello from desk1" {
		t.Fatalf("%q %v", got, err)
	}
	if len(states) == 0 || states[0] != "the controller closed the connection" {
		t.Fatalf("the host saw %q", states)
	}
}

func TestRefusalsSayWhy(t *testing.T) {
	s, hc, token := controller(t)
	ctx := context.Background()

	var ref *Refused
	err := hc.Register(ctx, "not-the-token", true)
	if !errors.As(err, &ref) || ref.Code != http.StatusUnauthorized || !strings.HasPrefix(ref.Why, "rotated on ") {
		t.Fatalf("wrong token: %v", err)
	}

	wrongPin := hc
	wrongPin.Pin = "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if err := wrongPin.CheckPin(ctx); !errors.Is(err, ErrPin) {
		t.Fatalf("wrong pin: %v", err)
	}
	if err := wrongPin.Register(ctx, token, false); !errors.Is(err, ErrPin) {
		t.Fatalf("wrong pin, register: %v", err)
	}
	if why := dialWhy(wrongPin.CheckPin(ctx)); why != ErrPin.Error() {
		t.Fatalf("why %q", why)
	}

	// A probe registers nothing.
	if err := hc.Register(ctx, token, true); err != nil || len(s.Members()) != 0 {
		t.Fatalf("probe: %v, members %d", err, len(s.Members()))
	}
	if err := hc.Register(ctx, token, false); err != nil {
		t.Fatal(err)
	}

	// Another machine asking for the same name is told who has it.
	other := hc
	other.ID, other.Secret = NewHostID()
	err = other.Register(ctx, token, false)
	if !errors.As(err, &ref) || ref.Code != http.StatusConflict || !strings.Contains(ref.Why, "name desk1 is taken by another host") {
		t.Fatalf("name conflict: %v", err)
	}
	s.Reserved = func(n string) bool { return n == "server1" }
	other.Name = "server1"
	if err := other.Register(ctx, token, false); !errors.As(err, &ref) || !strings.Contains(ref.Why, "pumasi.yaml") {
		t.Fatalf("reserved name: %v", err)
	}
	other.Name = "bad name;rm"
	if err := other.Register(ctx, token, false); !errors.As(err, &ref) || ref.Code != http.StatusBadRequest {
		t.Fatalf("bad name: %v", err)
	}

	// A host the controller does not know gets no session, and is told what to do.
	stranger := &Host{Config: other, Log: quiet}
	if up, why := stranger.connect(ctx); up || !strings.Contains(why, "run the invite line again") {
		t.Fatalf("stranger: %v %q", up, why)
	}
}

func TestRotatedTokenKeepsJoinedHosts(t *testing.T) {
	s, hc, token := controller(t)
	ctx := context.Background()
	if err := hc.Register(ctx, token, false); err != nil {
		t.Fatal(err)
	}
	fresh, err := RotateToken(s.Dir)
	if err != nil || fresh == token {
		t.Fatal(err, fresh)
	}
	run(t, &Host{Config: hc})
	waitUp(t, s.Peer(hc.ID)) // by its own secret
	if err := hc.Register(ctx, "", true); err != nil {
		t.Fatalf("a joined host asking by its secret: %v", err)
	}
	other := hc
	other.ID, other.Secret = NewHostID()
	other.Name = "desk2"
	if err := other.Register(ctx, token, false); err == nil {
		t.Fatal("the old token still works")
	}
	if err := other.Register(ctx, fresh, false); err != nil {
		t.Fatal(err)
	}
}

func TestLeaveForgetsTheHost(t *testing.T) {
	s, hc, token := controller(t)
	ctx := context.Background()
	if err := hc.Register(ctx, token, false); err != nil {
		t.Fatal(err)
	}
	stop := run(t, &Host{Config: hc})
	p := s.Peer(hc.ID)
	waitUp(t, p)
	stop()
	if err := hc.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.Members()) != 0 || len(ReadMembers(s.Dir)) != 0 {
		t.Fatal("still a member")
	}
	if up, why := (&Host{Config: hc, Log: quiet}).connect(ctx); up || !strings.Contains(why, "not known") {
		t.Fatalf("after leave: %v %q", up, why)
	}
}

func TestAnotherBuildSwitchesOrSaysWhyNot(t *testing.T) {
	s, hc, token := controller(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(s.Dist, "pumasi-linux-amd64"), []byte("the controller's build"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := FileVersion(filepath.Join(s.Dist, "pumasi-linux-amd64"))
	if err := hc.Register(ctx, token, false); err != nil {
		t.Fatal(err)
	}
	p := s.Peer(hc.ID)

	// The host fetches the build, checks it and carries on as that build.
	var got strings.Builder
	h := &Host{Config: hc, Version: "old", Arch: "amd64"}
	h.Upgrade = func(ctx context.Context, w string) (string, error) {
		if err := hc.Download(ctx, "amd64", &got); err != nil {
			return "", err
		}
		return w, nil
	}
	stop := run(t, h)
	waitUp(t, p)
	if ln := p.Link(); ln.Version != want || ln.Stay != "" || got.String() != "the controller's build" {
		t.Fatalf("link %+v, downloaded %q", ln, got.String())
	}
	stop()
	eventually(t, "drop", func() bool { return !p.Link().Connected })

	// It cannot switch: it stays on its build, connected, and the controller knows why.
	h = &Host{Config: hc, Version: "old", Arch: "amd64"}
	h.Upgrade = func(context.Context, string) (string, error) { return "", errors.New("disk full") }
	run(t, h)
	waitUp(t, p)
	if ln := p.Link(); ln.Version != "old" || !strings.Contains(ln.Stay, "disk full") {
		t.Fatalf("link %+v", ln)
	}
	// A host of an arch the controller has no build for is left alone.
	if s.wanted("arm64") != "" {
		t.Fatal("no arm64 build, yet a version is wanted")
	}
}

func TestScriptAndDownloads(t *testing.T) {
	s, hc, _ := controller(t)
	if err := os.WriteFile(filepath.Join(s.Dist, "pumasi-linux-arm64"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) (int, string) {
		resp, err := hc.client().Get("https://" + hc.Controller + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, script := fetch("/join.sh")
	if code != 200 || !strings.Contains(script, "controller='"+hc.Controller+"'") || !strings.Contains(script, "pin='"+hc.Pin+"'") {
		t.Fatalf("script %d:\n%s", code, script)
	}
	if code, body := fetch("/dl/pumasi-linux-arm64"); code != 200 || body != "bin" {
		t.Fatalf("%d %q", code, body)
	}
	if code, body := fetch("/dl/pumasi-linux-arm64.sha256"); code != 200 || strings.TrimSpace(body) != FileVersion(filepath.Join(s.Dist, "pumasi-linux-arm64")) {
		t.Fatalf("%d %q", code, body)
	}
	for _, p := range []string{"/dl/pumasi-linux-amd64", "/dl/join.key", "/dl/..%2fjoin.key", "/dl/hosts.json"} {
		if code, _ := fetch(p); code != 404 {
			t.Fatalf("%s: %d", p, code)
		}
	}
}

// The invite line is curl's: the pin has to be in the form curl checks.
func TestCurlAcceptsThePinAndRejectsAnother(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	_, hc, _ := controller(t)
	url := "https://" + hc.Controller + "/join.sh"
	out, err := exec.Command(curl, "-fsSk", "--pinnedpubkey", hc.Pin, url).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "pin='"+hc.Pin+"'") {
		t.Fatalf("curl with the pin: %v\n%s", err, out)
	}
	out, err = exec.Command(curl, "-fsSk", "--pinnedpubkey", "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", url).CombinedOutput()
	if err == nil {
		t.Fatalf("curl took a server with another key:\n%s", out)
	}
	// The script is valid sh.
	if out, err := exec.Command("sh", "-n", "join.sh").CombinedOutput(); err != nil {
		t.Fatalf("sh -n join.sh: %v\n%s", err, out)
	}
}
