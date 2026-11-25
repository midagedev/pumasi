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
	token, _ := NewToken(dir, "", 100, time.Hour) // most tests register more than once
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
	if !errors.As(err, &ref) || ref.Code != http.StatusUnauthorized || !strings.HasPrefix(ref.Why, "the controller does not know this token") {
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

// A token admits one host within its time, and a host that joined needs none.
func TestTokenAdmitsOneHostWithinItsTime(t *testing.T) {
	s, hc, _ := controller(t)
	ctx := context.Background()
	token, err := NewToken(s.Dir, "", 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := hc.Register(ctx, token, true); err != nil {
		t.Fatalf("asking does not use it up: %v", err)
	}
	if err := hc.Register(ctx, token, false); err != nil {
		t.Fatal(err)
	}
	run(t, &Host{Config: hc})
	waitUp(t, s.Peer(hc.ID)) // by its own secret
	if err := hc.Register(ctx, "", true); err != nil {
		t.Fatalf("a joined host asking by its secret: %v", err)
	}
	other := hc
	other.ID, other.Secret = NewHostID()
	other.Name = "desk2"
	var ref *Refused
	if err := other.Register(ctx, token, false); !errors.As(err, &ref) || ref.Code != http.StatusUnauthorized {
		t.Fatalf("a second host with a token for one: %v", err)
	}
	old, _ := NewToken(s.Dir, "", 1, time.Nanosecond)
	time.Sleep(time.Millisecond)
	if err := other.Register(ctx, old, false); !errors.As(err, &ref) || !strings.Contains(ref.Why, "the token ended at") {
		t.Fatalf("a token past its time: %v", err)
	}
	two, _ := NewToken(s.Dir, "", 2, time.Hour)
	third := other
	third.ID, third.Secret = NewHostID()
	third.Name = "desk3"
	if err := errors.Join(other.Register(ctx, two, false), third.Register(ctx, two, false)); err != nil {
		t.Fatalf("a token for two: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(tokenDir(s.Dir), "*.json")); len(files) != 1 { // the test's own
		t.Fatalf("spent tokens left behind: %v", files)
	}
}

// The controller's own token for a host of its config: it admits that name
// only, takes a reserved name, and replaces the member that had it.
func TestTokenForOneName(t *testing.T) {
	s, hc, any := controller(t)
	ctx := context.Background()
	s.Reserved = func(n string) bool { return n == "server1" }
	hc.Name = "server1"
	var ref *Refused
	if err := hc.Register(ctx, any, false); !errors.As(err, &ref) || ref.Code != http.StatusConflict {
		t.Fatalf("an invite taking a name of the config: %v", err)
	}
	bound, _ := NewToken(s.Dir, "server1", 1, time.Hour)
	other := hc
	other.Name = "desk9"
	if err := other.Register(ctx, bound, false); !errors.As(err, &ref) || !strings.Contains(ref.Why, "for the host named server1") {
		t.Fatalf("the token under another name: %v", err)
	}
	if err := hc.Register(ctx, bound, false); err != nil {
		t.Fatal(err)
	}
	// The machine lost its files and joins again as someone new.
	again := hc
	again.ID, again.Secret = NewHostID()
	bound, _ = NewToken(s.Dir, "server1", 1, time.Hour)
	if err := again.Register(ctx, bound, false); err != nil {
		t.Fatal(err)
	}
	if ms := s.Members(); len(ms) != 1 || ms[0].ID != again.ID {
		t.Fatalf("members %+v", ms)
	}
}

// The Host header ends up in the script: only an address gets one.
func TestScriptRefusesAHostHeaderThatIsNotAnAddress(t *testing.T) {
	_, hc, _ := controller(t)
	c := hc.client()
	for host, want := range map[string]int{"10.0.0.5:7411": 200, "ctl.example.com": 200, "[fe80::1]:7411": 200, "x'; rm -rf ~; '": 400, "a b": 400} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+hc.Controller+"/join.sh", nil)
		req.Host = host
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %q: %d, want %d", host, resp.StatusCode, want)
		}
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
		if err := hc.Download(ctx, "linux", "amd64", &got); err != nil {
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
	if s.wanted("linux", "arm64") != "" {
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
	for _, p := range []string{"/dl/pumasi-linux-amd64", "/dl/join.key", "/dl/..%2fjoin.key", "/dl/hosts.json",
		"/dl/pumasi-windows-amd64", "/dl/pumasi-linux-arm64.exe", "/dl/pumasi-plan9-amd64"} {
		if code, _ := fetch(p); code != 404 {
			t.Fatalf("%s: %d", p, code)
		}
	}

	// Every system asks for its own build under its own name.
	for _, name := range []string{"pumasi-darwin-arm64", "pumasi-windows-amd64.exe"} {
		if err := os.WriteFile(filepath.Join(s.Dist, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if code, body := fetch("/dl/" + name); code != 200 || body != name {
			t.Fatalf("%s: %d %q", name, code, body)
		}
		if code, body := fetch("/dl/" + name + ".sha256"); code != 200 || strings.TrimSpace(body) != FileVersion(filepath.Join(s.Dist, name)) {
			t.Fatalf("%s.sha256: %d %q", name, code, body)
		}
	}
	code, ps := fetch("/join.ps1")
	if code != 200 || !strings.Contains(ps, "$controller = '"+hc.Controller+"'") || !strings.Contains(ps, "$pin = '"+hc.Pin+"'") ||
		!strings.Contains(ps, "pumasi-windows-amd64.exe") {
		t.Fatalf("ps1 %d:\n%s", code, ps)
	}
	for _, build := range []string{"linux-amd64", "linux-arm64", "darwin-arm64", "darwin-amd64"} {
		if !strings.Contains(script, "build="+build+" ;;") {
			t.Fatalf("join.sh does not know %s", build)
		}
	}
}

func TestDistName(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"linux", "amd64"}: "pumasi-linux-amd64", {"darwin", "arm64"}: "pumasi-darwin-arm64",
		{"windows", "amd64"}: "pumasi-windows-amd64.exe", {"plan9", "amd64"}: "", {"linux", "../x"}: "", {"", "amd64"}: "",
	} {
		if got := DistName(in[0], in[1]); got != want {
			t.Errorf("DistName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// A darwin host must not be handed the linux build of the same arch.
func TestBuildIsMatchedPerSystem(t *testing.T) {
	s, hc, token := controller(t)
	ctx := context.Background()
	for _, name := range []string{"pumasi-linux-arm64", "pumasi-darwin-arm64"} {
		if err := os.WriteFile(filepath.Join(s.Dist, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := hc.Register(ctx, token, false); err != nil {
		t.Fatal(err)
	}
	mac := FileVersion(filepath.Join(s.Dist, "pumasi-darwin-arm64"))
	var asked string
	h := &Host{Config: hc, Version: mac, OS: "darwin", Arch: "arm64"}
	h.Upgrade = func(_ context.Context, w string) (string, error) { asked = w; return w, nil }
	stop := run(t, h)
	waitUp(t, s.Peer(hc.ID))
	if asked != "" {
		t.Fatalf("a host on the controller's darwin build was told to switch to %s", asked)
	}
	stop()
	eventually(t, "drop", func() bool { return !s.Peer(hc.ID).Link().Connected })

	// The same host without the header is a linux host, as before other systems could join.
	h = &Host{Config: hc, Version: mac, Arch: "arm64"}
	h.Upgrade = func(_ context.Context, w string) (string, error) { asked = w; return w, nil }
	run(t, h)
	waitUp(t, s.Peer(hc.ID))
	if asked != FileVersion(filepath.Join(s.Dist, "pumasi-linux-arm64")) {
		t.Fatalf("asked to switch to %q", asked)
	}
}

// The controller reads a connection the host closed as "agent exited"; the
// host that closed it for another reason says so when it is back.
func TestHostSaysWhyItDialedAgain(t *testing.T) {
	p := &Peer{up: make(chan struct{})}
	p.why = WhyExited
	p.attach(nil, "v", "", WhyKeepalive, "")
	if got := p.Link().Why; got != WhyKeepalive {
		t.Fatalf("why = %q", got)
	}
	// A new agent process has nothing to say: the controller's reading stands.
	p = &Peer{up: make(chan struct{})}
	p.why = WhyExited
	p.attach(nil, "v", "", "", "")
	if got := p.Link().Why; got != WhyExited {
		t.Fatalf("why = %q", got)
	}
	// The host knows more about a timeout both sides saw.
	p = &Peer{up: make(chan struct{})}
	p.why = WhyKeepalive
	p.attach(nil, "v", "", "the controller closed the connection (the host was asleep for 5m0s)", "the host was asleep for 5m0s")
	if got := p.Link().Why; got != WhyKeepalive+" (the host was asleep for 5m0s)" {
		t.Fatalf("why = %q", got)
	}
	// What the controller saw itself is not overwritten.
	p = &Peer{up: make(chan struct{})}
	p.why = WhyKeepalive
	p.attach(nil, "v", "", "the controller closed the connection", "")
	if got := p.Link().Why; got != WhyKeepalive {
		t.Fatalf("why = %q", got)
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

func TestWatchdogNotesWhatJustHappened(t *testing.T) {
	w := &Watchdog{}
	t0 := time.Now()
	w.saw(t0, t0.Add(time.Second)) // an ordinary tick
	if got := w.Note(); got != "" {
		t.Fatalf("nothing happened, yet %q", got)
	}
	w.sawClocks(15*time.Second, 15*time.Second, time.Now()) // the process did not run for 14s
	if got := w.Note(); !strings.Contains(got, "did not get to run for 14s") {
		t.Fatalf("stall: %q", got)
	}
	// Asleep: the wall clock moved 5 minutes, the monotonic clock one tick.
	w.sawClocks(time.Second, 5*time.Minute+time.Second, time.Now())
	if got := w.Note(); got != "the host was asleep for 5m0s" {
		t.Fatalf("sleep: %q", got)
	}
	// A gap from a while ago does not explain a connection that ends now.
	w.sawClocks(15*time.Second, 15*time.Second, time.Now().Add(-20*time.Second))
	if got := w.Note(); got != "" {
		t.Fatalf("stale: %q", got)
	}
	var none *Watchdog
	if got := none.Note(); got != "" {
		t.Fatalf("no watchdog: %q", got)
	}
}
