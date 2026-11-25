package agent

import (
	"errors"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/cgroup"
)

// Usage is what one runner has used so far. IOBytes and Pressure are nil
// where the OS cannot measure them.
type Usage struct {
	CPUUsec    uint64
	MemCurrent int64
	MemPeak    int64
	IOBytes    *int64
	OOMKills   int               // processes killed (Linux) or refused memory (Windows) at the limit
	Pressure   map[string]uint64 // some-stall microseconds by cpu|memory|io
}

// HostUsage is the machine as a whole.
type HostUsage struct {
	MemBytes     int64
	MemAvailable int64
	CPUUsec      uint64             // CPU time used by everything since boot
	PSI          map[string]float64 // avg10 percent; nil where the OS has no PSI
	// Closed: the OS's own sign that the machine is short right now (macOS
	// memory pressure, Windows commit charge). Empty = fine.
	Closed string
	// InputIdle: how long ago someone last touched the keyboard or mouse. nil = not known.
	InputIdle *time.Duration
}

// Meter measures runners and the host. Linux reads cgroup v2 and PSI; the
// native drivers measure their own process trees.
type Meter interface {
	Runner(c Container) (Usage, error)
	Host() (HostUsage, error)
}

// CgroupMeter measures docker containers through their cgroups.
type CgroupMeter struct {
	R cgroup.Reader

	mu   sync.Mutex
	dirs map[string]string // runner -> cgroup directory
}

func (m *CgroupMeter) Runner(c Container) (Usage, error) {
	m.mu.Lock()
	dir := m.dirs[c.Name]
	m.mu.Unlock()
	if dir == "" {
		if c.Pid <= 0 {
			return Usage{}, errors.New("no pid")
		}
		var err error
		if dir, err = m.R.PathOfPid(c.Pid); err != nil {
			return Usage{}, err
		}
		m.mu.Lock()
		if m.dirs == nil {
			m.dirs = map[string]string{}
		}
		m.dirs[c.Name] = dir
		m.mu.Unlock()
	}
	s, err := m.R.Read(dir)
	if err != nil {
		return Usage{}, err
	}
	io := int64(s.IOBytes)
	return Usage{CPUUsec: s.CPUUsec, MemCurrent: int64(s.MemCurrent), MemPeak: int64(s.MemPeak),
		IOBytes: &io, OOMKills: int(s.OOMKills), Pressure: s.Pressure}, nil
}

func (m *CgroupMeter) Host() (HostUsage, error) {
	var h HostUsage
	psi, err1 := m.R.HostPressure()
	if err1 == nil {
		h.PSI = psi
	}
	mi, err2 := m.R.Meminfo()
	if err2 == nil {
		h.MemBytes, h.MemAvailable = int64(mi.Total), int64(mi.Available)
	}
	root, err3 := m.R.RootCPUUsec()
	h.CPUUsec = root
	h.InputIdle = inputIdle()
	return h, errors.Join(err1, err2, err3)
}
