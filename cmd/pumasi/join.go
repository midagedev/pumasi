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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/cgroup"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/join"
)

// runInvite prints the line that joins a host to this controller.
func runInvite(args []string) error {
	fs := flag.NewFlagSet("pumasi invite", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	rotate := fs.Bool("rotate", false, "make a new token; hosts that already joined stay")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Listen == "" {
		return fmt.Errorf("%s has no listen: address; add e.g. `listen: 0.0.0.0:7411` and restart `pumasi up`", *cfgPath)
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return err
	}
	id, err := join.LoadIdentity(cfg.StateDir())
	if err != nil {
		return err
	}
	token, err := join.Token(cfg.StateDir())
	if *rotate {
		token, err = join.RotateToken(cfg.StateDir())
	}
	if err != nil {
		return err
	}
	addr, others := advertised(cfg.Listen)
	fmt.Printf("Run this on the host that should join:\n\n  curl -fsSk --pinnedpubkey '%s' https://%s/join.sh | sh -s -- %s\n\n", id.Pin, addr, token)
	fmt.Println("The host lends half of its cpus and memory under its hostname; to say otherwise, add")
	fmt.Println("--name NAME, --cpus N, --mem 6G or --desktop (someone works on it) after the token.")
	if len(others) > 0 {
		fmt.Printf("This machine also has %s; use the address the host can reach.\n", strings.Join(others, ", "))
	}
	return nil
}

// advertised is the address hosts should dial for a listen address, and this
// machine's other addresses when the listen address names none.
func advertised(listen string) (string, []string) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen, nil
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return listen, nil
	}
	var all []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				all = append(all, n.IP.String())
			}
		}
	}
	// The address of the default route; no packet is sent.
	first := ""
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		first = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	} else if len(all) > 0 {
		first = all[0]
	} else {
		first = "127.0.0.1"
	}
	var others []string
	for _, a := range all {
		if a != first {
			others = append(others, a)
		}
	}
	return net.JoinHostPort(first, port), others
}

// Where a joined host keeps its things. Everything pumasi puts on a host is
// the binary, this directory and the service.
type hostPaths struct {
	dir   string // ~/.cache/pumasi
	conf  string // join.json: controller, pin, this host's id and secret
	state string // join.state: connected or not, written by the agent
	pid   string // join.pid: the restart loop, on hosts without a usable systemd
	log   string
}

func paths() hostPaths {
	cache, _ := os.UserCacheDir()
	d := filepath.Join(cache, "pumasi")
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
	desktop := fs.Bool("desktop", false, "someone works on this machine: it is filled after the servers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 3 {
		return errors.New("usage: pumasi join [--name NAME] [--cpus N] [--mem 6G] [--desktop] <controller> <pin> <token>\n(`pumasi invite` on the controller prints the whole line)")
	}
	p := paths()
	hc, err := join.LoadHostConfig(p.conf)
	if err != nil || hc.ID == "" {
		hc.ID, hc.Secret = join.NewHostID() // first time; a host that joins again keeps who it is
	}
	hc.Controller, hc.Pin = fs.Arg(0), fs.Arg(1)
	token := fs.Arg(2)
	hc.Name, hc.Kind, hc.CPUs = *name, "server", *cpus
	if *desktop {
		hc.Kind = "desktop"
	}
	if hc.Mem, err = config.ParseBytes(*memStr); err != nil {
		return fmt.Errorf("--mem: %w", err)
	}
	if hc.CPUs == 0 {
		hc.CPUs = max(1, float64(runtime.NumCPU()/2))
	}
	if hc.Mem == 0 {
		mi, err := cgroup.DefaultReader().Meminfo()
		if err != nil {
			return fmt.Errorf("cannot read this machine's memory (%w); pass --mem", err)
		}
		hc.Mem = int64(mi.Total) / 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !preflight(ctx, os.Stdout, hc, token) {
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
	if note != "" {
		fmt.Println(note)
	}
	fmt.Println("`pumasi doctor` checks the connection, `pumasi leave` removes everything.")
	return nil
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
			say(false, "token: the controller refused it (%s) — run `pumasi invite` again", ref.Why)
		case errors.As(err, &ref) && ref.Code == 409:
			say(false, "name: %s", ref.Why)
		default:
			say(false, "controller: %v", err)
		}
	}
	network()

	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	text := strings.TrimSpace(string(out))
	switch {
	case errors.Is(err, exec.ErrNotFound):
		say(false, "docker: not installed (no docker in PATH) — runners are Docker containers")
	case err != nil && strings.Contains(text, "permission denied"):
		say(false, "docker: this user may not use it — add the user to the docker group and log in again")
	case err != nil:
		say(false, "docker: the daemon does not answer (%s)", lastLineOf(text))
	default:
		say(true, "docker %s reachable as this user", text)
	}

	ctrl, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	have := strings.Fields(string(ctrl))
	switch {
	case err != nil:
		say(false, "cgroup v2: /sys/fs/cgroup is not a cgroup v2 mount — pumasi measures and limits runners through cgroup v2")
	case !contains(have, "memory") || !contains(have, "cpu"):
		say(false, "cgroup v2: the memory and cpu controllers are not both enabled (have: %s)", strings.Join(have, " "))
	default:
		say(true, "cgroup v2 with memory and cpu controllers")
	}
	return all
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
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
		if up := pumasiContainers(ctx, false); len(up) > 0 {
			return fmt.Errorf("%d runners are still up and may be running jobs:\n  %s\nnothing was removed; `pumasi leave --force` stops them too",
				len(up), strings.Join(up, "\n  "))
		}
	}
	hc, cerr := join.LoadHostConfig(p.conf)
	stopService(p, true)
	n, err := removeContainers(ctx)
	if err != nil {
		return err
	}
	told := "the controller was not told (this host had no join.json)"
	if cerr == nil {
		told = "the controller forgot this host"
		if err := hc.Leave(ctx); err != nil {
			told = fmt.Sprintf("the controller could not be told (%v); it keeps %s as unreachable until `pumasi down --force`", err, hc.Name)
		}
	}
	removeFiles(p)
	fmt.Printf("left: agent stopped, %d containers removed, %s and the binary removed; %s\n", n, p.dir, told)
	return nil
}
