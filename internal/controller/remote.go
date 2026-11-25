package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/midagedev/pumasi/internal/config"
)

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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(strings.ReplaceAll(string(out), "\r", ""))
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
// the pid really runs a binary from pumasi's directory.
const stopAgentScript = `
d=` + remoteDir + `
p=$(cat "$d/agent.pid" 2>/dev/null || true)
if [ -n "$p" ] && readlink "/proc/$p/exe" 2>/dev/null | grep -q "/.cache/pumasi/"; then
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
	out, err := runRemote(ctx, h, `uname -m; ls `+remoteDir+` 2>/dev/null || true`)
	if err != nil {
		return "", err
	}
	machine, have, _ := strings.Cut(out, "\n")
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(machine)]
	if arch == "" {
		return "", fmt.Errorf("%s: unsupported machine %q (uname -m)", h.Name, machine)
	}
	local := filepath.Join(distDir, "pumasi-linux-"+arch)
	bin, err := os.ReadFile(local)
	if err != nil {
		return "", fmt.Errorf("%s runs linux/%s but %s is missing: run `make` in the pumasi source tree", h.Name, arch, local)
	}
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	name := "pumasi-" + sha[:12]

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
setsid nohup "$d/%s" agent -listen 127.0.0.1:%d -host %s -budget-cpus %g -budget-mem %d -pidfile "$d/agent.pid" > "$d/agent.log" 2>&1 < /dev/null &
i=0
while [ ! -s "$d/agent.pid" ] && [ $i -lt 30 ]; do sleep 0.1; i=$((i+1)); done
sleep 0.3
if ! kill -0 "$(cat "$d/agent.pid" 2>/dev/null)" 2>/dev/null; then
  echo "agent did not stay up: $(tail -n 3 "$d/agent.log" | tr '\n' ' ')"
  exit 1
fi
`, stopAgentScript, name, name, h.AgentPort, h.Name, h.Budget.CPUs, int64(h.Budget.Memory))
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
	fmt.Fprintf(&b, "\nPUMASI_EOF\necho \"%s  $d/upload.tmp\" | sha256sum -c - >/dev/null\nchmod +x \"$d/upload.tmp\"\nmv \"$d/upload.tmp\" \"$d/%s\"\n", sha, name)
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

// stopAgent stops the agent on a host and leaves the cached binary for the next `up`.
func stopAgent(ctx context.Context, h config.Host) error {
	_, err := runRemote(ctx, h, stopAgentScript)
	return err
}

// cleanHost removes everything pumasi left on a host: the agent, every
// container labelled pumasi, and the cache directory.
func cleanHost(ctx context.Context, h config.Host) (string, error) {
	return runRemote(ctx, h, stopAgentScript+`
ids=$(docker ps -aq --filter label=pumasi 2>/dev/null || true)
n=0
if [ -n "$ids" ]; then n=$(echo "$ids" | wc -l | tr -d ' '); docker rm -f $ids >/dev/null; fi
rm -rf `+remoteDir+`
echo "$n"
`)
}
