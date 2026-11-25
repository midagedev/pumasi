package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/join"
)

// runRegister hands a registration token to the running controller, which
// passes it to the connected hosts; each registers runners for the empty
// slots of its classes. The token is one GitHub gives out for an hour to
// register runners with and nothing else; it is not kept.
func runRegister(args []string) error {
	fs := flag.NewFlagSet("pumasi register", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	class := fs.String("class", "", "only this class (default: every class with slots:)")
	host := fs.String("host", "", "only this host (default: every connected host)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("the token is not given as an argument (it would show in the process list): put it in %s or pipe it in", controller.RunnerTokenEnv)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(os.Getenv(controller.RunnerTokenEnv))
	if token == "" {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			token = strings.TrimSpace(line)
		}
	}
	if token == "" {
		return fmt.Errorf(`no registration token. GitHub shows one under Settings > Actions > Runners > "New self-hosted runner" (the value after --token), or:
  gh api -X POST %s/registration-token -q .token | pumasi register
It can also be given in %s. It works for an hour and only registers runners`, registrationAPI(cfg.URL), controller.RunnerTokenEnv)
	}
	out, err := controller.RequestRegister(cfg, token, *class, *host)
	if err != nil {
		return err
	}
	bad := 0
	for _, o := range out {
		switch {
		case o.Problem != "":
			bad++
			fmt.Printf("%s  FAILED  %s", o.Host, o.Problem)
			if len(o.Registered) > 0 {
				fmt.Printf("  (registered before that: %s)", strings.Join(o.Registered, ", "))
			}
			fmt.Println()
		case len(o.Registered) == 0:
			fmt.Printf("%s  nothing to register (%d runners already there)\n", o.Host, o.Had)
		default:
			fmt.Printf("%s  registered %s\n", o.Host, strings.Join(o.Registered, ", "))
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d hosts could not register", bad, len(out))
	}
	return nil
}

// registrationAPI is the gh api path that gives a registration token for url.
func registrationAPI(url string) string {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), "/"), "/")
	switch {
	case len(parts) == 3 && parts[1] == "enterprises":
		return "enterprises/" + parts[2] + "/actions/runners"
	case len(parts) == 3:
		return "repos/" + parts[1] + "/" + parts[2] + "/actions/runners"
	case len(parts) == 2:
		return "orgs/" + parts[1] + "/actions/runners"
	}
	return "repos/OWNER/REPO/actions/runners"
}

// runAdopt takes over runners that are already registered: it copies the
// credentials from their directories into slots of a class. Nothing changes
// on GitHub (name, labels, id stay) and nothing in those directories.
func runAdopt(args []string) error {
	fs := flag.NewFlagSet("pumasi adopt", flag.ContinueOnError)
	class := fs.String("class", "", "the class whose slots the runners take (default: the host's only class with slots:)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: pumasi adopt [-class NAME] <runner directory>...   (the directory that holds .runner and .credentials)")
	}
	p := paths()
	if _, err := join.LoadHostConfig(p.conf); err != nil {
		return errors.New("this host has not joined a controller yet: run the line of `pumasi invite` here first")
	}
	plan := agent.ReadSlotPlan(p.dir)
	store := agent.SlotStore{Dir: agent.SlotsDir(p.dir)}
	limit := 0
	if plan == nil || len(plan.Classes) == 0 {
		if *class == "" {
			return errors.New("the controller has not told this host its classes yet (is `pumasi up` running, with a class that has slots:?); name the class with -class to adopt anyway")
		}
		fmt.Printf("note: the controller has not told this host of a class %s with slots: yet; the runner listens once it has\n", *class)
	} else {
		var names []string
		for _, c := range plan.Classes {
			names = append(names, c.Name)
			if c.Name == *class || (*class == "" && len(plan.Classes) == 1) {
				*class, limit = c.Name, c.Slots
			}
		}
		if limit == 0 {
			return fmt.Errorf("name one of this host's classes with slots: with -class (%s)", strings.Join(names, ", "))
		}
	}
	for _, dir := range fs.Args() {
		dir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		rs, err := agent.ReadRunnerSettings(dir)
		if err != nil {
			return fmt.Errorf("%s is not a configured runner: %w", dir, err)
		}
		if plan != nil && !sameURL(plan.URL, rs.GitHubURL) {
			return fmt.Errorf("%s is registered at %s, this host's controller serves %s", rs.AgentName, rs.GitHubURL, plan.URL)
		}
		if pid := agent.ListenerPid(dir); pid != 0 {
			how := "stop it"
			if b, err := os.ReadFile(filepath.Join(dir, ".service")); err == nil && strings.TrimSpace(string(b)) != "" {
				how = fmt.Sprintf("stop its service (cd %s && sudo ./svc.sh stop — it is %s)", dir, filepath.Base(strings.TrimSpace(string(b))))
			}
			return fmt.Errorf("%s is listening from %s (pid %d). Two listeners of one registration fight over its session: %s, then run this again", rs.AgentName, dir, pid, how)
		}
		taken := false
		for _, m := range store.List() {
			if m.Name == rs.AgentName && m.URL == rs.GitHubURL {
				fmt.Printf("%s is slot %s already\n", m.Name, m.Key())
				taken = true
			}
		}
		if taken {
			continue
		}
		n := store.Free(*class)
		if limit > 0 && n > limit {
			return fmt.Errorf("class %s has slots: %d and all of them hold a runner; raise slots: in the controller's config", *class, limit)
		}
		m, err := store.Add(*class, n, agent.OriginAdopted, dir, true)
		if err != nil {
			return err
		}
		fmt.Printf("adopted %s as %s slot %d. It keeps its name, id and labels on GitHub; from now on it listens from a fresh copy of the runner for every job.\n", m.Name, m.Class, m.N)
		fmt.Printf("  %s is not changed. Leave its old service stopped. To go back: pumasi release %s, then start that service again.\n", dir, m.Name)
	}
	return nil
}

func sameURL(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }
	return norm(a) == norm(b)
}

// runRelease gives slots up again: the agent stops their listeners when no
// job can be lost and frees them. An adopted runner's files are where they
// were adopted from; those of a runner pumasi registered are kept.
func runRelease(args []string) error {
	fs := flag.NewFlagSet("pumasi release", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := paths()
	store := agent.SlotStore{Dir: agent.SlotsDir(p.dir)}
	all := store.List()
	if fs.NArg() == 0 {
		if len(all) == 0 {
			return errors.New("this host keeps no registered runners")
		}
		fmt.Println("usage: pumasi release <runner>...   This host keeps:")
		for _, m := range all {
			fmt.Printf("  %s  (%s slot %d, %s)\n", m.Name, m.Class, m.N, m.Origin)
		}
		return nil
	}
	var asked []agent.SlotMeta
	for _, want := range fs.Args() {
		found := false
		for _, m := range all {
			if m.Name == want || m.Key() == want {
				if err := store.AskRelease(m); err != nil {
					return err
				}
				asked, found = append(asked, m), true
			}
		}
		if !found {
			return fmt.Errorf("this host keeps no runner %s (`pumasi release` lists them)", want)
		}
	}
	if running, how := serviceRunning(p); !running {
		return fmt.Errorf("asked, but the agent is not running (%s) and it is the agent that stops a listener safely: `pumasi doctor` says what is wrong", how)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, m := range asked {
		for {
			if _, err := os.Stat(store.Path(m)); err != nil {
				where := m.From
				if m.Origin != agent.OriginAdopted {
					where = filepath.Join(keepDir(), m.Name)
				}
				fmt.Printf("released %s: it is still registered on GitHub and its files are in %s\n", m.Name, where)
				if m.Origin == agent.OriginAdopted {
					fmt.Printf("  to run it the old way again, start its service there (cd %s && sudo ./svc.sh start, or ./run.sh)\n", where)
				}
				break
			}
			if ctx.Err() != nil {
				fmt.Printf("%s is not released yet: it runs a job, and is released when that job ends\n", m.Name)
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil
}
