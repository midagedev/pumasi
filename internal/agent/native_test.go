//go:build darwin || windows

package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
