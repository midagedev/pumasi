package controller

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/join"
)

// ssh is used for one thing: to run on a host of the config what a person
// would otherwise type there — the invite line when the host is not connected,
// `pumasi leave` when `down` cannot reach it through its connection.

// runRemote pipes a script to the host's shell over ssh and returns its output.
// The script always travels on stdin: an ssh command line would be re-parsed by
// whatever shell the host gives ssh (PowerShell on a Windows host), and what is
// in the script (the invite's token) stays out of every process's arguments.
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

// Exit code of the bootstrap script when the host reached none of the controller's addresses.
const exitUnreachable = 69

// bootstrapScript is the invite line as the controller runs it for a host of
// its config: the first of addrs the host can reach, then join with the
// config's name, budget and kind.
func bootstrapScript(h config.Host, pin, token string, addrs []string) string {
	kind := "--server"
	if h.Desktop {
		kind = "--desktop"
	}
	return fmt.Sprintf(`export PUMASI_TOKEN='%s'
pin='%s'
why=
for a in %s; do
  if s=$(curl -fsSk --connect-timeout 5 --pinnedpubkey "$pin" "https://$a/join.sh" 2>&1); then
    printf '%%s\n' "$s" | sh -s -- --name %s --cpus %g --mem %d %s
    exit $?
  fi
  why=$s
done
echo "$why"
exit %d
`, token, pin, strings.Join(addrs, " "), h.Name, h.Budget.CPUs, int64(h.Budget.Memory), kind, exitUnreachable)
}

// bootstrap makes a host of the config join: it runs the invite line there
// over ssh. Running it again on a host that has joined restarts its agent with
// the config's budget; runners are not touched.
func (c *Controller) bootstrap(ctx context.Context, h config.Host) error {
	id, err := join.LoadIdentity(c.cfg.StateDir())
	if err != nil {
		return err
	}
	token, err := join.NewToken(c.cfg.StateDir(), h.Name, 1, 10*time.Minute)
	if err != nil {
		return err
	}
	addrs := join.Advertised(c.listenAddr)
	out, err := runRemote(ctx, h, bootstrapScript(h, id.Pin, token, addrs))
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == exitUnreachable:
		return fmt.Errorf("%s cannot reach this controller at %s (%s): hosts connect to the controller, so set listen: to an address that host can reach, or open the port",
			h.Name, strings.Join(addrs, ", "), firstLine(lastLine(out)))
	}
	// `pumasi join` names what failed on its FAIL lines.
	var fails []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FAIL ") {
			fails = append(fails, strings.TrimSpace(strings.TrimPrefix(line, "FAIL ")))
		}
	}
	if len(fails) > 0 {
		return fmt.Errorf("%s did not join: %s", h.Name, strings.Join(fails, "; "))
	}
	return fmt.Errorf("%s did not join: %w", h.Name, err)
}

// needsBootstrap: the host is not connected as the config describes it.
func (c *Controller) needsBootstrap(h *host) bool {
	if !h.connected() {
		return true
	}
	ms := c.join.Members()
	i := slices.IndexFunc(ms, func(m join.Member) bool { return m.Name == h.cfg.Name })
	return i < 0 || ms[i].CPUs != h.cfg.Budget.CPUs || ms[i].Mem != int64(h.cfg.Budget.Memory) || ms[i].Kind != h.cfg.Kind()
}

// leaveOverSSH runs `pumasi leave` on a host of the config that is not
// connected. Without force the host itself refuses while runners are up.
func leaveOverSSH(ctx context.Context, h config.Host, force bool) (string, error) {
	flag := ""
	if force {
		flag = " --force"
	}
	out, err := runRemote(ctx, h, `b="$HOME/.local/bin/pumasi"
if [ ! -x "$b" ]; then echo "left: pumasi is not on this host"; exit 0; fi
exec "$b" leave`+flag+"\n")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, firstLine(out))
	}
	return lastLine(out), nil
}
