// Package cgroup reads cgroup v2 accounting files and /proc pressure and memory
// files. Parsers take the file contents so they can be tested on fixtures.
package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pressure is one line ("some" or "full") of a PSI file.
type Pressure struct {
	Avg10 float64
	Total uint64 // microseconds stalled
}

// Stats is what pumasi keeps per runner cgroup.
type Stats struct {
	CPUUsec    uint64
	MemCurrent uint64
	MemPeak    uint64
	IOBytes    uint64
	OOMKills   uint64 // memory.events oom_kill: processes killed at the memory limit
	// Some-stall totals in microseconds, keyed by cpu|memory|io.
	Pressure map[string]uint64
}

// ParseCPUStat returns usage_usec from cpu.stat.
func ParseCPUStat(s string) (uint64, error) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "usage_usec" {
			return strconv.ParseUint(f[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("cpu.stat: no usage_usec")
}

// ParseMemoryEvents returns oom_kill from memory.events (0 when absent).
func ParseMemoryEvents(s string) uint64 {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "oom_kill" {
			n, _ := strconv.ParseUint(f[1], 10, 64)
			return n
		}
	}
	return 0
}

// ParseUint parses single-value files such as memory.peak and memory.current.
func ParseUint(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "max" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// ParseIOStat sums rbytes+wbytes over devices. skip, when non-nil, drops a device
// ("MAJ:MIN"); it is used to leave out disks that sit under a device-mapper or md
// device, whose bytes the upper device already counts.
func ParseIOStat(s string, skip func(dev string) bool) (uint64, error) {
	var total uint64
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if skip != nil && skip(f[0]) {
			continue
		}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || (k != "rbytes" && k != "wbytes") {
				continue
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("io.stat %q: %w", kv, err)
			}
			total += n
		}
	}
	return total, nil
}

// ParsePressure returns the "some" line of a PSI file (cgroup *.pressure or /proc/pressure/*).
func ParsePressure(s string) (Pressure, error) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "some" {
			continue
		}
		var p Pressure
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			var err error
			switch k {
			case "avg10":
				p.Avg10, err = strconv.ParseFloat(v, 64)
			case "total":
				p.Total, err = strconv.ParseUint(v, 10, 64)
			}
			if err != nil {
				return Pressure{}, fmt.Errorf("pressure %q: %w", kv, err)
			}
		}
		return p, nil
	}
	return Pressure{}, fmt.Errorf("pressure: no some line")
}

// Meminfo holds the two /proc/meminfo fields pumasi uses, in bytes.
type Meminfo struct {
	Total, Available uint64
}

func ParseMeminfo(s string) (Meminfo, error) {
	var m Meminfo
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			m.Total = n * 1024
		case "MemAvailable:":
			m.Available = n * 1024
		}
	}
	if m.Total == 0 {
		return m, fmt.Errorf("meminfo: no MemTotal")
	}
	return m, nil
}

// ParseProcCgroup returns the unified (v2) cgroup path from /proc/<pid>/cgroup.
func ParseProcCgroup(s string) (string, error) {
	for _, line := range strings.Split(s, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry")
}

// Reader reads from a cgroup root and a /proc root (both overridable for tests).
type Reader struct {
	CgroupRoot string // /sys/fs/cgroup
	ProcRoot   string // /proc
	SysRoot    string // /sys
}

func DefaultReader() Reader {
	return Reader{CgroupRoot: "/sys/fs/cgroup", ProcRoot: "/proc", SysRoot: "/sys"}
}

func readString(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// PathOfPid returns the absolute cgroup directory of a process.
func (r Reader) PathOfPid(pid int) (string, error) {
	s, err := readString(filepath.Join(r.ProcRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	p, err := ParseProcCgroup(s)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.CgroupRoot, p), nil
}

// Read reads the accounting files of one cgroup directory. memory.peak is
// optional (kernels before 5.19); then the peak is the current value.
func (r Reader) Read(dir string) (Stats, error) {
	var st Stats
	s, err := readString(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return st, err
	}
	if st.CPUUsec, err = ParseCPUStat(s); err != nil {
		return st, err
	}
	if s, err = readString(filepath.Join(dir, "memory.current")); err == nil {
		st.MemCurrent, _ = ParseUint(s)
	}
	if s, err = readString(filepath.Join(dir, "memory.peak")); err == nil {
		st.MemPeak, _ = ParseUint(s)
	}
	st.MemPeak = max(st.MemPeak, st.MemCurrent)
	if s, err = readString(filepath.Join(dir, "io.stat")); err == nil {
		st.IOBytes, _ = ParseIOStat(s, r.hasHolders)
	}
	if s, err = readString(filepath.Join(dir, "memory.events")); err == nil {
		st.OOMKills = ParseMemoryEvents(s)
	}
	st.Pressure = map[string]uint64{}
	for _, res := range []string{"cpu", "memory", "io"} {
		if s, err = readString(filepath.Join(dir, res+".pressure")); err == nil {
			if p, err := ParsePressure(s); err == nil {
				st.Pressure[res] = p.Total
			}
		}
	}
	return st, nil
}

// hasHolders reports whether a block device is used by another block device
// (for example sda under a dm-crypt volume).
func (r Reader) hasHolders(dev string) bool {
	entries, err := os.ReadDir(filepath.Join(r.SysRoot, "dev", "block", dev, "holders"))
	return err == nil && len(entries) > 0
}

// RootCPUUsec reads usage_usec of the root cgroup (whole host).
func (r Reader) RootCPUUsec() (uint64, error) {
	s, err := readString(filepath.Join(r.CgroupRoot, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	return ParseCPUStat(s)
}

// HostPressure reads /proc/pressure/{cpu,memory,io} avg10 values.
func (r Reader) HostPressure() (map[string]float64, error) {
	out := map[string]float64{}
	for _, res := range []string{"cpu", "memory", "io"} {
		s, err := readString(filepath.Join(r.ProcRoot, "pressure", res))
		if err != nil {
			return out, err
		}
		p, err := ParsePressure(s)
		if err != nil {
			return out, err
		}
		out[res] = p.Avg10
	}
	return out, nil
}

func (r Reader) Meminfo() (Meminfo, error) {
	s, err := readString(filepath.Join(r.ProcRoot, "meminfo"))
	if err != nil {
		return Meminfo{}, err
	}
	return ParseMeminfo(s)
}
