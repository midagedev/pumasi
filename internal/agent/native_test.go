//go:build darwin || linux || windows

package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRunnerLog(t *testing.T) {
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	log := "\n√ Connected to GitHub\n\nCurrent runner version: '2.338.2'\n2026-11-25 07:31:04Z: Listening for Jobs\n" +
		"2026-11-25 07:31:09Z: Running job: sleeper\n2026-11-25 07:31:40Z: Job sleeper completed with result: Succeeded\n"
	ev := parseRunnerLog(log, now)
	if !ev.Listening.Equal(time.Date(2026, 11, 25, 7, 31, 4, 0, time.UTC)) || !ev.JobStarted.Equal(time.Date(2026, 11, 25, 7, 31, 9, 0, time.UTC)) {
		t.Errorf("events %+v", ev)
	}
	if ev := parseRunnerLog("Current runner version: '2.338.2'\n", now); !ev.Listening.IsZero() || !ev.JobStarted.IsZero() {
		t.Errorf("nothing yet, got %+v", ev)
	}
	// A line the runner printed without its usual time still counts, as of now.
	if ev := parseRunnerLog("Listening for Jobs\n", now); !ev.Listening.Equal(now) {
		t.Errorf("events %+v", ev)
	}
}

func TestRunnerEnvDropsCredentials(t *testing.T) {
	got := runnerEnv([]string{"PATH=/bin", "HOME=/Users/u", "GH_TOKEN=x", "GITHUB_TOKEN=x", "PUMASI_GITHUB_TOKEN=x", "npm_config_token=x",
		"AWS_SECRET_ACCESS_KEY=x", "OPENAI_API_KEY=x", "DB_PASSWORD=x", "ACTIONS_RUNNER_INPUT_JITCONFIG=old", "SSH_AUTH_SOCK=/tmp/s", "LANG=C"})
	if want := []string{"PATH=/bin", "HOME=/Users/u", "SSH_AUTH_SOCK=/tmp/s", "LANG=C"}; !slices.Equal(got, want) {
		t.Errorf("env %v, want %v", got, want)
	}
}

func TestRunnerVersionFromImage(t *testing.T) {
	if v, err := runnerVersion("ghcr.io/actions/actions-runner:2.338.2"); err != nil || v != "2.338.2" {
		t.Errorf("%q %v", v, err)
	}
	for _, image := range []string{"ghcr.io/actions/actions-runner", "ghcr.io/actions/actions-runner:latest", "localhost:5000/runner"} {
		if _, err := runnerVersion(image); err == nil {
			t.Errorf("%s: no version, want an error", image)
		}
	}
}

func TestInsideRefusesEscapes(t *testing.T) {
	if _, err := inside("/x/dist", "bin/Runner.Listener"); err != nil {
		t.Error(err)
	}
	for _, name := range []string{"../etc/passwd", "bin/../../x", ".."} {
		if _, err := inside("/x/dist", name); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestAssetDigest(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	body := `{"assets":[{"name":"actions-runner-osx-arm64-2.338.2.tar.gz","digest":"sha256:` + sum + `"},{"name":"old.zip","digest":null}]}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.Header().Set("X-RateLimit-Remaining", "0")
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	defer func(old string) { releaseAPI = old }(releaseAPI)
	releaseAPI = srv.URL + "/v"

	if got, err := assetDigest("2.338.2", "actions-runner-osx-arm64-2.338.2.tar.gz"); err != nil || got != sum {
		t.Errorf("digest %q %v", got, err)
	}
	// No digest, no such file, no answer: each is a refusal that says why.
	if _, err := assetDigest("2.338.2", "old.zip"); err == nil || !strings.Contains(err.Error(), "without a sha256") {
		t.Errorf("err %v", err)
	}
	if _, err := assetDigest("2.338.2", "missing.zip"); err == nil || !strings.Contains(err.Error(), "no file") {
		t.Errorf("err %v", err)
	}
	status = http.StatusForbidden
	if _, err := assetDigest("2.338.2", "old.zip"); err == nil || !strings.Contains(err.Error(), "hourly limit") {
		t.Errorf("err %v", err)
	}
	// A download that does not match is never unpacked.
	status = http.StatusOK
	dir := filepath.Join(t.TempDir(), "dist", "x")
	if err := fetchDist("2.338.2", "old.zip", dir); err == nil {
		t.Error("no digest: want an error")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("something was unpacked")
	}
}

// A directory the job left without write permission (a Go module cache) is no
// reason to keep a removed runner's files.
func TestRemoveTreeGetsThroughReadOnlyDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directories have no write bit on Windows")
	}
	dir := filepath.Join(t.TempDir(), "r")
	deep := filepath.Join(dir, "mod", "cache")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "f"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{deep, filepath.Dir(deep)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeTree(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
}

// Without a boot id a runner that a reboot took is told as "died", and the
// host's reboot is never reported.
func TestBootIDIsKnownAndSteady(t *testing.T) {
	id := BootID()
	if id == "" {
		t.Fatal("no boot id on this OS")
	}
	time.Sleep(20 * time.Millisecond)
	if again := BootID(); again != id {
		t.Errorf("boot id changed without a reboot: %s, then %s", id, again)
	}
	t.Logf("boot id %s", id)
}

func TestClassEnv(t *testing.T) {
	base := []string{"HOME=/home/u", "PATH=/usr/bin:/bin", "LANG=C"}
	if got := classEnv(base, Spec{}, "/r/x"); !slices.Equal(got, base) {
		t.Fatalf("no class env must change nothing: %v", got)
	}
	got := classEnv(base, Spec{
		Env:  map[string]string{"LANG": "en_US.UTF-8", "GRADLE_USER_HOME": "{runner}/gradle", "HOME": "{runner}"},
		Path: []string{"/opt/java/bin", "{runner}/bin"},
	}, "/r/x")
	want := map[string]string{
		"PATH": "/opt/java/bin" + string(os.PathListSeparator) + "/r/x/bin" + string(os.PathListSeparator) + "/usr/bin:/bin",
		"LANG": "en_US.UTF-8", "GRADLE_USER_HOME": "/r/x/gradle", "HOME": "/r/x",
	}
	if len(got) != len(want) {
		t.Fatalf("each name once: %v", got)
	}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		if want[k] != v {
			t.Errorf("%s = %q, want %q", k, v, want[k])
		}
	}
	// A class may set PATH outright; path: then goes in front of what the agent had, env wins.
	got = classEnv(base, Spec{Env: map[string]string{"PATH": "/only"}}, "/r/x")
	if !slices.Contains(got, "PATH=/only") || slices.Contains(got, "PATH=/usr/bin:/bin") {
		t.Fatalf("env PATH replaces: %v", got)
	}
}

// A class's work outlives its runners: the runner's _work is a link to the
// slot's directory, and removing the runner leaves what the job built there.
func TestWorkStaysWithTheSlot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "runners", "r1")
	if err := os.MkdirAll(filepath.Join(dir, "runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Spec{Class: "c", Slot: 3, WorkDir: filepath.Join(root, "work", "w{slot}")}
	if err := linkWork(dir, s); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(root, "work", "w3")
	if err := os.WriteFile(filepath.Join(dir, "runner", "_work", "node_modules"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(slot, "node_modules")); err != nil || string(b) != "x" {
		t.Fatalf("slot's work after the runner went: %q, %v", b, err)
	}
	if err := linkWork(dir, Spec{Class: "c", WorkDir: "/w/{slot}"}); err == nil {
		t.Error("no slot number: want an error")
	}
	if err := linkWork(dir, Spec{Class: "c"}); err != nil {
		t.Errorf("no work: %v", err)
	}
}
