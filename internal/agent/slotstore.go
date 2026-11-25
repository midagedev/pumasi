package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A slot is one registered runner the host keeps: the three files GitHub's
// config.sh leaves behind, which are all a runner needs to listen again.
//
//	<cache>/slots/<class>/<n>/slot.json                what pumasi knows about it
//	<cache>/slots/<class>/<n>/.runner                  the registration (name, id, URLs)
//	<cache>/slots/<class>/<n>/.credentials             its OAuth client
//	<cache>/slots/<class>/<n>/.credentials_rsaparams   its private key
//	<cache>/slots/plan.json                            the controller's last plan
//
// A runner never runs from here: each time a slot listens it gets a fresh
// copy of the runner with these files put in, and that copy is removed after
// one job.

const (
	OriginRegistered = "registered" // by pumasi, with a registration token
	OriginAdopted    = "adopted"    // the credentials of a fixed runner, copied
)

// credFiles make a registration; extraFiles come along when they are there:
// the settings GitHub pushed after the first session, and the environment a
// fixed runner was configured with.
var (
	credFiles  = []string{".runner", ".credentials", ".credentials_rsaparams"}
	extraFiles = []string{".runner_migrated", ".env", ".path"}
)

type SlotMeta struct {
	Class string `json:"class"`
	N     int    `json:"n"`
	// GitHub's name and id of the runner, and where it is registered.
	Name   string    `json:"name"`
	ID     int64     `json:"id"`
	URL    string    `json:"url"`
	Origin string    `json:"origin"`
	From   string    `json:"from,omitempty"` // adopted: the directory the files were copied from
	At     time.Time `json:"at"`
	// V2: the runner talks to GitHub's broker (github.com). Only there is it
	// known when an idle listener can be stopped without losing a job.
	V2 bool `json:"v2"`
}

// SlotKey names a slot: class/n.
func SlotKey(class string, n int) string { return class + "/" + strconv.Itoa(n) }

func (m SlotMeta) Key() string { return SlotKey(m.Class, m.N) }

type SlotStore struct{ Dir string }

// SlotsDir is where a host keeps its slots, under pumasi's cache directory.
func SlotsDir(cache string) string { return filepath.Join(cache, "slots") }

func (s SlotStore) dir(class string, n int) string {
	return filepath.Join(s.Dir, class, strconv.Itoa(n))
}

// Path is the directory holding a slot's files.
func (s SlotStore) Path(m SlotMeta) string { return s.dir(m.Class, m.N) }

// List is every slot with a complete set of files, by class and number.
func (s SlotStore) List() []SlotMeta {
	var out []SlotMeta
	classes, _ := os.ReadDir(s.Dir)
	for _, c := range classes {
		if !c.IsDir() {
			continue
		}
		ns, _ := os.ReadDir(filepath.Join(s.Dir, c.Name()))
		for _, e := range ns {
			n, err := strconv.Atoi(e.Name())
			if err != nil || n < 1 {
				continue // being written, or not a slot
			}
			b, err := os.ReadFile(filepath.Join(s.dir(c.Name(), n), "slot.json"))
			var m SlotMeta
			if err != nil || json.Unmarshal(b, &m) != nil {
				continue
			}
			m.Class, m.N = c.Name(), n
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].N < out[j].N
	})
	return out
}

// Free is the lowest slot number of the class that holds no runner.
func (s SlotStore) Free(class string) int {
	for n := 1; ; n++ {
		if _, err := os.Stat(s.dir(class, n)); err != nil {
			return n
		}
	}
}

// RunnerSettings is what pumasi reads of a runner's .runner file.
type RunnerSettings struct {
	AgentID   int64  `json:"agentId"`
	AgentName string `json:"agentName"`
	GitHubURL string `json:"gitHubUrl"`
	UseV2Flow bool   `json:"useV2Flow"`
}

var bom = []byte{0xEF, 0xBB, 0xBF} // the runner writes its settings with one

// ReadRunnerSettings reads <dir>/.runner.
func ReadRunnerSettings(dir string) (RunnerSettings, error) {
	var rs RunnerSettings
	b, err := os.ReadFile(filepath.Join(dir, ".runner"))
	if err != nil {
		return rs, err
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, bom), &rs); err != nil {
		return rs, fmt.Errorf("%s: %w", filepath.Join(dir, ".runner"), err)
	}
	if rs.AgentName == "" {
		return rs, fmt.Errorf("%s names no runner", filepath.Join(dir, ".runner"))
	}
	return rs, nil
}

// Add copies a registration from the runner directory `from` into slot n of
// the class and returns what it found there. The source is not changed.
// With extras the runner's .env and .path come along (a fixed runner's
// environment); the settings GitHub pushed (.runner_migrated) always do.
func (s SlotStore) Add(class string, n int, origin, from string, extras bool) (SlotMeta, error) {
	rs, err := ReadRunnerSettings(from)
	if err != nil {
		return SlotMeta{}, err
	}
	m := SlotMeta{Class: class, N: n, Name: rs.AgentName, ID: rs.AgentID, URL: rs.GitHubURL, Origin: origin, At: time.Now().UTC(), V2: rs.UseV2Flow}
	if origin == OriginAdopted {
		m.From = from
	}
	dst := s.dir(class, n)
	if _, err := os.Stat(dst); err == nil {
		return m, fmt.Errorf("slot %s is taken", m.Key())
	}
	tmp := dst + ".new"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return m, err
	}
	fail := func(err error) (SlotMeta, error) {
		_ = os.RemoveAll(tmp)
		return m, err
	}
	for _, f := range credFiles {
		if err := copyCred(filepath.Join(from, f), filepath.Join(tmp, f)); err != nil {
			return fail(fmt.Errorf("%s is not a configured runner: %w", from, err))
		}
	}
	for _, f := range extraFiles {
		if f != ".runner_migrated" && !extras {
			continue
		}
		if err := copyCred(filepath.Join(from, f), filepath.Join(tmp, f)); err != nil && !os.IsNotExist(err) {
			return fail(err)
		}
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(tmp, "slot.json"), b, 0o600); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fail(err)
	}
	return m, nil
}

// Remove deletes a slot's files. The registration on GitHub is not touched.
func (s SlotStore) Remove(m SlotMeta) error {
	err := os.RemoveAll(s.Path(m))
	_ = os.Remove(filepath.Join(s.Dir, m.Class)) // when it was the class's last
	return err
}

// MoveOut puts a slot's registration into a directory under `to`, named after
// the runner, and frees the slot: the files of a runner that stays registered
// on GitHub are not thrown away.
func (s SlotStore) MoveOut(m SlotMeta, to string) (string, error) {
	dst := filepath.Join(to, m.Name)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", err
	}
	for _, f := range append(append([]string{}, credFiles...), extraFiles...) {
		if err := copyCred(filepath.Join(s.Path(m), f), filepath.Join(dst, f)); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return dst, s.Remove(m)
}

// AskRelease marks a slot to be given up: the agent stops its listener when
// that is safe (an idle one now, a busy one after its job) and then frees it.
func (s SlotStore) AskRelease(m SlotMeta) error {
	return os.WriteFile(filepath.Join(s.Path(m), "release"), nil, 0o600)
}

func (s SlotStore) releaseAsked(m SlotMeta) bool {
	_, err := os.Stat(filepath.Join(s.Path(m), "release"))
	return err == nil
}

// install puts the slot's files into a fresh runner directory. The copy runs
// with its work directory inside itself, so removing it removes the job's
// files, and without self-update: a runner that updated itself would do so
// again in every fresh copy.
func (s SlotStore) install(m SlotMeta, runnerDir string, classPath []string) error {
	src := s.Path(m)
	for _, f := range append(append([]string{}, credFiles...), extraFiles...) {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			if os.IsNotExist(err) && !contains(credFiles, f) {
				continue
			}
			return err
		}
		switch f {
		case ".runner", ".runner_migrated":
			if b, err = rewriteSettings(b); err != nil {
				return fmt.Errorf("slot %s: %s: %w", m.Key(), f, err)
			}
		case ".path":
			// The runner takes PATH from this file alone; the class's path goes in front.
			if len(classPath) > 0 {
				b = []byte(strings.Join(classPath, string(os.PathListSeparator)) + string(os.PathListSeparator) + strings.TrimSpace(string(b)))
			}
		}
		if err := os.WriteFile(filepath.Join(runnerDir, f), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// saveBack keeps the settings GitHub pushed to the runner during its first
// session, so the next copy does not fetch them (and restart its session) again.
func (s SlotStore) saveBack(m SlotMeta, runnerDir string) {
	const f = ".runner_migrated"
	if _, err := os.Stat(filepath.Join(s.Path(m), f)); err == nil {
		return
	}
	if _, err := os.Stat(s.Path(m)); err != nil {
		return // the slot was released meanwhile
	}
	_ = copyCred(filepath.Join(runnerDir, f), filepath.Join(s.Path(m), f))
}

// rewriteSettings sets the two settings a slot's copy runs with and leaves
// the rest of the runner's settings file as it is.
func rewriteSettings(b []byte) ([]byte, error) {
	hadBOM := bytes.HasPrefix(b, bom)
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, bom)))
	dec.UseNumber() // ids are not floats
	var kv map[string]any
	if err := dec.Decode(&kv); err != nil {
		return nil, err
	}
	if kv == nil {
		return nil, errors.New("not a settings object")
	}
	// The file is written by the runner in either spelling of its keys.
	set := func(key string, v any) {
		for k := range kv {
			if strings.EqualFold(k, key) {
				kv[k] = v
				return
			}
		}
		kv[key] = v
	}
	set("workFolder", "_work")
	set("disableUpdate", true)
	out, err := json.MarshalIndent(kv, "", "  ")
	if err != nil {
		return nil, err
	}
	if hadBOM {
		out = append(append([]byte{}, bom...), out...)
	}
	return out, nil
}

func copyCred(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
