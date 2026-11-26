package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/join"
)

// runInvite prints the line that joins a host to this controller. Every
// invite is a token of its own.
func runInvite(args []string) error {
	fs := flag.NewFlagSet("pumasi invite", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	uses := fs.Int("uses", join.InviteUses, "how many hosts may join with this line")
	ttl := fs.Duration("ttl", join.InviteTTL, "how long the line works")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return err
	}
	id, err := join.LoadIdentity(cfg.StateDir())
	if err != nil {
		return err
	}
	token, err := join.NewToken(cfg.StateDir(), "", *uses, *ttl)
	if err != nil {
		return err
	}
	addrs := join.Advertised(cfg.Listen)
	fmt.Print(inviteText(id.Pin, addrs[0], token, *uses, *ttl))
	if len(addrs) > 1 {
		fmt.Printf("This machine also has %s; use the address the host can reach.\n", strings.Join(addrs[1:], ", "))
	}
	if controller.DashboardURL(cfg) == "" {
		fmt.Println("No `pumasi up` is running here: a host can join only while it runs.")
	}
	return nil
}

// inviteText is what `pumasi invite` prints: one line per kind of shell.
func inviteText(pin, addr, token string, uses int, ttl time.Duration) string {
	hosts := "one host"
	if uses > 1 {
		hosts = fmt.Sprintf("%d hosts", uses)
	}
	return fmt.Sprintf(`Run this on the host that should join. It works for %[4]s, until %[5]s.

Linux, macOS:

  curl -fsSk --pinnedpubkey '%[1]s' https://%[2]s/join.sh | PUMASI_TOKEN=%[3]s sh -s --

Windows (PowerShell, in a window on that machine's desktop):

  $env:PUMASI_TOKEN='%[3]s'; & ([scriptblock]::Create((curl.exe -fsSk --pinnedpubkey '%[1]s' https://%[2]s/join.ps1 | Out-String)))

curl's -k is safe here: --pinnedpubkey accepts only this controller's key, whatever the certificate says.
The host lends half of its cpus and memory under its hostname; to say otherwise, add
--name NAME, --cpus N or --mem 6G at the end. A Mac, a Windows machine and a Linux machine
with a screen session join as a desktop (someone works on it: theirs first); --server or
--desktop says otherwise. On macOS and Windows runners are processes of the user who ran
the line, not containers.
`, pin, addr, token, hosts, time.Now().Add(ttl).Format("15:04"))
}

// Where a joined host keeps its things. Everything pumasi puts on a host is
// the binary, this directory and the service.
type hostPaths struct {
	dir   string // ~/.cache/pumasi
	conf  string // join.json: controller, pin, this host's id and secret
	state string // join.state: connected or not, written by the agent
	pid   string // join.pid: the process that restarts the agent, where no service manager does
	log   string
}

// cacheDir is ~/.cache on every OS (not ~/Library/Caches or %LocalAppData%),
// or what XDG_CACHE_HOME says on Linux.
func cacheDir() string {
	if runtime.GOOS == "linux" {
		if d, err := os.UserCacheDir(); err == nil {
			return d
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache")
}

// installedBin is where the join script puts the binary.
func installedBin() string {
	home, _ := os.UserHomeDir()
	name := "pumasi"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, ".local", "bin", name)
}

// runningInstalled reports whether this process is the installed binary.
func runningInstalled() bool {
	exe, err := os.Executable()
	return err == nil && filepath.Clean(exe) == installedBin()
}

func paths() hostPaths {
	d := filepath.Join(cacheDir(), "pumasi")
	return hostPaths{dir: d, conf: filepath.Join(d, "join.json"), state: filepath.Join(d, "join.state"),
		pid: filepath.Join(d, "join.pid"), log: filepath.Join(d, "agent.log")}
}

type linkState struct {
	Connected bool      `json:"connected"`
	At        time.Time `json:"at"`
	Why       string    `json:"why,omitempty"`
}

func readLinkState(p hostPaths) (linkState, bool) {
	var s linkState
	b, err := os.ReadFile(p.state)
	return s, err == nil && json.Unmarshal(b, &s) == nil
}

var hostNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// runJoin attaches this host to a controller: check, register, run as a service.
func runJoin(args []string) error {
	hostname, _ := os.Hostname()
	fs := flag.NewFlagSet("pumasi join", flag.ContinueOnError)
	name := fs.String("name", strings.Trim(hostNameRE.ReplaceAllString(hostname, "-"), "-."), "the host's name at the controller")
	cpus := fs.Float64("cpus", 0, "cpus to lend (default: half of this machine)")
	memStr := fs.String("mem", "", "memory to lend, e.g. 6G (default: half of this machine)")
	desktop := fs.Bool("desktop", false, "someone works on this machine: it is filled after the servers and takes no new runners while they are at it")
	server := fs.Bool("server", false, "nobody works on this machine")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The token comes in the environment, not as an argument: arguments show in ps.
	token := os.Getenv("PUMASI_TOKEN")
	os.Unsetenv("PUMASI_TOKEN") // the agent and its runners do not inherit it
	if fs.NArg() != 2 || token == "" || (*desktop && *server) {
		return errors.New("usage: PUMASI_TOKEN=<token> pumasi join [--name NAME] [--cpus N] [--mem 6G] [--desktop|--server] <controller> <pin>\n(`pumasi invite` on the controller prints the whole line)")
	}
	p := paths()
	hc, err := join.LoadHostConfig(p.conf)
	first := err != nil || hc.ID == ""
	if first {
		hc.ID, hc.Secret = join.NewHostID() // a host that joins again keeps who it is
	}
	hc.Controller, hc.Pin = fs.Arg(0), fs.Arg(1)
	hc.Name, hc.CPUs = *name, *cpus
	kindWhy := ""
	switch {
	case *desktop:
		hc.Kind = "desktop"
	case *server:
		hc.Kind = "server"
	default:
		hc.Kind, kindWhy = defaultKind()
	}
	if hc.Mem, err = config.ParseBytes(*memStr); err != nil {
		return fmt.Errorf("--mem: %w", err)
	}
	if hc.CPUs == 0 {
		hc.CPUs = max(1, float64(runtime.NumCPU()/2))
	}
	if hc.Mem == 0 {
		_, meter := agent.Platform("pumasi", p.dir)
		hu, err := meter.Host()
		if err != nil || hu.MemBytes <= 0 {
			return fmt.Errorf("cannot read this machine's memory (%v); pass --mem", err)
		}
		hc.Mem = hu.MemBytes / 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !preflight(ctx, os.Stdout, hc, token) {
		if first && runningInstalled() {
			removeBinary() // the line fetches it again; a host that did not join keeps nothing
		}
		return errors.New("not joined: fix the FAIL lines and run the invite line again")
	}
	if err := hc.Register(ctx, token, false); err != nil {
		return fmt.Errorf("the controller did not take the host: %w", err)
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return err
	}
	if err := hc.Save(p.conf); err != nil {
		return err
	}
	_ = os.Remove(p.state)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	how, note, err := installService(p, exe)
	if err != nil {
		return fmt.Errorf("registered, but the agent could not be started: %w", err)
	}
	st, ok := linkState{}, false
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if st, ok = readLinkState(p); ok && st.Connected {
			break
		}
	}
	fmt.Println()
	if !ok || !st.Connected {
		why := "the agent has not reported yet"
		if ok {
			why = st.Why
		}
		return fmt.Errorf("registered as %s, but the agent is not connected: %s\nsee %s, then `pumasi doctor`", hc.Name, why, p.log)
	}
	fmt.Printf("joined as %s (%s): lends %g cpus and %s, runs as %s\n", hc.Name, hc.Kind, hc.CPUs, gib(hc.Mem), how)
	if kindWhy != "" {
		fmt.Println(kindWhy)
	}
	if note != "" {
		fmt.Println(note)
	}
	fmt.Println("`pumasi doctor` checks the connection, `pumasi leave` removes everything.")
	return nil
}

// defaultKind is what a host is when the line does not say: a machine someone
// sits at is a desktop. It also returns the line that tells the person so.
func defaultKind() (kind, why string) {
	const server = "it joined as a server: runners are placed whenever there is room; if someone works on this machine, run the line again with --desktop"
	const desktop = "it joined as a desktop (%s): it takes no new runners while someone is at it; if nobody works here, run the line again with --server"
	switch runtime.GOOS {
	case "darwin":
		return "desktop", fmt.Sprintf(desktop, "a Mac")
	case "windows":
		return "desktop", fmt.Sprintf(desktop, "a Windows machine")
	}
	if screen := screenSession(); screen != "" {
		return "desktop", fmt.Sprintf(desktop, "it has a screen session, "+screen)
	}
	return "server", server
}

// screenSession names a graphical session on this Linux machine ("" = none):
// an X or Wayland socket. WSL with its desktop integration has one too, and a
// WSL host is someone's desktop.
func screenSession() string {
	for _, pat := range []string{"/tmp/.X11-unix/X*", filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "wayland-*"), "/run/user/*/wayland-*", "/mnt/wslg/runtime-dir/wayland-*"} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// preflight checks one thing after the other and names what failed. token ""
// asks as a host that has already joined.
func preflight(ctx context.Context, w io.Writer, hc join.HostConfig, token string) bool {
	all := true
	say := func(ok bool, format string, a ...any) bool {
		mark := "ok   "
		if !ok {
			mark, all = "FAIL ", false
		}
		fmt.Fprintf(w, "%s %s\n", mark, fmt.Sprintf(format, a...))
		return ok
	}
	network := func() {
		host, _, err := net.SplitHostPort(hc.Controller)
		if err != nil {
			say(false, "controller address %q: want host:port", hc.Controller)
			return
		}
		if net.ParseIP(host) == nil {
			if _, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
				say(false, "resolve %s: this host's DNS does not know the name — use the controller's IP address in the invite line", host)
				return
			}
		}
		say(true, "resolve %s", host)
		began := time.Now()
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", hc.Controller)
		if err != nil {
			say(false, "tcp %s: %s — is `pumasi up` running there, and can this host reach that address and port (firewall, VPN)?", hc.Controller, tcpWhy(err))
			return
		}
		conn.Close()
		say(true, "tcp %s (%s)", hc.Controller, time.Since(began).Round(time.Millisecond))
		if err := hc.CheckPin(ctx); err != nil {
			if errors.Is(err, join.ErrPin) {
				say(false, "controller certificate: %s shows another key than the invite — copy the line from `pumasi invite` again", hc.Controller)
			} else {
				say(false, "controller certificate: no TLS answer from %s (%v) — is that port really pumasi's listen: port?", hc.Controller, err)
			}
			return
		}
		say(true, "controller certificate matches the invite")
		err = hc.Register(ctx, token, true)
		var ref *join.Refused
		switch {
		case err == nil && token == "":
			say(true, "the controller knows this host as %s", hc.Name)
		case err == nil:
			say(true, "token accepted, the name %s is free", hc.Name)
		case errors.As(err, &ref) && ref.Code == 401 && token == "":
			say(false, "membership: %s — run the invite line again (`pumasi invite` on the controller)", ref.Why)
		case errors.As(err, &ref) && ref.Code == 401:
			say(false, "token: %s — run `pumasi invite` on the controller again", ref.Why)
		case errors.As(err, &ref) && ref.Code == 409:
			say(false, "name: %s", ref.Why)
		default:
			say(false, "controller: %v", err)
		}
	}
	network()
	note := func(format string, a ...any) { fmt.Fprintf(w, "note  %s\n", fmt.Sprintf(format, a...)) }
	preflightHost(ctx, say, note)
	return all
}

// runners lists pumasi's runners on this host (containers on Linux,
// processes elsewhere): all of them, or only those that are up.
func runners(ctx context.Context, p hostPaths, all bool) []string {
	drv, _ := agent.Platform("pumasi", p.dir)
	cs, _ := drv.List(ctx)
	var names []string
	for _, c := range cs {
		if all || c.State == "running" {
			names = append(names, c.Name)
		}
	}
	return names
}

func removeRunners(ctx context.Context, p hostPaths) (int, error) {
	drv, _ := agent.Platform("pumasi", p.dir)
	cs, err := drv.List(ctx)
	if err != nil {
		return 0, err
	}
	for _, c := range cs {
		if err := drv.Remove(ctx, c.Name); err != nil {
			return 0, fmt.Errorf("remove runner %s: %w", c.Name, err)
		}
	}
	// Native runners of a class with driver: native live in their own scopes.
	scopes := sweepScopes()
	if c, ok := drv.(interface{ Clean(context.Context) }); ok {
		c.Clean(ctx) // what the driver made besides runners (Linux: the docker: inside image)
	}
	// The runners' own place, outside p.dir. What cannot be deleted there
	// (files of root that a job left) stays, and stays said.
	root := agent.RunnerRoot(p.dir)
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "pumasi: %s is not removed (%v); delete it by hand\n", root, err)
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(filepath.Dir(root)) // C:\pm, when no other user has runners there
	}
	return len(cs) + scopes, nil
}

func tcpWhy(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "no answer (timeout)"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

func lastLineOf(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// runDoctor repeats join's checks on a joined host and adds how the agent is doing.
func runDoctor(args []string) error {
	if err := flag.NewFlagSet("pumasi doctor", flag.ContinueOnError).Parse(args); err != nil {
		return err
	}
	p := paths()
	hc, err := join.LoadHostConfig(p.conf)
	if err != nil {
		return fmt.Errorf("this host has not joined a controller (%s is missing); `pumasi invite` on the controller prints the line to run here", p.conf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ok := preflight(ctx, os.Stdout, hc, "")
	mark := func(good bool) string {
		if good {
			return "ok   "
		}
		ok = false
		return "FAIL "
	}
	running, how := serviceRunning(p)
	if running {
		fmt.Printf("%s agent runs as %s\n", mark(true), how)
	} else {
		fmt.Printf("%s agent: not running (%s) — run the invite line again to start it; its log is %s\n", mark(false), how, p.log)
	}
	st, have := readLinkState(p)
	switch {
	case have && st.Connected:
		fmt.Printf("%s connected to %s since %s\n", mark(true), hc.Controller, st.At.Local().Format("01-02 15:04:05"))
	case have:
		fmt.Printf("%s not connected since %s: %s\n", mark(false), st.At.Local().Format("01-02 15:04:05"), st.Why)
	default:
		fmt.Printf("%s the agent has not reported a connection yet\n", mark(false))
	}
	if !ok {
		return errors.New("something is wrong on this host (see FAIL above)")
	}
	return nil
}

// runLeave removes this host from its controller and everything pumasi put here.
func runLeave(args []string) error {
	fs := flag.NewFlagSet("pumasi leave", flag.ContinueOnError)
	force := fs.Bool("force", false, "also stop runners that are running jobs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !*force {
		if up := runners(ctx, p, false); len(up) > 0 {
			return fmt.Errorf("%d runners are still up and may be running jobs:\n  %s\nnothing was removed; `pumasi leave --force` stops them too",
				len(up), strings.Join(up, "\n  "))
		}
	}
	// The controller first: a host that says goodbye is not reported as lost.
	told := "the controller was not told (this host had no join.json)"
	if hc, err := join.LoadHostConfig(p.conf); err == nil {
		told = "the controller forgot this host"
		if err := hc.Leave(ctx); err != nil {
			told = fmt.Sprintf("the controller could not be told (%v); it shows %s as unreachable until someone runs `pumasi down` there", err, hc.Name)
		}
	}
	stopService(p, true)
	n, err := removeRunners(ctx, p)
	if err != nil {
		return err
	}
	fmt.Printf("left: agent stopped, %d runners removed, %s; %s\n", n, removeFiles(p), told)
	return nil
}
