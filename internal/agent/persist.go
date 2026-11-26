package agent

import (
	"encoding/json"
	"os"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

// The agent keeps its runner records in one small file so a restart loses
// neither the final cgroup numbers of a container that already exited nor the
// times read from runner output. The boot id tells a restart from a reboot.

type savedState struct {
	BootID  string        `json:"bootId"`
	Runners []savedRunner `json:"runners"`
}

type savedRunner struct {
	api.Runner
	ReceivedAt time.Time `json:"receivedAt"`
}

// load reads the state file. Runners that were alive under another boot id are
// marked: if their container is gone or dead, the host rebooted under them.
func (a *Agent) load() {
	if a.cfg.StateFile == "" {
		return
	}
	b, err := os.ReadFile(a.cfg.StateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			a.log.Warn("state file", "err", err)
		}
		return
	}
	var s savedState
	if err := json.Unmarshal(b, &s); err != nil {
		a.log.Warn("state file unreadable, starting empty", "err", err)
		return
	}
	rebooted := s.BootID != "" && a.cfg.BootID != "" && s.BootID != a.cfg.BootID
	for _, sr := range s.Runners {
		if sr.State == "starting" { // `docker run` never returned; nothing to keep
			continue
		}
		r := &rec{Runner: sr.Runner, receivedAt: sr.ReceivedAt, lastUsec: uint64(sr.CPUSec * 1e6)}
		r.rebooted = rebooted && sr.State != "exited" && sr.Lost == ""
		a.recs[sr.Name] = r
	}
	if rebooted {
		a.log.Warn("host rebooted since the last run", "runners", len(a.recs))
	}
}

// save writes the records atomically. Callers hold opMu.
func (a *Agent) save() {
	if a.cfg.StateFile == "" || a.forgotten.Load() {
		return
	}
	a.dataMu.Lock()
	s := savedState{BootID: a.cfg.BootID, Runners: make([]savedRunner, 0, len(a.recs))}
	for _, r := range a.recs {
		s.Runners = append(s.Runners, savedRunner{Runner: r.Runner, ReceivedAt: r.receivedAt})
	}
	a.dataMu.Unlock()
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := a.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		a.log.Warn("state file", "err", err)
		return
	}
	if err := os.Rename(tmp, a.cfg.StateFile); err != nil {
		a.log.Warn("state file", "err", err)
	}
}
