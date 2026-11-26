package controller

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
)

func TestWhichJobsCanReadTheControllersCredentials(t *testing.T) {
	state := filepath.Join(string(filepath.Separator)+"home", "ci", ".local", "state", "pumasi")
	cfg := &config.Config{Ledger: filepath.Join(state, "pumasi.db"), Classes: []config.Class{
		{Name: "box", OS: "linux", Driver: "docker"},
		{Name: "proc", OS: "linux", Driver: "native"},
		{Name: "dind", OS: "linux", Driver: "docker", Docker: "inside"},
		{Name: "home", OS: "linux", Driver: "docker", Mounts: []string{"/srv/cache", "/home/ci:ro"}},
		{Name: "cache", OS: "linux", Driver: "docker", Mounts: []string{"/home/ci/.local/state/pumasi-other"}},
		{Name: "elsewhere", OS: "linux", Driver: "native", Hosts: []string{"server9"}},
		{Name: "mac", OS: "macos", Driver: "native"},
	}}
	for _, tc := range []struct {
		name    string
		snap    *api.Snapshot
		classes string
		why     []string
	}{
		{"another machine", &api.Snapshot{Machine: "other", UID: "1000"}, "", nil},
		{"an agent that does not say where it runs", &api.Snapshot{}, "", nil},
		{"not heard yet", nil, "", nil},
		{"this machine, the controller's user", &api.Snapshot{Machine: "me", UID: "1000"}, "proc dind home",
			[]string{"processes of the user who runs this controller", "privileged (docker: inside)", "mounts /home/ci, which holds the controller's state"}},
		{"this machine, another user with docker", &api.Snapshot{Machine: "me", UID: "1001"}, "proc dind home",
			[]string{"a user who may use docker"}},
		{"this Mac, the controller's user", &api.Snapshot{Machine: "me", UID: "1000", OS: "macos"}, "mac", []string{"the user who runs this controller"}},
		{"this Mac, another user", &api.Snapshot{Machine: "me", UID: "502", OS: "macos"}, "", nil},
	} {
		classes, why := exposed(cfg, "me", "1000", "desk1", tc.snap)
		if got := strings.Join(classes, " "); got != tc.classes {
			t.Errorf("%s: classes %q, want %q", tc.name, got, tc.classes)
		}
		all := strings.Join(why, " | ")
		for _, w := range tc.why {
			if !strings.Contains(all, w) {
				t.Errorf("%s: no %q in %q", tc.name, w, all)
			}
		}
	}
	// A controller that cannot tell its own machine warns of nothing rather than of everything.
	if classes, _ := exposed(cfg, "", "1000", "desk1", &api.Snapshot{UID: "1000"}); len(classes) != 0 {
		t.Errorf("no machine identity: %v", classes)
	}
}
