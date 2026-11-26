package controller

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
)

// The credentials are the controller's alone: the GitHub token, and the key
// hosts know the controller by. A host gets one runner's one-time
// registration and nothing else. That holds while jobs run somewhere else.
// A host on the controller's own machine can break it: a job that is a
// process of the controller's user reads the token like the controller does.
//
// pumasi warns and does not refuse. The same machine is where everyone tries
// pumasi first, and it is a sound setup when every workflow that reaches
// those runners is the operator's own; whether that is so (no fork pull
// requests, no outside contributors) is nothing pumasi can see. A refusal
// would be switched off as a whole; a warning that names the classes and the
// way out stays until someone decides — jobsMayReadCredentials: true.

const exposureAdvice = "Run the controller on a machine that takes no jobs, or keep those classes off this machine with hosts:. If every workflow that reaches them is as trusted as you are, jobsMayReadCredentials: true ends this warning."

// exposed is, for one host, the classes whose jobs can read the controller's
// credentials, each with the reason. self is the controller's own machine
// and user (agent.Identity).
func exposed(cfg *config.Config, selfMachine, selfUID string, name string, snap *api.Snapshot) (classes []string, why []string) {
	if snap == nil || selfMachine == "" || snap.Machine != selfMachine {
		return nil, nil
	}
	hostOS := snap.OS
	if hostOS == "" {
		hostOS = "linux"
	}
	state := filepath.Clean(cfg.StateDir())
	add := func(class, reason string) {
		classes = append(classes, class)
		if !slices.Contains(why, reason) {
			why = append(why, reason)
		}
	}
	for _, cl := range cfg.Classes {
		if cl.OS != hostOS || len(cl.Hosts) > 0 && !slices.Contains(cl.Hosts, name) {
			continue
		}
		switch {
		case cl.Driver == "native" && snap.UID == selfUID:
			add(cl.Name, "its runners are processes of the user who runs this controller")
		case cl.Driver == "native" && hostOS == "linux":
			// The agent's user reaches docker there (a host joins only when it does).
			add(cl.Name, "its runners are processes of a user who may use docker, and docker reads any file of the machine")
		case cl.Driver == "native":
			// Another user on a Mac or Windows machine: the files are not theirs to read.
		case cl.Docker == "inside":
			add(cl.Name, "its runner containers are privileged (docker: inside)")
		default:
			for _, m := range mountPaths(cl.Mounts) {
				m = filepath.Clean(m)
				if m == state || strings.HasPrefix(state+string(filepath.Separator), strings.TrimSuffix(m, string(filepath.Separator))+string(filepath.Separator)) {
					add(cl.Name, fmt.Sprintf("it mounts %s, which holds the controller's state", m))
					break
				}
			}
		}
	}
	return classes, why
}

// exposure is the warnings for the hosts as they are now, and logs each when
// it first holds. It runs on the evaluating goroutine.
func (c *Controller) exposure() []string {
	if c.cfg.JobsMayReadCredentials {
		return nil
	}
	if c.selfMachine == "" {
		c.selfMachine, c.selfUID = agent.Identity()
	}
	var out []string
	for _, h := range c.hosts {
		classes, why := exposed(c.cfg, c.selfMachine, c.selfUID, h.cfg.Name, h.snap)
		if h.snap == nil {
			continue // not heard: what was said stays said
		}
		text := ""
		if len(classes) > 0 {
			text = fmt.Sprintf("jobs of %s on %s can read this controller's GitHub token and keys: %s is this machine, and %s. %s",
				cond(len(classes) == 1, "class ", "classes ")+strings.Join(classes, ", "), h.cfg.Name, h.cfg.Name, strings.Join(why, "; "), exposureAdvice)
			out = append(out, text)
		}
		if text != h.exposed && text != "" {
			c.log.Warn(text)
		}
		h.exposed = text
	}
	return out
}
