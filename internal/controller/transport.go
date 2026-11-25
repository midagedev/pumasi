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
)

// Transport gives the base URL of one host's agent. M1 reaches agents through
// ssh port forwarding; an agent-dials-controller transport can replace it later.
type Transport interface {
	BaseURL(ctx context.Context) (string, error)
	Close()
}

// SSHTunnel keeps `ssh -N -L 127.0.0.1:<local>:127.0.0.1:<remote> <dest>` running
// and restarts it on demand when it has died. The agent never listens off loopback.
type SSHTunnel struct {
	Dest       string
	RemotePort int
	Log        *slog.Logger

	mu    sync.Mutex
	cmd   *exec.Cmd
	local int
	dead  chan struct{}
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
	cmd := exec.Command("ssh", "-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-L", fwd, t.Dest)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ssh %s: %w", t.Dest, err)
	}
	dead := make(chan struct{})
	go func() {
		err := cmd.Wait()
		t.Log.Warn("ssh tunnel exited", "dest", t.Dest, "local", port, "err", err)
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
	t.Log.Info("ssh tunnel up", "dest", t.Dest, "local", port, "remote", t.RemotePort)
	return nil
}

func (t *SSHTunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil {
		_ = t.cmd.Process.Kill()
		t.cmd = nil
	}
}
