package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

// link is how the controller reaches one host and looks after its agent. It
// is the only place that knows who dials whom: over ssh the controller goes to
// the host (sshLink), with join the host comes to the controller (joinLink).
// Everything else — placement, the ledger, incidents, recovery — sees a host
// with an agent API.
type link interface {
	// Agent is the client of the host's agent API.
	Agent() *api.Client
	// Via names the way the host is attached: "ssh" or "join".
	Via() string
	// Why says why the host was last cut off ("" = not known).
	Why() string
	// Ensure makes the host ready before the first round and says what it did.
	Ensure(ctx context.Context, distDir string) (string, error)
	// Revive brings a silent agent back. "" with a nil error: nothing the
	// controller can do from here, the host has to come back by itself.
	Revive(ctx context.Context, distDir string) (string, error)
	// Running lists the pumasi containers that are up on the host.
	Running(ctx context.Context) ([]string, error)
	// Clean removes everything pumasi put on the host and returns how many
	// containers that took.
	Clean(ctx context.Context) (string, error)
	Close()
}

// SSHTunnel keeps `ssh -N -L 127.0.0.1:<local>:127.0.0.1:<remote> <dest>` running
// and restarts it on demand when it has died. The agent never listens off loopback.
type SSHTunnel struct {
	Dest       string
	RemotePort int
	Log        *slog.Logger

	mu     sync.Mutex
	cmd    *exec.Cmd
	local  int
	dead   chan struct{}
	closed bool
}

func (t *SSHTunnel) BaseURL(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil {
		select {
		case <-t.dead:
			t.cmd = nil
		default:
			return "http://127.0.0.1:" + strconv.Itoa(t.local), nil
		}
	}
	if err := t.start(ctx); err != nil {
		return "", err
	}
	return "http://127.0.0.1:" + strconv.Itoa(t.local), nil
}

func (t *SSHTunnel) start(ctx context.Context) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	fwd := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", port, t.RemotePort)
	// Not bound to ctx: the tunnel outlives the call that opened it. Close stops it.
	// ControlPath=none: through a shared master connection ssh ignores -N and opens
	// a shell that exits at once (seen with a Windows OpenSSH host).
	cmd := exec.Command("ssh", "-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-L", fwd, t.Dest)
	// Own process group: a terminal Ctrl-C must not take the tunnel down before
	// the controller has removed its runners through it.
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ssh %s: %w", t.Dest, err)
	}
	dead := make(chan struct{})
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		closed := t.closed
		t.mu.Unlock()
		if !closed {
			t.Log.Warn("ssh tunnel exited", "dest", t.Dest, "err", err)
		}
		close(dead)
	}()

	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-dead:
			return fmt.Errorf("ssh tunnel to %s exited before forwarding", t.Dest)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return fmt.Errorf("ssh tunnel to %s: no forward after 30s", t.Dest)
		}
	}
	t.cmd, t.local, t.dead = cmd, port, dead
	t.Log.Debug("ssh tunnel up", "dest", t.Dest, "local", port, "remote", t.RemotePort)
	return nil
}

func (t *SSHTunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.cmd != nil {
		_ = t.cmd.Process.Kill()
		t.cmd = nil
	}
}
