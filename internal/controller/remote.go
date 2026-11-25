package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
)

// sshLink: the controller goes to the host. It uploads and starts the agent
// over ssh and talks to it through a port forward.
type sshLink struct {
	h   config.Host
	tun *SSHTunnel
	api *api.Client
}

func newSSHLink(h config.Host, log *slog.Logger) *sshLink {
	tun := &SSHTunnel{Dest: h.SSH, RemotePort: h.AgentPort, Log: log}
	return &sshLink{h: h, tun: tun, api: &api.Client{BaseURL: tun.BaseURL, HTTP: &http.Client{Timeout: 90 * time.Second}}}
}

func (l *sshLink) Agent() *api.Client { return l.api }
func (l *sshLink) Via() string        { return "ssh" }
func (l *sshLink) Why() string        { return "" }
func (l *sshLink) Close()             { l.tun.Close() }

func (l *sshLink) Ensure(ctx context.Context, distDir string) (string, error) {
	return ensureAgent(ctx, l.h, distDir)
}

func (l *sshLink) Revive(ctx context.Context, distDir string) (string, error) {
	return ensureAgent(ctx, l.h, distDir)
}

func (l *sshLink) Running(ctx context.Context) ([]string, error) { return runningContainers(ctx, l.h) }

func (l *sshLink) Clean(ctx context.Context) (string, error) {
	// A runner is best ended by its agent: of a process tree it knows the
	// processes that left the runner's group. The script below gets the rest.
	byAgent := 0
	if snap, err := l.api.Snapshot(ctx); err == nil {
		for _, r := range snap.Runners {
			if _, err := l.api.Delete(ctx, r.Name); err == nil {
				byAgent++
			}
		}
	}
	out, err := cleanHost(ctx, l.h)
	n, _ := strconv.Atoi(lastLine(out))
	return strconv.Itoa(n + byAgent), err
}

// Everything pumasi puts on a host lives here: the agent binary, its pid file and its log.
const remoteDir = `"$HOME/.cache/pumasi"`

// runRemote pipes a script to the host's shell over ssh and returns its output.
// The script always travels on stdin: an ssh command line would be re-parsed by
// whatever shell the host gives ssh (PowerShell on a Windows host).
func runRemote(ctx context.Context, h config.Host, script string) (string, error) {
	args := append([]string{"-o", "BatchMode=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none", h.SSH},
		strings.Fields(h.Shell)...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	// Own process group: a terminal Ctrl-C must not kill the cleanup ssh.
	detach(cmd)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	// A Windows hop reports its own errors in UTF-16; dropping NULs keeps them readable.
	text := strings.TrimSpace(strings.NewReplacer("\r", "", "\x00", "").Replace(string(out)))
	if err != nil {
		return text, fmt.Errorf("ssh %s: %w: %s", h.SSH, err, lastLine(text))
	}
	return text, nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// stopAgentScript stops the agent named by the pid file, after checking that
// the pid really runs a binary from pumasi's directory (macOS has no /proc: ps
// names the executable there).
const stopAgentScript = `
d=` + remoteDir + `
p=$(cat "$d/agent.pid" 2>/dev/null || true)
if [ -n "$p" ] && { readlink "/proc/$p/exe" 2>/dev/null || ps -o comm= -p "$p" 2>/dev/null; } | grep -q "/.cache/pumasi/"; then
  kill "$p" 2>/dev/null || true
  i=0
  while kill -0 "$p" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
fi
rm -f "$d/agent.pid"
`

// ensureAgent makes the host run this build's agent: upload the binary when the
// host does not have it, then (re)start the agent with the host's budget.
// It returns what it did, for the log.
func ensureAgent(ctx context.Context, h config.Host, distDir string) (string, error) {
	// Line 1: OS and machine. Line 2: command line of the agent the pid file names, if alive. Rest: cache listing.
	out, err := runRemote(ctx, h, `uname -sm
d=`+remoteDir+`
p=$(cat "$d/agent.pid" 2>/dev/null || true)
if [ -n "$p" ] && [ -r "/proc/$p/cmdline" ]; then tr '\0' ' ' < "/proc/$p/cmdline"
elif [ -n "$p" ]; then ps -o args= -p "$p" 2>/dev/null | tr '\n' ' '; fi
echo
ls "$d" 2>/dev/null || true`)
	if err != nil {
		return "", err
	}
	machine, rest, _ := strings.Cut(out, "\n")
	running, have, _ := strings.Cut(rest, "\n")
	kernel, machine, _ := strings.Cut(strings.TrimSpace(machine), " ")
	goos := map[string]string{"Linux": "linux", "Darwin": "darwin"}[kernel]
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[machine]
	if goos == "" || arch == "" {
		return "", fmt.Errorf("%s: unsupported machine %q (uname -sm)", h.Name, kernel+" "+machine)
	}
	local := filepath.Join(distDir, "pumasi-"+goos+"-"+arch)
	bin, err := os.ReadFile(local)
	if err != nil {
		return "", fmt.Errorf("%s runs %s/%s but %s is missing: run `make` in the pumasi source tree", h.Name, goos, arch, local)
	}
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	name := "pumasi-" + sha[:12]

	agentArgs := fmt.Sprintf("agent -listen 127.0.0.1:%d -host %s -budget-cpus %g -budget-mem %d -pidfile",
		h.AgentPort, h.Name, h.Budget.CPUs, int64(h.Budget.Memory))
	// Same build, same settings: leave it alone. It has been sampling its runners
	// all along and holds the final numbers of those that finished meanwhile.
	if strings.Contains(running, "/.cache/pumasi/"+name+" "+agentArgs+" ") {
		return "agent already running", nil
	}

	did := "agent started"
	if !strings.Contains("\n"+have+"\n", "\n"+name+"\n") {
		if _, err := runRemote(ctx, h, uploadScript(bin, sha, name)); err != nil {
			return "", fmt.Errorf("upload agent: %w", err)
		}
		did = fmt.Sprintf("agent uploaded (%.1fM) and started", float64(len(bin))/(1<<20))
	}
	start := fmt.Sprintf(`set -e
%s
find "$d" -maxdepth 1 -name 'pumasi-*' ! -name %s -delete
own=; command -v setsid >/dev/null 2>&1 && own=setsid
$own nohup "$d/%s" %s "$d/agent.pid" > "$d/agent.log" 2>&1 < /dev/null &
i=0
while [ ! -s "$d/agent.pid" ] && [ $i -lt 30 ]; do sleep 0.1; i=$((i+1)); done
sleep 0.3
if ! kill -0 "$(cat "$d/agent.pid" 2>/dev/null)" 2>/dev/null; then
  echo "agent did not stay up: $(tail -n 3 "$d/agent.log" | tr '\n' ' ')"
  exit 1
fi
`, stopAgentScript, name, name, agentArgs)
	if _, err := runRemote(ctx, h, start); err != nil {
		return "", fmt.Errorf("start agent: %w", err)
	}
	return did, nil
}

// uploadScript carries the binary inside the script as gzip+base64 text. Raw
// bytes after the script on the same stdin are not safe: the shell may read
// ahead, and a Windows hop may translate them.
func uploadScript(bin []byte, sha, name string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "set -e\nd=%s\nmkdir -p \"$d\"\nbase64 -d <<'PUMASI_EOF' | gzip -d > \"$d/upload.tmp\"\n", remoteDir)
	enc := base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: &b})
	zw := gzip.NewWriter(enc)
	_, _ = zw.Write(bin)
	_ = zw.Close()
	_ = enc.Close()
	fmt.Fprintf(&b, "\nPUMASI_EOF\nsum=sha256sum; command -v sha256sum >/dev/null 2>&1 || sum=\"shasum -a 256\"\necho \"%s  $d/upload.tmp\" | $sum -c - >/dev/null\nchmod +x \"$d/upload.tmp\"\nmv \"$d/upload.tmp\" \"$d/%s\"\n", sha, name)
	return b.String()
}

// lineWrapper breaks base64 output into 76-column lines.
type lineWrapper struct {
	w   *bytes.Buffer
	col int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	for _, c := range p {
		if l.col == 76 {
			l.w.WriteByte('\n')
			l.col = 0
		}
		l.w.WriteByte(c)
		l.col++
	}
	return len(p), nil
}

// nativeRunners is sh for runners that are processes: it sets $up to the pids
// of the runners whose process still lives. On macOS each is the leader of its
// runner's process group; on Linux its systemd scope holds the runner.
const nativeRunners = `
d=` + remoteDir + `
up=
for m in "$d"/runners/*/pumasi.json; do
  [ -f "$m" ] || continue
  p=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$m")
  # The pid must still be that runner's shell: a dead runner's pid may be someone else's by now.
  if [ -n "$p" ] && ps -o args= -p "$p" 2>/dev/null | grep -q "/.cache/pumasi/runners/"; then up="$up $p:$(basename "$(dirname "$m")")"; fi
done
`

// runningContainers lists the pumasi runners that are up on a host.
func runningContainers(ctx context.Context, h config.Host) ([]string, error) {
	out, err := runRemote(ctx, h, `if [ "$(uname -s)" = Linux ]; then docker ps --filter label=pumasi --format '{{.Names}}'; fi`+nativeRunners+`
for r in $up; do echo "${r#*:}"; done`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// cleanHost removes everything pumasi left on a host: the agent, every runner
// (containers labelled pumasi and the scopes of process runners on Linux, the
// runners' process groups elsewhere) and the cache directory. Docker is touched on Linux only: a Mac's docker is
// its user's, not pumasi's.
func cleanHost(ctx context.Context, h config.Host) (string, error) {
	return runRemote(ctx, h, stopAgentScript+`
n=0
if [ "$(uname -s)" = Linux ]; then
  ids=$(docker ps -aq --filter label=pumasi 2>/dev/null || true)
  if [ -n "$ids" ]; then n=$(echo "$ids" | wc -l | tr -d ' '); docker rm -f $ids >/dev/null; fi
  export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
  for u in $(systemctl --user list-units 'pumasi-*.scope' --all --no-legend --plain 2>/dev/null | awk '{print $1}'); do
    systemctl --user kill --signal=SIGKILL "$u" 2>/dev/null || true
    systemctl --user stop "$u" 2>/dev/null || true
    n=$((n+1))
  done
else`+nativeRunners+`
  for r in $up; do kill -TERM -- "-${r%%:*}" 2>/dev/null || true; n=$((n+1)); done
  [ -z "$up" ] || sleep 2
  for r in $up; do kill -KILL -- "-${r%%:*}" 2>/dev/null || true; done
fi
rm -rf `+remoteDir+`
echo "$n"
`)
}
