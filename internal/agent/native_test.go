//go:build darwin || windows

package agent

import (
	"slices"
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
