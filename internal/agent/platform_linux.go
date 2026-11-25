package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/cgroup"
)

// Platform is how this OS runs and measures runners. Linux has both ways: a
// class's runners are docker containers (the default) or, with driver:
// native, processes of the agent's user kept in dir.
func Platform(tag, dir string) (Driver, Meter) {
	l := &linux{docker: Docker{Tag: tag}, meter: &CgroupMeter{R: cgroup.DefaultReader()}, native: &Native{Root: dir, Runners: RunnerRoot(dir)}}
	return l, l
}

// BootID changes when the host has rebooted.
func BootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// linux sends each call to the driver that holds the runner. A native runner
// is known by its directory.
type linux struct {
	docker Docker
	meter  *CgroupMeter
	native *Native
}

func (l *linux) isNative(name string) bool {
	_, err := os.Stat(l.native.dir(name))
	return err == nil
}

func (l *linux) Start(ctx context.Context, s Spec) error {
	if s.Driver != "native" {
		return l.docker.Start(ctx, s)
	}
	if p := nativeProblem(); p != "" {
		return errors.New(p)
	}
	return l.native.Start(ctx, s)
}

func (l *linux) Remove(ctx context.Context, name string) error {
	if l.isNative(name) {
		return l.native.Remove(ctx, name)
	}
	return l.docker.Remove(ctx, name)
}

func (l *linux) List(ctx context.Context) ([]Container, error) {
	cs, err := l.docker.List(ctx)
	if err != nil {
		return nil, err
	}
	ns, err := l.native.List(ctx)
	return append(cs, ns...), err
}

func (l *linux) LogEvents(ctx context.Context, name string, since time.Time) (LogEvents, error) {
	if l.isNative(name) {
		return l.native.LogEvents(ctx, name, since)
	}
	return l.docker.LogEvents(ctx, name, since)
}

func (l *linux) LogTail(ctx context.Context, name string, lines int) string {
	if l.isNative(name) {
		return l.native.LogTail(ctx, name, lines)
	}
	return l.docker.LogTail(ctx, name, lines)
}

func (l *linux) DataDir(ctx context.Context) string { return l.docker.DataDir(ctx) }

func (l *linux) Check(ctx context.Context, image string) error { return l.docker.Check(ctx, image) }

func (l *linux) NativeProblem() string { return nativeProblem() }

func (l *linux) nativeDriver() *Native { return l.native }

func (l *linux) Clean(ctx context.Context) { l.docker.Clean(ctx) }

func (l *linux) Runner(c Container) (Usage, error) {
	if l.isNative(c.Name) {
		return l.native.Runner(c)
	}
	return l.meter.Runner(c)
}

func (l *linux) Host() (HostUsage, error) { return l.meter.Host() }
