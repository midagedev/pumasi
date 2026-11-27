package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/ledger"
)

// The line, and what operators change in it while `up` runs. The commands
// that change something call the running controller's API (opsapi.go):
// it keeps what they set in its ledger and applies it in its next round.

// parseAll parses flags that may come before, between or after the
// positional arguments, and returns the positional ones.
func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// call asks the running controller and decodes its answer into out. A
// refusal comes back as the controller's own words.
func call(cfg *config.Config, method, path string, body, out any) error {
	code, b, err := controller.Call(cfg, method, path, body)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s %s: %d %s", method, path, code, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// runExpedite puts a job before every other one, or takes it out again.
func runExpedite(args []string) error {
	fs := flag.NewFlagSet("pumasi expedite", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	clear := fs.Bool("clear", false, "take the job out of the front of the line")
	name := fs.String("name", "", "with a run's URL: the job's name in that run")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: pumasi expedite <job> [--name <job name>]
       pumasi expedite --clear <job>
       pumasi expedite            (the expedited jobs)

<job> is pumasi's job id (pumasi status --json: waitingJobs[].id), GitHub's
job id, the job's URL (https://github.com/OWNER/REPO/actions/runs/R/job/J),
or a run's URL with --name. The job goes before every other one; the hosts
it can go to take nothing new until it starts. Running jobs are not touched.
`)
		fs.PrintDefaults()
	}
	pos, err := parseAll(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	w := os.Stdout
	switch {
	case len(pos) == 0 && !*clear:
		var v controller.PriorityView
		if err := call(cfg, "GET", "/api/priority", nil, &v); err != nil {
			return err
		}
		printExpedited(w, v.Expedited, v.RecentlyExpedited)
		return nil
	case len(pos) != 1:
		fs.Usage()
		return flag.ErrHelp
	case *clear:
		var out struct {
			Cleared controller.Expedited `json:"cleared"`
		}
		if err := call(cfg, "DELETE", "/api/expedite?job="+url.QueryEscape(pos[0]), nil, &out); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s (run %d) is no longer expedited; it keeps its place by age and rules\n", out.Cleared.Name, out.Cleared.RunID)
		return nil
	}
	var res controller.ExpediteResult
	if err := call(cfg, "POST", "/api/expedite", map[string]string{"job": pos[0], "name": *name}, &res); err != nil {
		return err
	}
	where := fmt.Sprintf("position %d in line", res.Position)
	switch {
	case res.Pending:
		where = "GitHub still holds it for class " + res.Class
	case res.Position == 0:
		where = "a runner of its class that is up takes it next"
	}
	verb := "expedited"
	if res.Already {
		verb = "already expedited:"
	}
	fmt.Fprintf(w, "%s %s (run %d) of class %s: %s\n%s\n%s\n", verb, res.Name, res.RunID, res.Class, where, res.Note, res.URL)
	return nil
}

// runMode shows or switches the mode.
func runMode(args []string) error {
	fs := flag.NewFlagSet("pumasi mode", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	forDur := fs.Duration("for", 0, "high-only: how long (default 1h, at most 24h); it ends by itself")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: pumasi mode                       the mode now
       pumasi mode high-only [--for 30m]   only expedited and high jobs get new runners
       pumasi mode normal                  every job again

Running jobs go on in every mode.
`)
		fs.PrintDefaults()
	}
	pos, err := parseAll(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var m controller.Mode
	switch len(pos) {
	case 0:
		var v controller.PriorityView
		if err := call(cfg, "GET", "/api/priority", nil, &v); err != nil {
			return err
		}
		m = v.Mode
	case 1:
		body := map[string]string{"name": pos[0]}
		if *forDur > 0 {
			body["for"] = forDur.String()
		}
		if err := call(cfg, "POST", "/api/mode", body, &m); err != nil {
			return err
		}
	default:
		fs.Usage()
		return flag.ErrHelp
	}
	fmt.Println(modeLine(m, time.Now()))
	return nil
}

func modeLine(m controller.Mode, now time.Time) string {
	if m.Until == nil {
		s := "mode " + m.Name
		if m.By != "" && m.At != nil {
			s += fmt.Sprintf(" (since %s, %s)", m.At.Local().Format("15:04"), m.By)
		}
		return s
	}
	return fmt.Sprintf("mode %s until %s (%s more, set by %s): only expedited and high jobs get new runners; running jobs go on",
		m.Name, m.Until.Local().Format("15:04"), m.Until.Sub(now).Round(time.Second), m.By)
}

// runPriority shows the line, the rules and what operators set, or adds and
// removes runtime rules.
func runPriority(args []string) error {
	if len(args) > 0 && (args[0] == "add" || args[0] == "rm") {
		return runRuleEdit(args[0], args[1:])
	}
	fs := flag.NewFlagSet("pumasi priority", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var v controller.PriorityView
	if err := call(cfg, "GET", "/api/priority", nil, &v); err != nil {
		// No controller: what the ledger says, in the order the rules give.
		if _, serr := os.Stat(cfg.Ledger); serr != nil {
			v, err = controller.PriorityView{Mode: controller.Mode{Name: controller.ModeNormal}}, nil
			for _, r := range cfg.Rules(nil) {
				v.Rules = append(v.Rules, controller.RuleView{ID: r.ID, Source: "file", Match: r.Match, Level: r.Level, YAML: r.YAML()})
			}
		} else {
			l, lerr := ledger.Open(cfg.Ledger)
			if lerr != nil {
				return lerr
			}
			defer l.Close()
			if v, err = controller.CollectPriority(cfg, l, controller.Status{}, false, time.Now()); err != nil {
				return err
			}
		}
		fmt.Println("no controller running; the ledger alone, in the order the rules give")
		fmt.Println()
	}
	printPriority(os.Stdout, cfg, v)
	return nil
}

func printPriority(w io.Writer, cfg *config.Config, v controller.PriorityView) {
	now := time.Now()
	fmt.Fprintln(w, modeLine(v.Mode, now))
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(v.Expedited) > 0 {
		printExpedited(w, v.Expedited, nil)
		fmt.Fprintln(w)
	}
	if len(v.Rules) == 0 {
		fmt.Fprintf(w, "no priority rules: jobs get room oldest first. Add one for now with\n")
		fmt.Fprintf(w, "  pumasi priority add workflow='deploy-*.yml' --level high --for 2h\n")
		fmt.Fprintf(w, "or for good under priority: in %s:\n", cfg.Path)
		fmt.Fprintln(w, "  priority:\n    - {match: {workflow: \"deploy-*.yml\"}, level: high}\n    - {match: {event: schedule}, level: low}")
	} else {
		fmt.Fprintln(tw, "RULE\tLEVEL\tMATCH\tUNTIL\tFROM")
		var keep []controller.RuleView
		for _, r := range v.Rules {
			until, from := "-", cfg.Path
			if r.Source == "runtime" {
				until, from = "removed", r.By
				if r.Expires != nil {
					until = r.Expires.Local().Format("01-02 15:04")
				}
				keep = append(keep, r)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Level, matchText(r.Match), until, from)
		}
		tw.Flush()
		fmt.Fprintf(w, "\nThe first rule that matches a job counts; runtime rules (r-) go before the file's.\n")
		fmt.Fprintf(w, "A high job is placed as if it had come %s earlier, a low one %s later: nobody waits forever.\n", v.HeadStart, v.HeadStart)
		if len(keep) > 0 {
			fmt.Fprintf(w, "To keep a runtime rule, put its line under priority: in %s and remove it here (pumasi priority rm <id>):\n", cfg.Path)
			for _, r := range keep {
				fmt.Fprintf(w, "  %s    # %s\n", r.YAML, r.ID)
			}
		}
	}
	fmt.Fprintln(w, "Rules and expedites order classes, not the jobs inside one: GitHub gives a class its oldest job first,")
	fmt.Fprintln(w, "so the jobs before an urgent one in its class move up with it. No running job is stopped for them.")

	fmt.Fprintln(w)
	if len(v.Line) == 0 {
		fmt.Fprintln(w, "no jobs waiting")
		return
	}
	fmt.Fprintln(tw, "LINE\tCLASS\tJOB\tWORKFLOW\tLEVEL\tWAITED\tWHY HERE\tWAITS FOR")
	for _, j := range v.Line {
		pos := "-"
		if j.Position > 0 {
			pos = fmt.Sprint(j.Position)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", pos, j.Class, dash(j.Name), dash(j.Workflow), j.Level, dash(j.Waited), j.Why, dash(j.WaitingFor))
	}
	tw.Flush()
}

func printExpedited(w io.Writer, active, recent []controller.Expedited) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	now := time.Now()
	if len(active) == 0 {
		fmt.Fprintln(w, "no job is expedited")
	} else {
		fmt.Fprintln(tw, "EXPEDITED\tRUN\tCLASS\tSINCE\tBY\tAHEAD\tWHERE")
		for _, x := range active {
			where := fmt.Sprintf("line %d", x.Position)
			switch {
			case x.Pending:
				where = "GitHub holds it"
			case x.Position == 0:
				where = "a runner is up for it"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s ago\t%s\t%d\t%s\n", x.Name, x.RunID, x.Class, now.Sub(x.Since).Round(time.Second), x.By, x.Ahead, where)
		}
		tw.Flush()
	}
	if len(recent) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(tw, "LATELY\tRUN\tCLASS\tEXPEDITED\tBY\tENDED\tSTARTED AFTER")
		for _, x := range recent {
			after := "-"
			if x.AfterSeconds != nil {
				after = (time.Duration(*x.AfterSeconds) * time.Second).String()
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", x.Name, x.RunID, x.Class, x.Since.Local().Format("01-02 15:04"), x.By, x.Ended, after)
		}
		tw.Flush()
	}
}

func matchText(m config.RuleMatch) string { return config.PriorityRule{Match: m}.String() }

// runRuleEdit adds or removes a runtime rule:
//
//	pumasi priority add workflow='deploy-*.yml' [class=… job=… …] --level high [--for 2h]
//	pumasi priority rm r-3
func runRuleEdit(verb string, args []string) error {
	fs := flag.NewFlagSet("pumasi priority "+verb, flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	level := fs.String("level", "", "add: high, normal or low")
	forDur := fs.Duration("for", 0, "add: the rule ends by itself after this long (default: until removed)")
	pos, err := parseAll(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if verb == "rm" {
		if len(pos) != 1 {
			return fmt.Errorf("usage: pumasi priority rm <r-id>")
		}
		if err := call(cfg, "DELETE", "/api/priority/rules/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("rule %s removed; the jobs waiting are ordered again in the next round\n", pos[0])
		return nil
	}
	var m config.RuleMatch
	fields := map[string]*string{"class": &m.Class, "repo": &m.Repo, "workflow": &m.Workflow, "job": &m.Job, "ref": &m.Ref, "branch": &m.Branch, "event": &m.Event}
	for _, kv := range pos {
		k, v, ok := strings.Cut(kv, "=")
		f, known := fields[k]
		if !ok || !known {
			return fmt.Errorf("%q: match keys are class=, repo=, workflow=, job=, ref=, branch=, event=", kv)
		}
		*f = v
	}
	body := map[string]any{"match": m, "level": *level}
	if *forDur > 0 {
		body["for"] = forDur.String()
	}
	var r controller.RuleView
	if err := call(cfg, "POST", "/api/priority/rules", body, &r); err != nil {
		return err
	}
	until := "until removed (pumasi priority rm " + r.ID + ")"
	if r.Expires != nil {
		until = "until " + r.Expires.Local().Format("01-02 15:04")
	}
	fmt.Printf("rule %s: %s for %s, before the file's rules, %s\nto keep it, put under priority: in %s\n  %s\n", r.ID, r.Level, matchText(r.Match), until, cfg.Path, r.YAML)
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
