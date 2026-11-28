package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func demoJSON(t *testing.T, d *Demo, at float64, now time.Time) string {
	t.Helper()
	b, err := json.Marshal(d.view(at, now).state())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The same seed and second give the same state: a screenshot can be taken again.
func TestDemoIsDeterministic(t *testing.T) {
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	a, b := NewDemo(3), NewDemo(3)
	want := demoJSON(t, a, 437.3, now)
	if got := demoJSON(t, b, 437.3, now); got != want {
		t.Fatal("two demos of one seed differ")
	}
	// Ahead and back again (a checkpoint), and from scratch: the same.
	demoJSON(t, b, 905, now)
	if got := demoJSON(t, b, 437.3, now); got != want {
		t.Error("going back to a moment gives another state")
	}
	// Another wall clock moves the timestamps only.
	later := demoJSON(t, NewDemo(3), 437.3, now.Add(time.Hour))
	if len(later) != len(want) || later == want {
		t.Error("a later wall clock should move every timestamp and nothing else")
	}
	if demoJSON(t, NewDemo(4), 437.3, now) == want {
		t.Error("another seed gives the same shop")
	}
}

// The scenes the demo is for come round in every cycle.
func TestDemoScenes(t *testing.T) {
	d := NewDemo(1)
	now := time.Now()
	s := d.view(350, now).state()
	if h := s.Hosts[3]; h.Name != "linux-spare" || *h.Reachable || h.Runners == 0 {
		t.Errorf("at 350 linux-spare is away with its runners: %+v", h)
	}
	if len(s.Incidents) == 0 || s.Incidents[0].Kind != "host-unreachable" {
		t.Errorf("at 350 the unreachable host is a problem: %+v", s.Incidents)
	}
	if h := s.Hosts[2]; h.Closed != "" {
		t.Errorf("wsl-desktop is free after 180: %q", h.Closed)
	}
	if h := d.view(100, now).state().Hosts[2]; h.Closed != "in use" {
		t.Errorf("wsl-desktop is in use before 180: %q", h.Closed)
	}
	if h := d.view(500, now).state().Hosts[1]; h.Closed != "disk 8% free" || *h.DiskFreePct != 8 {
		t.Errorf("mac-mini is short of disk at 500: %+v", h)
	}

	oom, short, deployHigh, deployOther, afterOK, afterSeen, watched := false, false, 0, 0, true, false, false
	for at := 2.0; at < 600; at += 2 {
		s := d.view(at, now).state()
		for _, in := range s.RecentIncidents {
			if in.Kind == "out-of-memory" && strings.Contains(in.Detail, "hit class e2e's memory limit (20.0G)") && in.OpenedAt.After(now.Add(-time.Duration(at)*time.Second)) {
				oom = true
			}
		}
		for _, w := range s.Waiting {
			short = short || strings.HasPrefix(w.Reason, "no host with ")
		}
		for _, j := range s.WaitingJobs {
			if j.Class == "deploy" {
				if j.Level == "high" {
					deployHigh++
				} else {
					deployOther++
				}
			}
		}
		for _, r := range s.Runners {
			watched = watched || r.Watched
		}
		var light, mac Overflow
		for _, o := range s.Overflow {
			switch o.Class {
			case "light":
				light = o
			case "light-mac":
				mac = o
			}
		}
		if mac.Take > mac.Held {
			afterSeen = true
			if light.Take > light.Held {
				afterOK = false
			}
		}
	}
	if !oom {
		t.Error("no out-of-memory in the first cycle")
	}
	if !short {
		t.Error("nothing waited for room")
	}
	if deployHigh == 0 || deployOther > 0 {
		t.Errorf("deploy jobs waiting: %d high, %d not", deployHigh, deployOther)
	}
	if !afterSeen || !afterOK {
		t.Errorf("light-mac offers room (seen %v) only while light has none (held %v)", afterSeen, afterOK)
	}
	if !watched {
		t.Error("no runner of an unmeasured job was watched")
	}
	if jobs := d.view(600, now).jobs(); len(jobs) != 50 || jobs[0].FinishedAt.Before(jobs[49].FinishedAt) {
		t.Errorf("jobs: %d, newest first", len(jobs))
	}
}

func post(t *testing.T, url, body string, json bool) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if json {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	_ = json.Unmarshal(b.Bytes(), &m)
	return resp.StatusCode, m
}

func getState(t *testing.T, url string) State {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s State
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The demo's priority controls act through the real placement.
func TestDemoPriorityControls(t *testing.T) {
	srv := httptest.NewServer(NewDemo(1).Handler())
	defer srv.Close()
	// At 250 the e2e class waits; take its newest job.
	s := getState(t, srv.URL+"/api/state?at=250")
	var target JobWait
	for _, j := range s.WaitingJobs {
		if j.Class == "e2e" {
			target = j
		}
	}
	if target.GithubJobID == 0 || target.Position < 2 {
		t.Fatalf("no e2e job waiting behind others at 250: %+v", s.WaitingJobs)
	}
	if code, _ := post(t, srv.URL+"/api/expedite?at=250", `{"job":1}`, false); code != http.StatusUnsupportedMediaType {
		t.Errorf("a write without a JSON content type: %d, want 415", code)
	}
	if code, m := post(t, srv.URL+"/api/expedite?at=250", `{"job":"nope"}`, true); code != http.StatusNotFound || m["ok"] != false || !strings.HasPrefix(m["error"].(string), "no job \"nope\" waits") {
		t.Errorf("unknown job: %d %v", code, m)
	}
	code, m := post(t, srv.URL+"/api/expedite?at=250", `{"job":`+jsonNum(target.GithubJobID)+`}`, true)
	if code != 200 || m["ok"] != true || m["class"] != "e2e" || m["job"] != target.ID || m["by"] != "api 127.0.0.1" {
		t.Fatalf("expedite: %d %v", code, m)
	}
	pos := int(m["position"].(float64))
	if pos >= target.Position {
		t.Errorf("expedited job is at %d, was %d", pos, target.Position)
	}
	s = getState(t, srv.URL+"/api/state?at=250")
	for _, j := range s.WaitingJobs {
		if j.GithubJobID == target.GithubJobID && (!j.Expedited || j.Level != "expedite" || j.Position != pos) {
			t.Errorf("state after expedite: %+v", j)
		}
	}
	resp, err := http.Get(srv.URL + "/api/priority?at=250")
	if err != nil {
		t.Fatal(err)
	}
	var pr PriorityView
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(pr.Expedited) != 1 || pr.Expedited[0].GitHubJobID != target.GithubJobID || pr.Mode.Name != "normal" || len(pr.Rules) != 2 || pr.HeadStart != "30m" {
		t.Errorf("priority %+v", pr)
	}
	if len(pr.Line) == 0 || pr.Line[0].Position != 1 || pr.Line[pos-1].GitHubJobID != target.GithubJobID {
		t.Errorf("line %+v", pr.Line)
	}

	// High-only for ten minutes: from the next round, normal jobs wait for that reason.
	if code, m := post(t, srv.URL+"/api/mode?at=260", `{"name":"high-only","for":"10m"}`, true); code != 200 || m["name"] != "high-only" || m["until"] == nil {
		t.Fatalf("mode: %d %v", code, m)
	}
	s = getState(t, srv.URL+"/api/state?at=280")
	found := false
	for _, w := range s.Waiting {
		found = found || strings.HasPrefix(w.Reason, "high-only mode for ")
	}
	if !found {
		t.Errorf("no class waits for high-only mode: %+v", s.Waiting)
	}
	// Before the mode was set, nothing changed.
	for _, w := range getState(t, srv.URL+"/api/state?at=258").Waiting {
		if strings.HasPrefix(w.Reason, "high-only") {
			t.Errorf("high-only before it was set: %+v", w)
		}
	}

	// Rules: one from the dashboard, and one from the file that cannot be removed here.
	code, m = post(t, srv.URL+"/api/priority/rules?at=290", `{"match":{"class":"nightly"},"level":"high","for":"1h"}`, true)
	if code != 200 || m["id"] != "r-1" || m["source"] != "runtime" || m["expires"] == nil {
		t.Errorf("rule: %d %v", code, m)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/priority/rules/file-1?at=290", nil)
	if code, m := do(t, req); code != http.StatusConflict || m["ok"] != false || !strings.HasPrefix(m["error"].(string), "rule file-1 is in ") {
		t.Errorf("delete file rule: %d %v", code, m)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/priority/rules/r-1?at=295", nil)
	if code, _ := do(t, req); code != 200 {
		t.Errorf("delete runtime rule: %d", code)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/expedite?at=295&job="+jsonNum(target.GithubJobID), nil)
	if code, m := do(t, req); code != 200 && code != http.StatusNotFound {
		t.Errorf("un-expedite: %d %v", code, m)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Waiting jobs say when they are expected to start, where, and whose end
// makes the room; one waiting for a runner on a host that is down says nothing.
func TestDemoWaitingJobsHaveAnExpectedStart(t *testing.T) {
	st := NewDemo(1).view(250, time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)).state()
	var told int
	for _, j := range st.WaitingJobs {
		if j.Expect == nil {
			continue
		}
		told++
		if j.Expect.Host == "" || j.Expect.InSeconds < 0 || j.Expect.InSeconds > 7200 {
			t.Fatalf("%s: %+v", j.Job, j.Expect)
		}
	}
	if told < 2 {
		t.Fatalf("%d of %d waiting jobs have an expected start", told, len(st.WaitingJobs))
	}
}

func TestExpectText(t *testing.T) {
	for _, c := range []struct {
		e    JobExpect
		want string
	}{
		{JobExpect{InSeconds: 40, Host: "h1"}, "starts in under a minute on h1"},
		{JobExpect{InSeconds: 150, Host: "h1", After: "e2e"}, "starts in ~3m on h1, when e2e ends"},
		{JobExpect{InSeconds: 6000, Host: "h2", After: "build"}, "starts in ~1h40m on h2, when build ends"},
	} {
		if got := c.e.Text(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}

// Once the jobs have come a few times, the demo's busy classes keep runners
// warm, and the state says how many.
func TestDemoKeepsRunnersWarm(t *testing.T) {
	st := NewDemo(1).view(575, time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)).state()
	warm := map[string]int{}
	for _, c := range st.Classes {
		warm[c.Name] = c.Warm
	}
	if warm["pr-checks"] == 0 || warm["light"] != 0 {
		t.Fatalf("warm by class %v", warm)
	}
	idle := 0
	for _, r := range st.Runners {
		if r.State == "idle" {
			idle++
		}
	}
	if idle == 0 {
		t.Fatalf("no runner up without a job: %+v", st.Runners)
	}
}
