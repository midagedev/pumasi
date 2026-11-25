package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedRunner writes what config.sh leaves in a runner directory.
func fixedRunner(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o755)
	runner := "\ufeff" + `{"agentId": 9007199254740993, "agentName": "` + name + `", "poolId": 1, "gitHubUrl": "https://github.com/o/r", "workFolder": "/srv/work", "useV2Flow": true, "serverUrlV2": "https://broker.example/"}`
	for f, body := range map[string]string{".runner": runner, ".credentials": `{"scheme":"OAuth"}`, ".credentials_rsaparams": `{"d":"secret"}`, ".env": "LANG=C\n", ".path": "/usr/bin:/bin\n"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSlotStoreAdoptsWithoutTouchingTheSource(t *testing.T) {
	src := fixedRunner(t, "ci-7")
	before, _ := os.ReadFile(filepath.Join(src, ".runner"))
	s := SlotStore{Dir: filepath.Join(t.TempDir(), "slots")}
	if n := s.Free("build"); n != 1 {
		t.Fatalf("free = %d", n)
	}
	m, err := s.Add("build", 1, OriginAdopted, src, true)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "ci-7" || m.ID != 9007199254740993 || !m.V2 || m.From != src || m.URL != "https://github.com/o/r" {
		t.Errorf("meta %+v", m)
	}
	if after, _ := os.ReadFile(filepath.Join(src, ".runner")); string(after) != string(before) {
		t.Error("the adopted runner's .runner changed")
	}
	if _, err := s.Add("build", 1, OriginAdopted, src, true); err == nil {
		t.Error("a taken slot was overwritten")
	}
	if got := s.List(); len(got) != 1 || got[0].Key() != "build/1" || s.Free("build") != 2 {
		t.Errorf("list %+v", got)
	}

	// A copy to run from: its own work directory, no self-update, the rest as
	// it was (the id is beyond what a float holds), the class's path in front.
	run := t.TempDir()
	if err := s.install(m, run, []string{"/opt/tools/bin"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(run, ".runner"))
	if !strings.HasPrefix(string(b), "\ufeff") {
		t.Error("the BOM the runner writes is gone")
	}
	var kv map[string]any
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(b), "\ufeff")))
	dec.UseNumber()
	if err := dec.Decode(&kv); err != nil {
		t.Fatal(err)
	}
	if kv["workFolder"] != "_work" || kv["disableUpdate"] != true || kv["agentId"].(json.Number).String() != "9007199254740993" || kv["serverUrlV2"] != "https://broker.example/" {
		t.Errorf("settings %v", kv)
	}
	if p, _ := os.ReadFile(filepath.Join(run, ".path")); string(p) != "/opt/tools/bin"+string(os.PathListSeparator)+"/usr/bin:/bin" {
		t.Errorf(".path %q", p)
	}
	if fi, _ := os.Stat(filepath.Join(run, ".credentials_rsaparams")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file %v", fi)
	}

	// Released: the slot is free again, the source still as it was.
	if err := s.AskRelease(m); err != nil || !s.releaseAsked(m) {
		t.Fatal("release not marked", err)
	}
	if err := s.Remove(m); err != nil || len(s.List()) != 0 {
		t.Fatal("not removed", err)
	}
	if _, err := os.Stat(filepath.Join(src, ".credentials")); err != nil {
		t.Error("the adopted runner's files are gone")
	}
}

func TestSlotStoreKeepsARegisteredRunnersFiles(t *testing.T) {
	src := fixedRunner(t, "small-h-1")
	s := SlotStore{Dir: filepath.Join(t.TempDir(), "slots")}
	m, err := s.Add("small", 1, OriginRegistered, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Path(m), ".env")); err == nil {
		t.Error("a registered runner took the scratch directory's .env along")
	}
	keep := t.TempDir()
	dst, err := s.MoveOut(m, keep)
	if err != nil || dst != filepath.Join(keep, "small-h-1") {
		t.Fatal(dst, err)
	}
	for _, f := range credFiles {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Error(err)
		}
	}
	if len(s.List()) != 0 {
		t.Error("slot still there")
	}
	if _, err := os.Stat(s.Dir + "/small"); err == nil {
		t.Error("empty class directory left")
	}
}

func TestSlotStoreRefusesWhatIsNotARunner(t *testing.T) {
	s := SlotStore{Dir: filepath.Join(t.TempDir(), "slots")}
	if _, err := s.Add("c", 1, OriginAdopted, t.TempDir(), true); err == nil {
		t.Error("an empty directory was adopted")
	}
	half := fixedRunner(t, "half")
	os.Remove(filepath.Join(half, ".credentials_rsaparams"))
	if _, err := s.Add("c", 1, OriginAdopted, half, true); err == nil || len(s.List()) != 0 {
		t.Error("a runner without its key was adopted", err)
	}
	if _, err := os.Stat(s.dir("c", 1) + ".new"); err == nil {
		t.Error("half a slot left behind")
	}
}

func TestParseSlotLog(t *testing.T) {
	now := time.Now()
	ev := parseSlotLog(`
√ Connected to GitHub

Current runner version: '2.338.2'
2026-01-02 03:04:05Z: Listening for Jobs
2026-01-02 03:04:09Z: Running job: build (linux, 22)
2026-01-02 03:05:09Z: Job build (linux, 22) completed with result: Succeeded
Exiting with return code 0
`, now)
	if ev.Job != "build (linux, 22)" || ev.Result != "Succeeded" || ev.Conflict {
		t.Errorf("%+v", ev)
	}
	if got := ev.lastLine(); !strings.Contains(got, "completed with result") {
		t.Errorf("last line %q", got)
	}
	ev = parseSlotLog("2026-01-02 03:04:05Z: Listening for Jobs\n2026-01-02 03:04:09Z: Running job: deploy\n", now)
	if ev.Job != "deploy" || ev.Result != "" {
		t.Errorf("a job cut short: %+v", ev)
	}
	ev = parseSlotLog("A session for this runner already exists.\nRunner connect error: Error: Conflict. Retrying until reconnected.\n", now)
	if !ev.Conflict || ev.Job != "" {
		t.Errorf("conflict: %+v", ev)
	}
	if got := parseSlotLog("", now).lastLine(); got != "it printed nothing" {
		t.Errorf("%q", got)
	}
}
