package controller

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
)

// The dashboard's writes: expedite a job, switch the mode, add and remove
// runtime priority rules. Reads are open to whoever reaches the dashboard,
// as before; writes come from this machine only, or with dashboardToken.

// priorityRoutes adds the priority API ("priority" among the dashboard's
// apis) to the dashboard's routes.
func (c *Controller) priorityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/priority", func(w http.ResponseWriter, _ *http.Request) {
		v, err := CollectPriority(c.cfgSnapshot(), c.ledger, c.liveStatus(), true, c.now())
		reply(w, v, err)
	})
	mux.HandleFunc("POST /api/expedite", c.write(func(r *http.Request, by string) (any, error) {
		var body struct {
			Job  json.RawMessage `json:"job"`
			Name string          `json:"name"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		return c.expedite(r.Context(), rawRef(body.Job), body.Name, by, c.now())
	}))
	mux.HandleFunc("DELETE /api/expedite", c.write(func(r *http.Request, by string) (any, error) {
		e, err := c.clearExpedite(r.URL.Query().Get("job"), by, c.now())
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "cleared": expeditedOf(e)}, nil
	}))
	mux.HandleFunc("POST /api/mode", c.write(func(r *http.Request, by string) (any, error) {
		var body struct {
			Name string `json:"name"`
			For  string `json:"for"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		d, err := parseFor(body.For)
		if err != nil {
			return nil, err
		}
		return c.setMode(body.Name, d, by, c.now())
	}))
	mux.HandleFunc("POST /api/priority/rules", c.write(func(r *http.Request, by string) (any, error) {
		pr, d, err := ruleBody(r)
		if err != nil {
			return nil, err
		}
		rule, err := c.addRule(pr, d, by, c.now())
		return ruleView(rule), err
	}))
	mux.HandleFunc("PUT /api/priority/rules/{id}", c.write(func(r *http.Request, by string) (any, error) {
		pr, d, err := ruleBody(r)
		if err != nil {
			return nil, err
		}
		rule, err := c.replaceRule(r.PathValue("id"), pr, d, by, c.now())
		return ruleView(rule), err
	}))
	mux.HandleFunc("DELETE /api/priority/rules/{id}", c.write(func(r *http.Request, by string) (any, error) {
		if err := c.removeRule(r.PathValue("id"), by, c.now()); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "removed": r.PathValue("id")}, nil
	}))
}

// cfgSnapshot is the config with the priority rules of the moment (the file
// is read again while up runs).
func (c *Controller) cfgSnapshot() *config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg := *c.cfg
	cfg.Priority = append([]config.PriorityRule{}, c.cfg.Priority...)
	return &cfg
}

// write serves a request that changes something: it checks who asks and
// answers JSON, a refusal as {"ok": false, "error": "..."}.
func (c *Controller) write(h func(r *http.Request, by string) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if code, why := mayWrite(c.cfg, r); code != 0 {
			writeError(w, code, why)
			return
		}
		// A page elsewhere can make a browser send a form here, but not JSON:
		// that needs a preflight this server never answers.
		if r.Method != http.MethodDelete {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "send JSON (Content-Type: application/json)")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		v, err := h(r, requester(r))
		if err != nil {
			var oe *opsError
			if errors.As(err, &oe) {
				writeError(w, oe.code, oe.msg)
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, v)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// mayWrite says whether a request may change something, and if not the
// status and why. This machine may, unless the dashboard listens on other
// addresses and has a token: then everyone shows the token.
// Elsewhere, only with the token.
func mayWrite(cfg *config.Config, r *http.Request) (int, string) {
	token := cfg.DashboardToken
	if token != "" {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			return 0, ""
		}
		if !onLoopback(cfg.Dashboard) {
			return http.StatusUnauthorized, "writes need Authorization: Bearer <dashboardToken of the config>"
		}
	}
	if !fromLoopback(r) {
		if token != "" {
			return http.StatusUnauthorized, "writes from another machine need Authorization: Bearer <dashboardToken of the config>"
		}
		return http.StatusForbidden, "writes come only from the controller's machine; set dashboardToken in the config to take them from elsewhere"
	}
	// A page of another site whose name was pointed at this machine (DNS
	// rebinding) reaches loopback from the browser; it cannot send an
	// address as the host it asked for.
	if !localHost(r.Host) {
		return http.StatusForbidden, "writes are taken only at an address (127.0.0.1, localhost), not at the name " + r.Host
	}
	return 0, ""
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// onLoopback says whether a listen address takes only this machine's connections.
func onLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localHost: the Host a request was sent to is an address or localhost.
func localHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || net.ParseIP(host) != nil
}

// requester is who a change is recorded as made by: the client (the CLI
// says it is one), its user when it says so, and the address it came from.
func requester(r *http.Request) string {
	client := "api"
	if v := r.Header.Get("X-Pumasi-Client"); v == "cli" || v == "web" {
		client = v
	}
	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		addr = r.RemoteAddr
	}
	if u := clean(r.Header.Get("X-Pumasi-User")); u != "" {
		addr = u + "@" + addr
	}
	return client + " " + addr
}

// clean keeps a short run of plain characters of what a client says of itself.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 32 {
			break
		}
		if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return opsErr(http.StatusBadRequest, "body: %v", err)
	}
	return nil
}

// rawRef reads the job an operator names: a string, or GitHub's job id as a number.
func rawRef(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func parseFor(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, opsErr(http.StatusBadRequest, "for: a duration like 30m or 2h")
	}
	return d, nil
}

func ruleBody(r *http.Request) (config.PriorityRule, time.Duration, error) {
	var body struct {
		Match config.RuleMatch `json:"match"`
		Level string           `json:"level"`
		For   string           `json:"for"`
	}
	if err := decodeBody(r, &body); err != nil {
		return config.PriorityRule{}, 0, err
	}
	d, err := parseFor(body.For)
	return config.PriorityRule{Match: body.Match, Level: body.Level}, d, err
}

// PriorityView is GET /api/priority: the mode, the rules in the order they
// are tried, the expedited jobs and the line.
type PriorityView struct {
	Mode      Mode        `json:"mode"`
	HeadStart string      `json:"headStart"`
	File      string      `json:"file"` // where the file's rules are written
	Rules     []RuleView  `json:"rules"`
	Expedited []Expedited `json:"expedited"`
	// RecentlyExpedited: the last that started or ended, with how long after
	// they were expedited they started.
	RecentlyExpedited []Expedited `json:"recentlyExpedited"`
	Line              []LineJob   `json:"line"`
	// Controller: a running up drew the line ("" positions are its last round);
	// false = the ledger alone, in the order the rules give.
	Controller bool `json:"controller"`
}

type RuleView struct {
	ID      string           `json:"id"`
	Source  string           `json:"source"` // runtime | file
	Match   config.RuleMatch `json:"match"`
	Level   string           `json:"level"`
	Expires *time.Time       `json:"expires"`
	By      string           `json:"by,omitempty"`
	At      *time.Time       `json:"at,omitempty"`
	// YAML: the rule as a line under priority: in the config file.
	YAML string `json:"yaml"`
}

func ruleView(r config.Rule) RuleView {
	v := RuleView{ID: r.ID, Source: "file", Match: r.Match, Level: r.Level, By: r.By, YAML: r.YAML()}
	if r.Runtime {
		v.Source = "runtime"
		at := r.At
		v.At = &at
	}
	if !r.Expires.IsZero() {
		exp := r.Expires
		v.Expires = &exp
	}
	return v
}

// LineJob is one waiting job in the order of the line.
type LineJob struct {
	Job         string `json:"job"` // pumasi's id ("x-<n>" for an expedited job GitHub still holds)
	GitHubJobID int64  `json:"githubJobId,omitempty"`
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	RunID       int64  `json:"runId"`
	Workflow    string `json:"workflow"`
	Class       string `json:"class"`
	Level       string `json:"level"` // expedite | high | normal | low
	// Position: its place in the line (1 = the next room that fits it); 0 =
	// a runner of its class that is up takes it next.
	Position      int    `json:"position"`
	Waited        string `json:"waited"`
	WaitedSeconds int64  `json:"waitedSeconds"`
	Why           string `json:"why"`        // why it stands there
	WaitingFor    string `json:"waitingFor"` // what it waits for ("" = a runner starts for it now)
	Pending       bool   `json:"pending,omitempty"`
	// For /api/state: the rule or job behind its level ("" = its age), and
	// whether it was expedited by hand (not only pulled along).
	ranked    string
	expedited bool
}

func levelWord(l decide.Lined) string {
	switch {
	case l.Expedite > 0:
		return "expedite"
	case l.Level > 0:
		return "high"
	case l.Level < 0:
		return "low"
	}
	return "normal"
}

// CollectPriority is the priority view from the ledger, and with a running
// controller (live, ok) the line as its last round drew it.
func CollectPriority(cfg *config.Config, l *ledger.Ledger, live Status, ok bool, now time.Time) (PriorityView, error) {
	v := PriorityView{Mode: CurrentMode(l, now), HeadStart: shortDur(cfg.PriorityHeadStart), File: cfg.Path, Controller: ok,
		Rules: []RuleView{}, Expedited: []Expedited{}, RecentlyExpedited: []Expedited{}, Line: []LineJob{}}
	rt, err := RuntimeRules(l, now)
	if err != nil {
		return v, err
	}
	rules := cfg.Rules(rt)
	for _, r := range rules {
		v.Rules = append(v.Rules, ruleView(r))
	}
	line, active, err := lineJobs(cfg, l, rules, live, ok, now)
	if err != nil {
		return v, err
	}
	v.Line = line
	for _, e := range active {
		x := expeditedOf(e)
		x.Pending = true
		for _, j := range line {
			if j.Repo == e.Repo && j.RunID == e.RunID && j.Name == e.Job || j.Job == phantomID(e.ID) {
				x.Position, x.Pending = j.Position, j.Pending
				if !j.Pending {
					x.Job = j.Job
				}
			}
		}
		for _, j := range line {
			if j.Class == x.Class && j.Level == "expedite" && j.Position > 0 && j.Position < x.Position && strings.HasPrefix(j.Why, "ahead of "+e.Job+" ") {
				x.Ahead++
			}
		}
		v.Expedited = append(v.Expedited, x)
	}
	recent, err := l.RecentExpedites(10)
	if err != nil {
		return v, err
	}
	for _, e := range recent {
		v.RecentlyExpedited = append(v.RecentlyExpedited, expeditedOf(e))
	}
	return v, nil
}

// lineJobs is every waiting job in the order of the line: as a running
// controller's last round drew it, or else as the rules order the ledger's.
func lineJobs(cfg *config.Config, l *ledger.Ledger, rules []config.Rule, live Status, ok bool, now time.Time) ([]LineJob, []ledger.Expedite, error) {
	open, err := l.OpenDemand()
	if err != nil {
		return nil, nil, err
	}
	active, err := l.ActiveExpedites()
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]ledger.Demand{}
	for _, d := range open {
		byID[d.JobID] = d
	}
	exp := map[string]ledger.Expedite{}
	for _, e := range active {
		exp[phantomID(e.ID)] = e
		exp[e.Repo+"\x00"+strconv.FormatInt(e.RunID, 10)+"\x00"+e.Job] = e
	}
	lined := live.Line
	if !ok {
		for _, d := range decide.Line(lineup(open, active, rules), nil, cfg.PriorityHeadStart) {
			lined = append(lined, decide.Lined{ID: d.ID, Class: d.Class, Name: d.Name, Level: d.Level, Expedite: d.Expedite, Pending: d.Pending, Ranked: d.Ranked})
		}
	}
	out := []LineJob{}
	seen := map[string]bool{}
	add := func(l decide.Lined, pos int) {
		j := LineJob{Job: l.ID, Name: l.Name, Class: l.Class, Level: levelWord(l), Position: pos, Why: cmpOr(l.Ranked, "by its age"),
			WaitingFor: l.Reason, Pending: l.Pending, ranked: l.Ranked}
		var since time.Time
		if d, ok := byID[l.ID]; ok {
			j.Repo, j.RunID, j.Workflow, since = d.Repo, d.RunID, workflowFile(d.Workflow), d.FirstSeen
			if e, ok := exp[d.Repo+"\x00"+strconv.FormatInt(d.RunID, 10)+"\x00"+d.Job]; ok {
				j.GitHubJobID, j.expedited = e.GHJobID, true
			}
		} else if e, ok := exp[l.ID]; ok {
			j.Repo, j.RunID, j.Workflow, j.GitHubJobID, since = e.Repo, e.RunID, workflowFile(e.Workflow), e.GHJobID, cmpTime(e.QueuedAt, e.At)
			j.expedited = true
		} else {
			return // gone since that round
		}
		if !since.IsZero() {
			j.WaitedSeconds = int64(now.Sub(since).Seconds())
			j.Waited = (time.Duration(j.WaitedSeconds) * time.Second).String()
		}
		seen[l.ID] = true
		out = append(out, j)
	}
	for i, l := range lined {
		add(l, i+1)
	}
	// Jobs that runners already up will take, and jobs that came after that round.
	for _, d := range open {
		if !seen[d.JobID] {
			add(decide.Lined{ID: d.JobID, Class: d.Class, Name: d.Job, Reason: "a runner of its class that is up takes it next, or it came after the last round"}, 0)
		}
	}
	return out, active, nil
}

func workflowFile(ref string) string {
	w := ledger.KeyOf("", ref, "").Workflow
	if i := strings.LastIndex(w, "/"); i >= 0 {
		return w[i+1:]
	}
	return w
}

func cmpTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	return a
}

// shortDur writes 30m for 30m0s.
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Call sends a request to the running controller's API, as the CLI does:
// with the config's token when it has one. It returns the answer's status
// and body; err only when no controller answered.
func Call(cfg *config.Config, method, path string, body any) (int, []byte, error) {
	b, err := os.ReadFile(addrFile(cfg))
	if err != nil {
		return 0, nil, errors.New("no controller running (`pumasi up`): expedite, mode and runtime rules are kept by it")
	}
	var rd io.Reader
	if body != nil {
		j, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, "http://"+strings.TrimSpace(string(b))+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Pumasi-Client", "cli")
	if u := os.Getenv("USER"); u != "" {
		req.Header.Set("X-Pumasi-User", u)
	} else if u := os.Getenv("USERNAME"); u != "" {
		req.Header.Set("X-Pumasi-User", u)
	}
	if cfg.DashboardToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.DashboardToken)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("no controller answered at %s: %w", strings.TrimSpace(string(b)), err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, out, err
}
