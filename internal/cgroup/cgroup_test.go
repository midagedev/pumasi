package cgroup

import (
	"path/filepath"
	"testing"
)

const scope = "system.slice/docker-bdfedcdf244d7769e643f4ca02d4c203c693296c006811ec590b3f72bef505ad.scope"

func fixtureReader() Reader {
	return Reader{
		CgroupRoot: filepath.Join("testdata", "cg"),
		ProcRoot:   filepath.Join("testdata", "proc"),
		SysRoot:    filepath.Join("testdata", "sys"),
	}
}

func TestParsers(t *testing.T) {
	tests := []struct {
		name string
		got  func() (any, error)
		want any
	}{
		{"cpu.stat", func() (any, error) { return ParseCPUStat("user_usec 1\nusage_usec 42\n") }, uint64(42)},
		{"memory.peak", func() (any, error) { return ParseUint("95039488\n") }, uint64(95039488)},
		{"memory.max unlimited", func() (any, error) { return ParseUint("max\n") }, uint64(0)},
		{"io.stat sum", func() (any, error) {
			return ParseIOStat("8:0 rbytes=10 wbytes=5 rios=1 wios=1\n252:0 rbytes=3 wbytes=2 dbytes=99\n", nil)
		}, uint64(20)},
		{"io.stat skip", func() (any, error) {
			return ParseIOStat("8:0 rbytes=10 wbytes=5\n252:0 rbytes=3 wbytes=2\n", func(d string) bool { return d == "8:0" })
		}, uint64(5)},
		{"pressure", func() (any, error) {
			return ParsePressure("some avg10=15.61 avg60=17.17 avg300=15.31 total=155042271262\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
		}, Pressure{Avg10: 15.61, Total: 155042271262}},
		{"meminfo", func() (any, error) {
			return ParseMeminfo("MemTotal:       129002740 kB\nMemFree: 1 kB\nMemAvailable:    8118260 kB\n")
		}, Meminfo{Total: 129002740 * 1024, Available: 8118260 * 1024}},
		{"proc cgroup", func() (any, error) {
			return ParseProcCgroup("0::/system.slice/docker-abc.scope\n")
		}, "/system.slice/docker-abc.scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.got()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := ParseCPUStat("user_usec 1\n"); err == nil {
		t.Error("cpu.stat without usage_usec: want error")
	}
	if _, err := ParsePressure("full avg10=0 total=0\n"); err == nil {
		t.Error("pressure without some: want error")
	}
	if _, err := ParseProcCgroup("12:cpu:/foo\n"); err == nil {
		t.Error("v1-only cgroup: want error")
	}
}

func TestReaderOnFixtures(t *testing.T) {
	r := fixtureReader()
	dir, err := r.PathOfPid(42)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("testdata", "cg", scope); dir != want {
		t.Fatalf("path %q, want %q", dir, want)
	}
	st, err := r.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.CPUUsec != 571525387 {
		t.Errorf("cpu %d", st.CPUUsec)
	}
	if st.MemPeak != 95039488 || st.MemCurrent != 91234304 {
		t.Errorf("mem peak %d current %d", st.MemPeak, st.MemCurrent)
	}
	// 8:0 is held by dm (252:0), so it is not counted twice.
	if st.OOMKills != 2 {
		t.Errorf("oom kills %d, want 2", st.OOMKills)
	}
	if want := uint64(2772353024 + 24555520 + 139743232 + 170946560 + 1023324160 + 4096); st.IOBytes != want {
		t.Errorf("io %d, want %d", st.IOBytes, want)
	}
	if st.Pressure["cpu"] != 44396719 || st.Pressure["memory"] != 128515439 || st.Pressure["io"] != 740512275 {
		t.Errorf("pressure %v", st.Pressure)
	}

	root, err := r.RootCPUUsec()
	if err != nil || root != 31733006685000 {
		t.Errorf("root cpu %d %v", root, err)
	}
	psi, err := r.HostPressure()
	if err != nil {
		t.Fatal(err)
	}
	if psi["cpu"] != 15.61 || psi["memory"] != 0.73 || psi["io"] != 17.25 {
		t.Errorf("host psi %v", psi)
	}
	mi, err := r.Meminfo()
	if err != nil || mi.Available != 8118260*1024 {
		t.Errorf("meminfo %+v %v", mi, err)
	}
}
