package controller

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// Explain reads a State the way a person at the dashboard would and says what
// needs doing: the open problems, why each waiting class waits and how much
// room it lacks where, and what is worth knowing. Every step it suggests is a
// command pumasi has or a key of the config; none stops a running job.

// Step is one thing a person can do: a command to type, or a config key to set.
type Step struct {
	Run    string `json:"run,omitempty"`    // a command: `pumasi …` or `curl …`
	Config string `json:"config,omitempty"` // a config key or snippet
	Where  string `json:"where,omitempty"`  // where to run or write it
	Why    string `json:"why,omitempty"`
}

// Finding is one fact the dashboard points at.
type Finding struct {
	Level   string `json:"level"` // problem | wait | note
	Kind    string `json:"kind"`  // an incident's kind, or waiting, host, runner, overflow, warning, controller
	Subject string `json:"subject"`
	Host    string `json:"host,omitempty"`
	Class   string `json:"class,omitempty"`
	Runner  string `json:"runner,omitempty"`
	Job     string `json:"job,omitempty"`
	Text    string `json:"text"`
	Next    []Step `json:"next,omitempty"`
}

const (
	levelProblem = "problem"
	levelWait    = "wait"
	levelNote    = "note"
)

var (
	stepStatus   = Step{Run: "pumasi status", Why: "hosts with what they hold and have free, runners, the line and job sizes"}
	stepPriority = Step{Run: "pumasi priority", Why: "the priority rules and where they put the jobs waiting now"}
)

func stepDoctor(host string) Step {
	return Step{Run: "pumasi doctor", Where: "on " + host, Why: "checks the way to the controller, what runners need there, and the agent"}
}

func stepJob(runner string) Step {
	return Step{Run: "curl -s <dashboard>/api/job?runner=" + runner, Where: "<dashboard> is the address `pumasi up` logs and `pumasi open` opens",
		Why: "the runner's placement decisions and the end of its log"}
}

// Explain lists the findings: problems first, then the waiting classes in the
// order of the line, then notes. pv is the source's priority controls (nil = none).
func Explain(s State, pv *PriorityView, now time.Time) []Finding {
	var problems, waits, notes []Finding
	if pv != nil {
		if pv.Mode.Name == "high-only" {
			until := "until it is set back"
			if pv.Mode.Until != nil {
				until = "until " + pv.Mode.Until.Local().Format("15:04")
			}
			notes = append(notes, Finding{Level: levelNote, Kind: "mode", Subject: "priority",
				Text: "high-only mode " + until + ": only high and expedited jobs get room; normal and low jobs wait (running jobs go on)",
				Next: []Step{{Run: "pumasi mode normal", Why: "back to the usual line"}}})
		}
		for _, e := range pv.Expedited {
			ref := expediteRef(e, pv.Line)
			notes = append(notes, Finding{Level: levelNote, Kind: "expedited", Subject: ref, Class: e.Class, Job: e.Name,
				Text: fmt.Sprintf("%s (%s) is expedited by %s: %s; it starts when room frees, nothing running is stopped for it",
					e.Name, e.Class, cmp.Or(e.By, "someone"), expediteWhere(e)),
				Next: []Step{{Run: "pumasi expedite --clear " + ref, Why: "puts it back in its place by age"}}})
		}
	}
	hosts := map[string]HostState{}
	for _, h := range s.Hosts {
		hosts[h.Name] = h
	}
	classes := map[string]ClassState{}
	for _, c := range s.Classes {
		classes[c.Name] = c
	}
	if !s.Controller {
		problems = append(problems, Finding{Level: levelProblem, Kind: "controller", Subject: "controller",
			Text: "no controller answered: nothing new is placed and this is the ledger alone; running jobs go on on their hosts",
			Next: []Step{{Run: "pumasi up", Why: "starts the controller; it adopts the runners that are there"},
				{Run: "pumasi service status", Why: "whether it runs as a service and why it stopped"}}})
	}
	covered := map[string]bool{} // "kind/subject" an incident already speaks for
	for _, in := range s.Incidents {
		f := incidentFinding(in, s, classes)
		covered[in.Kind+"/"+in.Subject] = true
		problems = append(problems, f)
	}
	for _, w := range s.Warnings {
		problems = append(problems, Finding{Level: levelProblem, Kind: "warning", Subject: "credentials", Text: w,
			Next: []Step{{Config: "hosts", Where: "pumasi.yaml classes[…]", Why: "send only classes you trust to the hosts it names"},
				{Config: "jobsMayReadCredentials: true", Where: "pumasi.yaml", Why: "once you have judged the jobs there may read them; ends the warning"}}})
	}

	for _, h := range s.Hosts {
		switch {
		case h.Reachable != nil && !*h.Reachable:
			if covered["host-unreachable/"+h.Name] || covered["agent-down/"+h.Name] {
				continue
			}
			text := h.Name + " does not answer"
			if h.SeenSeconds != nil {
				text += fmt.Sprintf(" (last answer %s ago)", dur(*h.SeenSeconds))
			}
			if h.LastDrop != "" {
				text += ": " + h.LastDrop
			}
			text += fmt.Sprintf("; its %d runners' jobs go on there, nothing new is placed there", h.Runners)
			problems = append(problems, Finding{Level: levelProblem, Kind: "host", Subject: h.Name, Host: h.Name, Text: text,
				Next: []Step{stepDoctor(h.Name), {Run: "pumasi status", Why: "SEEN, RTT and LAST DROP of " + h.Name}}})
		case h.Closed != "":
			if covered["host-quarantined/"+h.Name] || covered["disk-low/"+h.Name] {
				continue
			}
			notes = append(notes, closedFinding(h, s.Limits))
		}
		if h.Reachable != nil && *h.Reachable {
			for _, p := range []struct {
				name  string
				v     *float64
				limit float64
			}{{"cpu", h.CPUPressure, s.Limits.PSICPU}, {"memory", h.MemPressure, s.Limits.PSIMemory}, {"disk", h.IOPressure, s.Limits.PSIIO}} {
				if p.v != nil && p.limit > 0 && *p.v >= p.limit {
					notes = append(notes, Finding{Level: levelNote, Kind: "host", Subject: h.Name, Host: h.Name,
						Text: fmt.Sprintf("%s %s pressure %.0f%% is over %.0f%%: it takes no new runners until it eases; running jobs go on", h.Name, p.name, *p.v, p.limit)})
				}
			}
		}
	}

	waitingJobs := map[string][]JobWait{}
	for _, j := range s.WaitingJobs {
		waitingJobs[j.Class] = append(waitingJobs[j.Class], j)
	}
	for _, w := range s.Waiting {
		waits = append(waits, waitFinding(w, waitingJobs[w.Class], hosts, classes, s.Limits))
	}

	for _, r := range s.Runners {
		if r.MemLimit > 0 && r.MemBytes > 0 && float64(r.MemBytes) >= 0.9*float64(r.MemLimit) {
			which := "the memory limit that protects " + r.Host
			var next []Step
			if c, ok := classes[r.Class]; ok && c.LimitMem > 0 && c.LimitMem <= r.MemLimit {
				which = "class " + r.Class + "'s memory limit"
				next = append(next, Step{Config: "limit", Where: "pumasi.yaml classes[" + r.Class + "]", Why: "the class's hard limit; raise or drop it if the job needs more"})
			}
			problems = append(problems, Finding{Level: levelProblem, Kind: "runner", Subject: r.Name, Host: r.Host, Class: r.Class, Runner: r.Name, Job: r.Job,
				Text: fmt.Sprintf("%s on %s uses %s, within 10%% of %s (%s): over it a process of the job is killed (out-of-memory)", jobOr(r), r.Host, gib(r.MemBytes), which, gib(r.MemLimit)),
				Next: next})
			continue
		}
		if r.EstMem > 0 && r.MemBytes > r.EstMem {
			notes = append(notes, Finding{Level: levelNote, Kind: "runner", Subject: r.Name, Host: r.Host, Class: r.Class, Runner: r.Name, Job: r.Job,
				Text: fmt.Sprintf("%s on %s uses %s, over its estimate %s: it goes on, and its next run is placed bigger", jobOr(r), r.Host, gib(r.MemBytes), gib(r.EstMem))})
		}
	}

	for _, o := range s.Overflow {
		if o.Take > 0 {
			continue
		}
		text := o.Class + " takes no jobs now (no room): GitHub gives them to other runners of its labels, or they wait at GitHub where pumasi does not see them"
		if o.After != "" {
			text = o.Class + " takes no jobs now: it offers room only while " + o.After + " has none, and GitHub gives the rest to other runners of its labels"
		}
		notes = append(notes, Finding{Level: levelNote, Kind: "overflow", Subject: o.Class, Class: o.Class, Text: text})
	}
	if len(s.TakenBack) > 0 {
		by := map[string]int{}
		for _, j := range s.TakenBack {
			by[j.Class]++
		}
		notes = append(notes, Finding{Level: levelNote, Kind: "taken-back", Subject: "GitHub",
			Text: fmt.Sprintf("GitHub took back %d jobs in the last hour before a runner had them (%s): given back for want of room, so they ran elsewhere, or cancelled", len(s.TakenBack), countList(by))})
	}
	return slices.Concat(problems, waits, notes)
}

func jobOr(r RunnerState) string {
	if r.Job == "" {
		return r.Name
	}
	return r.Job
}

func incidentFinding(in ledger.Incident, s State, classes map[string]ClassState) Finding {
	f := Finding{Level: levelProblem, Kind: in.Kind, Subject: in.Subject, Text: in.Detail}
	if in.Action != "" {
		f.Text += " [pumasi: " + in.Action + "]"
	}
	host := func() { f.Host = in.Subject }
	runner := func() {
		f.Runner = in.Subject
		for _, r := range s.Runners {
			if r.Name == in.Subject {
				f.Host, f.Class, f.Job = r.Host, r.Class, r.Job
			}
		}
	}
	switch in.Kind {
	case "host-unreachable", "agent-down":
		host()
		f.Next = []Step{stepDoctor(in.Subject), {Run: "pumasi status", Why: "SEEN, RTT and LAST DROP of " + in.Subject}}
	case "host-quarantined":
		host()
		f.Next = []Step{stepDoctor(in.Subject), {Run: "pumasi status", Why: "pumasi checks a quarantined host every 5 minutes and opens it when the check passes"}}
	case "disk-low":
		host()
		f.Next = []Step{{Config: "diskMinFree", Where: "pumasi.yaml", Why: fmt.Sprintf("the line below which a host takes no new runners (now %.0f%%); better: free disk on %s", s.Limits.DiskMinFree, in.Subject)}}
	case "out-of-memory":
		runner()
		f.Next = []Step{{Run: "pumasi status", Why: "JOB SIZE: the job's next run is placed with the size it reached"}}
		if m := classLimitRE.FindStringSubmatch(in.Detail); m != nil {
			f.Class = m[1]
			f.Next = append(f.Next, Step{Config: "limit", Where: "pumasi.yaml classes[" + m[1] + "]", Why: "the class's hard limit; raise or drop it if the job needs more"})
		}
		f.Next = append(f.Next, stepJob(in.Subject))
	case "job-waiting":
		f.Class = in.Subject
		f.Next = []Step{stepPriority, {Config: "priority", Where: "pumasi.yaml", Why: "rules that give some jobs room first while room is short; no running job is stopped for them"}}
	case "runner-died", "max-duration", "leftover":
		runner()
		f.Next = []Step{stepJob(in.Subject)}
		switch in.Kind {
		case "runner-died":
			if !strings.Contains(in.Action, "rerun") {
				f.Next = append(f.Next, Step{Config: "rerun: 1", Where: "pumasi.yaml", Why: "reruns a run's failed jobs once after a failure on pumasi's side"})
			}
		case "max-duration":
			f.Next = append(f.Next, Step{Config: "maxDuration", Where: "pumasi.yaml classes[" + cmp.Or(f.Class, "…") + "]", Why: "how long a job of the class may run"})
		case "leftover":
			f.Text += " — delete it by hand on the host"
		}
	case "host-rebooted":
		host()
		f.Next = []Step{stepStatus}
		if !strings.Contains(in.Action, "rerun") {
			f.Next = append(f.Next, Step{Config: "rerun: 1", Where: "pumasi.yaml", Why: "reruns a run's failed jobs once after a failure on pumasi's side"})
		}
	case "github-auth":
		f.Next = []Step{{Run: "pumasi up", Why: "restart it with credentials GitHub takes (PUMASI_GITHUB_TOKEN, or githubApp)"},
			{Run: "pumasi service install", Why: "when it runs as a service: install again with the new token in PUMASI_GITHUB_TOKEN"},
			{Config: "githubApp", Where: "pumasi.yaml", Why: "an App installation instead of a person's token"}}
	case "github-unreachable":
		f.Class = in.Subject
		f.Next = []Step{{Run: "pumasi status", Why: "the problem clears by itself when GitHub answers again; check the controller machine's network"}}
	case "agent-version":
		host()
		f.Next = []Step{stepDoctor(in.Subject)}
	case "ledger-rebuilt":
		f.Next = []Step{stepStatus}
	default:
		f.Next = []Step{stepStatus}
	}
	return f
}

var classLimitRE = regexp.MustCompile(`class (\S+)'s memory limit`)

func closedFinding(h HostState, lim StateLimits) Finding {
	f := Finding{Level: levelNote, Kind: "host", Subject: h.Name, Host: h.Name}
	switch {
	case h.Closed == "in use":
		f.Text = h.Name + " is in use: people first — a desktop takes no new runners while someone works at it (input within 15 minutes); its running jobs go on"
		f.Next = []Step{{Run: "pumasi invite", Why: "an unattended machine joins with --server at the end of the invite line and is filled first"}}
	case h.Closed == "quarantined":
		f.Level = levelProblem
		f.Text = h.Name + " is quarantined: runners failed to start there; pumasi checks it every 5 minutes"
		f.Next = []Step{stepDoctor(h.Name)}
	case strings.HasPrefix(h.Closed, "disk "):
		f.Level = levelProblem
		f.Text = fmt.Sprintf("%s has %s: under %.0f%% it takes no new runners", h.Name, h.Closed, lim.DiskMinFree)
		f.Next = []Step{{Config: "diskMinFree", Where: "pumasi.yaml", Why: "the line; better: free disk on " + h.Name}}
	default:
		f.Text = h.Name + " takes no new runners for now: " + h.Closed + "; running jobs go on"
	}
	return f
}

// shortage is one host's word in a "no host with …" reason.
type shortage struct {
	host             string
	freeCPU, freeMem float64 // in cpus and GiB; -1 = not said
	other            string  // what else keeps it: "in use", "down", "reserved for deploy", …
}

var (
	noHostRE  = regexp.MustCompile(`^no host with ([\d.]+)cpu/([\d.]+)G free \((.*)\)$`)
	hostFreeR = regexp.MustCompile(`^([\d.]+)cpu/([\d.]+)G(?:: (.*))?$`)
	watchesRE = regexp.MustCompile(`it watches (\d+) unmeasured job there and takes the next with ([\d.]+G) free`)
)

// parseReason splits decide's "no host with Xcpu/YG free (…)" reason. alone:
// what follows it for a job larger than any host of its class could ever
// lend, "its size … is more than any host of … can ever lend (…)" ("" = none).
func parseReason(reason string, known map[string]HostState) (needCPU, needMem float64, per []shortage, alone string, ok bool) {
	if i := strings.Index(reason, "); its size "); i >= 0 {
		reason, alone = reason[:i+1], reason[i+3:]
		alone, _, _ = strings.Cut(alone, ": it runs alone")
	}
	m := noHostRE.FindStringSubmatch(reason)
	if m == nil {
		return 0, 0, nil, "", false
	}
	needCPU, _ = strconv.ParseFloat(m[1], 64)
	needMem, _ = strconv.ParseFloat(m[2], 64)
	for _, part := range strings.Split(m[3], ", ") {
		name, rest, _ := strings.Cut(part, " ")
		name = strings.TrimSuffix(name, ":")
		if _, isHost := known[name]; !isHost {
			if len(per) > 0 && !strings.HasPrefix(part, "the others lack") && part != "no allowed host" {
				per[len(per)-1].other += ", " + part // a reason that had a comma of its own
				continue
			}
			per = append(per, shortage{freeCPU: -1, freeMem: -1, other: part})
			continue
		}
		s := shortage{host: name, freeCPU: -1, freeMem: -1}
		if f := hostFreeR.FindStringSubmatch(rest); f != nil {
			s.freeCPU, _ = strconv.ParseFloat(f[1], 64)
			s.freeMem, _ = strconv.ParseFloat(f[2], 64)
			s.other = f[3]
		} else {
			s.other = rest
		}
		per = append(per, s)
	}
	return needCPU, needMem, per, alone, true
}

// shortText says what one host lacks for a job of needCPU/needMem.
func shortText(s shortage, needCPU, needMem float64) string {
	if s.host == "" {
		return s.other
	}
	if s.freeCPU < 0 {
		return s.host + " " + s.other
	}
	var lack []string
	if d := needMem - s.freeMem; d > 0.05 {
		lack = append(lack, fmt.Sprintf("%.1fG memory", d))
	}
	if d := needCPU - s.freeCPU; d > 0.05 {
		lack = append(lack, fmt.Sprintf("%.1f cpu", d))
	}
	text := fmt.Sprintf("%s has %.1fcpu/%.1fG free", s.host, s.freeCPU, s.freeMem)
	if len(lack) > 0 {
		text += ", " + strings.Join(lack, " and ") + " short"
	}
	if m := watchesRE.FindStringSubmatch(s.other); m != nil {
		text += fmt.Sprintf("; it watches %s unmeasured job there and takes the next with %s free or once that one has shown its size", m[1], m[2])
	} else if s.other != "" {
		text += "; " + s.other
	}
	return text
}

func waitFinding(w WaitingState, jobs []JobWait, hosts map[string]HostState, classes map[string]ClassState, lim StateLimits) Finding {
	f := Finding{Level: levelWait, Kind: "waiting", Subject: w.Class, Class: w.Class}
	n := w.Jobs
	head := fmt.Sprintf("%s: %d %s wait", w.Class, n, plural(n, "job", "jobs"))
	if w.OldestSeconds > 0 {
		head += ", the oldest " + dur(w.OldestSeconds)
		if lim.WaitAlertSeconds > 0 && w.OldestSeconds >= lim.WaitAlertSeconds {
			head += " (over the " + dur(lim.WaitAlertSeconds) + " alert)"
		}
	}
	if w.Order > 0 {
		head += fmt.Sprintf("; place %d in line", w.Order)
	}
	if w.Priority != "" {
		head += " (" + w.Priority + ")"
	}
	if len(jobs) > 0 && jobs[0].Job != "" {
		f.Job = jobs[0].Job
	}
	f.Next = []Step{stepStatus, stepPriority}
	r := w.Reason
	switch {
	case r == "":
		f.Text = head + " — no reason given yet (the next placement round says)"
	case strings.HasPrefix(r, "no host with "):
		needCPU, needMem, per, alone, ok := parseReason(r, hosts)
		if !ok {
			f.Text = head + " — " + r
			break
		}
		var parts []string
		for _, s := range per {
			parts = append(parts, shortText(s, needCPU, needMem))
		}
		f.Text = fmt.Sprintf("%s — the next needs %.1fcpu/%.1fG and no host has it: %s", head, needCPU, needMem, strings.Join(parts, "; "))
		if alone != "" {
			// The measured size no host could ever lend: placed at what one lends, alone.
			f.Text += ". " + strings.ToUpper(alone[:1]) + alone[1:] + ": it is placed at what a host lends with no runner on it and starts once one is empty, alone there" +
				" (a size near or above a whole machine may be a measurement that counted shared memory twice: see the job's runs under JOB SIZE in `pumasi status`)"
		}
		if c, ok := classes[w.Class]; ok && c.LimitMem > 0 && needMem*float64(gb) > float64(c.LimitMem) {
			f.Text += fmt.Sprintf(". Its size %.1fG is over class %s's memory limit %s: it was cut short there before and will be again",
				needMem, w.Class, gib(c.LimitMem))
			f.Next = append(f.Next, Step{Config: "limit", Where: "pumasi.yaml classes[" + w.Class + "]", Why: "raise or drop the class's hard limit, or make the job smaller"})
		}
		f.Next = append(f.Next, Step{Config: "priority", Where: "pumasi.yaml", Why: "rules that give some jobs room first while room is short; no running job is stopped for them"})
		for _, s := range per {
			h, ok := hosts[s.host]
			if !ok || s.freeCPU < 0 {
				continue
			}
			if h.BudgetMem > 0 && h.MemBytes > 0 && h.BudgetMem < h.MemBytes && needMem-s.freeMem > 0.05 {
				f.Next = append(f.Next, Step{Config: "budget", Where: "pumasi.yaml hosts[" + h.Name + "] (or join it again with --cpus/--mem)",
					Why: fmt.Sprintf("%s lends at most %s of its %s", h.Name, gib(h.BudgetMem), gib(h.MemBytes))})
			}
		}
		if c, ok := classes[w.Class]; ok && len(c.Hosts) > 0 {
			f.Next = append(f.Next, Step{Config: "hosts", Where: "pumasi.yaml classes[" + w.Class + "]",
				Why: "the class runs only on " + strings.Join(c.Hosts, ", ") + "; others may have room"})
		}
		for _, s := range per {
			if s.host != "" && s.other != "" {
				switch {
				case strings.HasPrefix(s.other, "reserved for "):
					rc := strings.TrimPrefix(s.other, "reserved for ")
					f.Next = append(f.Next, Step{Config: "reserve", Where: "pumasi.yaml classes[" + rc + "]", Why: s.host + "'s room is kept for " + rc})
				case s.other == "down" || s.other == "has not joined":
					f.Next = append(f.Next, stepDoctor(s.host))
				}
			}
		}
		f.Next = append(f.Next, Step{Run: "pumasi invite", Why: "another host adds room"})
	case strings.HasPrefix(r, "behind an older job of its class"):
		f.Text = head + " — behind the class's oldest job, which waits for room: GitHub gives a class's jobs to its runners oldest first"
	case strings.HasPrefix(r, "no host has "):
		f.Text = head + " — " + r + ": the class's mounts are on no host that takes it"
		f.Next = append(f.Next, Step{Config: "mounts", Where: "pumasi.yaml classes[" + w.Class + "]", Why: "create the path on a host, or drop it from mounts"})
	default:
		f.Text = head + " — " + r
	}
	return f
}

// expediteRef is what names an expedited job to `pumasi expedite --clear`:
// pumasi's id while its class holds it, else GitHub's job id, else the
// line's id for a job GitHub still holds.
func expediteRef(e Expedited, line []LineJob) string {
	if e.Job != "" {
		return e.Job
	}
	if e.GitHubJobID != 0 {
		return strconv.FormatInt(e.GitHubJobID, 10)
	}
	for _, j := range line {
		if j.Pending && j.Repo == e.Repo && j.RunID == e.RunID && j.Name == e.Name {
			return j.Job
		}
	}
	return e.URL
}

// expediteWhere says where an expedited job stands in the line.
func expediteWhere(e Expedited) string {
	s := fmt.Sprintf("place %d in line", e.Position)
	if e.Position == 0 {
		s = "a runner of its class is up for it"
	}
	if e.Ahead > 0 {
		s += fmt.Sprintf(", %d older %s of its class %s first (GitHub gives a class its oldest job first)",
			e.Ahead, plural(e.Ahead, "job", "jobs"), plural(e.Ahead, "goes", "go"))
	}
	if e.Pending {
		s += ", GitHub still holds it: its class takes it when there is room"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func countList(by map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(by)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, by[k]))
	}
	return strings.Join(parts, ", ")
}

func dur(sec int64) string {
	d := time.Duration(max(sec, 0)) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", sec)
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
}

func pct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func stepText(st Step) string {
	var b strings.Builder
	switch {
	case st.Run != "":
		b.WriteString("`" + st.Run + "`")
	case st.Config != "":
		b.WriteString("config `" + st.Config + "`")
	}
	if st.Where != "" {
		b.WriteString(" (" + st.Where + ")")
	}
	if st.Why != "" {
		b.WriteString(" — " + st.Why)
	}
	return b.String()
}

// WriteExplain writes the findings and the state as a short plain-text report
// with fixed section headers: ATTENTION, LINE, HOSTS, RUNNERS, OVERFLOW, NOTES, RESOLVED.
func WriteExplain(w io.Writer, s State, fs []Finding, pv *PriorityView, url string, demo bool, now time.Time) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	head := "pumasi " + now.Local().Format("2006-01-02 15:04:05")
	if s.Controller {
		head += " · controller running"
	} else {
		head += " · no controller (ledger only)"
	}
	if demo {
		head += " · DEMO (simulated)"
	}
	if url != "" {
		head += " · " + url
	}
	p("%s", head)
	p("%d hosts, %d runners, %d jobs waiting, %d open problems", len(s.Hosts), len(s.Runners), len(s.WaitingJobs), len(s.Incidents))

	writeFindings := func(title string, level string) {
		p("")
		p("%s", title)
		found := false
		for _, f := range fs {
			if f.Level != level {
				continue
			}
			found = true
			mark := map[string]string{levelProblem: "!", levelWait: "~", levelNote: "-"}[level]
			p("  %s [%s] %s", mark, f.Kind, f.Text)
			for _, st := range f.Next {
				p("      next: %s", stepText(st))
			}
		}
		if !found {
			p("  none")
		}
	}
	writeFindings("ATTENTION", levelProblem)

	if pv != nil {
		p("")
		mode := pv.Mode.Name
		if pv.Mode.Until != nil {
			mode += " until " + pv.Mode.Until.Local().Format("15:04")
		}
		if pv.Mode.By != "" {
			mode += " (set by " + pv.Mode.By + ")"
		}
		p("MODE %s", cmp.Or(mode, "normal"))
		p("")
		p("EXPEDITED")
		if len(pv.Expedited) == 0 {
			p("  none")
		}
		for _, e := range pv.Expedited {
			p("  %s  %s (%s)  %s; by %s, since %s", expediteRef(e, pv.Line), e.Name, e.Class, expediteWhere(e), cmp.Or(e.By, "someone"), dur(int64(now.Sub(e.Since).Seconds())))
		}
	}

	p("")
	p("LINE (who gets room next, in order; #n = the job's place in the whole line)")
	if len(s.Waiting) == 0 {
		p("  nobody waits")
	}
	byClass := map[string][]JobWait{}
	for _, j := range s.WaitingJobs {
		byClass[j.Class] = append(byClass[j.Class], j)
	}
	for _, f := range fs {
		if f.Level != levelWait {
			continue
		}
		p("  %s", f.Text)
		var names []string
		for i, j := range byClass[f.Class] {
			if i == 6 {
				names = append(names, fmt.Sprintf("+%d more", len(byClass[f.Class])-6))
				break
			}
			n := cmp.Or(j.Job, "(name unknown)") + " " + dur(j.WaitedSeconds)
			if j.Level != "" {
				n += " " + j.Level
			}
			if j.Position > 0 {
				n = fmt.Sprintf("#%d %s", j.Position, n)
			}
			names = append(names, n)
		}
		if len(names) > 0 {
			p("      jobs: %s", strings.Join(names, ", "))
		}
		for _, st := range f.Next {
			p("      next: %s", stepText(st))
		}
	}

	p("")
	p("HOSTS (held = what runners hold; lent = budget, or the machine; free = what a new runner could take now)")
	for _, h := range s.Hosts {
		state := "unknown"
		if h.Reachable != nil {
			state = "ok"
			switch {
			case !*h.Reachable:
				state = "UNREACHABLE"
			case h.Closed != "":
				state = "closed: " + h.Closed
			}
		}
		lentCPU, lentMem := "?", "?" // not known while the host does not answer
		if h.BudgetCPUs > 0 {
			lentCPU = fmt.Sprintf("%g", h.BudgetCPUs)
		} else if h.CPUs > 0 {
			lentCPU = fmt.Sprintf("%g", h.CPUs)
		}
		if h.BudgetMem > 0 {
			lentMem = gib(h.BudgetMem)
		} else if h.MemBytes > 0 {
			lentMem = gib(h.MemBytes)
		}
		line := fmt.Sprintf("  %s  %s %s  %s  held %.1fcpu/%s of %scpu/%s", h.Name, h.Kind, cmp.Or(h.OS, "-"), state, h.UsedCPUs, gib(h.UsedMem), lentCPU, lentMem)
		if h.MemBytes > 0 {
			line += fmt.Sprintf("  free %.1fcpu/%s  others %.1fcpu/%s", h.FreeCPUs, gib(h.FreeMem), h.OtherCPUs, gib(h.OtherMem))
			line += fmt.Sprintf("  pressure cpu %s mem %s io %s", pct(h.CPUPressure), pct(h.MemPressure), pct(h.IOPressure))
		}
		if h.DiskFreePct != nil {
			line += fmt.Sprintf("  disk %.0f%% free", *h.DiskFreePct)
		}
		line += fmt.Sprintf("  %d runners", h.Runners)
		p("%s", line)
	}

	p("")
	p("RUNNERS")
	if len(s.Runners) == 0 {
		p("  none")
	}
	for _, r := range s.Runners {
		now := "-"
		if r.MemBytes > 0 {
			now = fmt.Sprintf("%.1fcpu/%s", r.CPUNow, gib(r.MemBytes))
		}
		est := fmt.Sprintf("%.1fcpu/%s", r.EstCPUs, gib(r.EstMem))
		switch {
		case r.Watched:
			est += " (first, watched)"
		case r.EstRuns == 0:
			est += " (first)"
		default:
			est += fmt.Sprintf(" (%d runs)", r.EstRuns)
		}
		flag := ""
		if r.EstMem > 0 && r.MemBytes > r.EstMem {
			flag = "  OVER ESTIMATE"
		}
		if r.MemLimit > 0 && float64(r.MemBytes) >= 0.9*float64(r.MemLimit) {
			flag += "  NEAR MEMORY LIMIT " + gib(r.MemLimit)
		}
		p("  %s  %s on %s  %s  %q  %s  now %s  held %.1fcpu/%s  est %s%s", r.Name, r.Class, r.Host, r.State, cmp.Or(r.Job, "-"), dur(r.AgeSeconds), now, r.HeldCPUs, gib(r.HeldMem), est, flag)
	}

	if len(s.Overflow) > 0 {
		p("")
		p("OVERFLOW (takes only what fits; jobs beyond that go to other runners of its labels or wait at GitHub)")
		for _, o := range s.Overflow {
			after := ""
			if o.After != "" {
				after = ", only while " + o.After + " has no room"
			}
			p("  %s  takes %d, holds %d%s", o.Class, o.Take, o.Held, after)
		}
	}

	writeFindings("NOTES", levelNote)

	p("")
	p("RESOLVED (last 24h)")
	if len(s.RecentIncidents) == 0 {
		p("  none")
	} else {
		by := map[string]int{}
		for _, in := range s.RecentIncidents {
			by[in.Kind]++
		}
		p("  %s", countList(by))
		for _, in := range s.RecentIncidents[max(0, len(s.RecentIncidents)-3):] {
			p("  %s ago  [%s] %s", dur(int64(now.Sub(in.ResolvedAt).Seconds())), in.Kind, in.Detail)
		}
	}
}
