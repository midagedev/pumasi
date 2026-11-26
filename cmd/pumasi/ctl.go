package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/ghauth"
)

// The controller as a service: `pumasi up`, kept running by the system's own
// service manager. It needs the GitHub token without a person's shell, so the
// token lives in a file only this user can read.

// tokenFile is where the controller's GitHub token is kept for a service.
func tokenFile(cfg *config.Config) string { return filepath.Join(cfg.StateDir(), "github-token") }

// githubToken is the token `up` and `down` work with: PUMASI_GITHUB_TOKEN, or
// the file a `pumasi service install` left. It also says where it came from.
func githubToken(cfg *config.Config) (token, from string, err error) {
	if t := os.Getenv("PUMASI_GITHUB_TOKEN"); t != "" {
		return t, "PUMASI_GITHUB_TOKEN", nil
	}
	path := tokenFile(cfg)
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("PUMASI_GITHUB_TOKEN is empty; e.g. PUMASI_GITHUB_TOKEN=$(gh auth token) pumasi up")
	}
	if err != nil {
		return "", "", err
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return "", "", fmt.Errorf("%s holds the GitHub token and others can read it (mode %o); chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if token = strings.TrimSpace(string(b)); token == "" {
		return "", "", fmt.Errorf("%s is empty", path)
	}
	return token, path, nil
}

// githubAuth is what `up` and `down` prove themselves to GitHub with: a
// GitHub App (githubApp: in the config, or PUMASI_GITHUB_APP_ID,
// PUMASI_GITHUB_APP_INSTALLATION_ID and PUMASI_GITHUB_APP_PRIVATE_KEY_FILE),
// or else a token. The App's key is named by its file, never by its content.
func githubAuth(cfg *config.Config) (ghauth.Auth, error) {
	app, from := cfg.GitHubApp, "githubApp in "+cfg.Path
	if !filepath.IsAbs(app.PrivateKeyFile) && app.PrivateKeyFile != "" {
		app.PrivateKeyFile = filepath.Join(filepath.Dir(cfg.Path), app.PrivateKeyFile)
	}
	env := false
	if v := os.Getenv("PUMASI_GITHUB_APP_ID"); v != "" {
		app.ID, env = v, true
	}
	if v := os.Getenv("PUMASI_GITHUB_APP_INSTALLATION_ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return ghauth.Auth{}, errors.New("PUMASI_GITHUB_APP_INSTALLATION_ID is not a number")
		}
		app.InstallationID, env = n, true
	}
	if v := os.Getenv("PUMASI_GITHUB_APP_PRIVATE_KEY_FILE"); v != "" {
		app.PrivateKeyFile, env = v, true
	}
	if env {
		from = "PUMASI_GITHUB_APP_*"
	}
	if !app.Set() {
		token, from, err := githubToken(cfg)
		return ghauth.Auth{Token: token, From: from}, err
	}
	if os.Getenv("PUMASI_GITHUB_TOKEN") != "" {
		return ghauth.Auth{}, fmt.Errorf("both a GitHub App (%s) and PUMASI_GITHUB_TOKEN are given; unset one, so that it is clear who pumasi is to GitHub", from)
	}
	a, err := ghauth.LoadApp(app.ID, app.InstallationID, app.PrivateKeyFile)
	if err != nil {
		return ghauth.Auth{}, fmt.Errorf("GitHub App (%s): %w", from, err)
	}
	return ghauth.Auth{App: a, From: from}, nil
}

// keepToken writes the token where the service reads it. The file is made
// with its final mode: it is never readable by anyone else, not for a moment.
func keepToken(cfg *config.Config, token string) error {
	path := tokenFile(cfg)
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

const serviceUsage = `usage: pumasi service install|remove|status [-config pumasi.yaml]

  install  keep ` + "`pumasi up`" + ` running: started again when it dies and after a reboot.
           The GitHub token is taken from PUMASI_GITHUB_TOKEN once and kept in a
           file only this user can read (<state dir>/github-token).
  remove   stop it and remove the service and the kept token (hosts and runners
           are not touched; ` + "`pumasi down`" + ` removes those)
  status   whether it runs, since when, how often it was started again`

// ctlService is one controller's service, as this OS keeps it.
type ctlService struct {
	cfg  *config.Config
	conf string // the config file, absolute
	exe  string
	log  string // where its output goes when the service manager keeps none
}

func runService(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(serviceUsage)
	}
	verb := args[0]
	fs := flag.NewFlagSet("pumasi service "+verb, flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	conf, err := filepath.Abs(*cfgPath)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	s := ctlService{cfg: cfg, conf: conf, exe: exe, log: filepath.Join(cfg.StateDir(), "up.log")}
	switch verb {
	case "install":
		return s.install()
	case "remove":
		if err := s.stop(); err != nil {
			return err
		}
		kept := "no token was kept"
		if err := os.Remove(tokenFile(cfg)); err == nil {
			kept = "the kept token is deleted"
		}
		fmt.Printf("removed: the controller is stopped and is not started again; %s.\nHosts keep their agents and running jobs go on; `pumasi up` serves them again, `pumasi down` removes everything.\n", kept)
		return nil
	case "status":
		return s.status()
	}
	return errors.New(serviceUsage)
}

func (s ctlService) install() error {
	if err := os.MkdirAll(s.cfg.StateDir(), 0o700); err != nil {
		return err
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(s.exe), "pumasi-*-*")); len(m) == 0 {
		return fmt.Errorf("no host builds next to %s: hosts fetch pumasi-<os>-<arch> from there when they join; run `make` in the pumasi source tree and use ./dist/pumasi", s.exe)
	}
	if err := s.can(); err != nil {
		return err
	}
	auth, err := githubAuth(s.cfg)
	if err != nil {
		return fmt.Errorf("the service needs GitHub credentials — a token once: PUMASI_GITHUB_TOKEN=$(gh auth token) pumasi service install, or githubApp: in the config (%w)", err)
	}
	if auth.App != nil && auth.From == "PUMASI_GITHUB_APP_*" {
		return errors.New("the service does not have this shell's environment: put the App in the config (githubApp: {id, installationId, privateKeyFile})")
	}
	token, from := auth.Token, auth.From
	// Installing again replaces the service; a controller a person started is theirs to stop.
	if err := s.stop(); err != nil {
		return err
	}
	unlock, err := controller.Lock(s.cfg.StateDir())
	if err != nil {
		return fmt.Errorf("%w — the service would fight it for the same hosts and scale sets", err)
	}
	unlock()
	creds := fmt.Sprintf("%s (mode 600; taken from %s)", tokenFile(s.cfg), from)
	switch {
	case auth.App != nil:
		// The key stays where it is; a token kept by an earlier install is not needed any more.
		_ = os.Remove(tokenFile(s.cfg))
		creds = fmt.Sprintf("%s, its key read from the file the config names", auth.What())
	case from != tokenFile(s.cfg):
		if err := keepToken(s.cfg, token); err != nil {
			return err
		}
	}
	how, notes, err := s.start()
	if err != nil {
		return err
	}
	fmt.Printf("installed: `pumasi up` runs as %s\n  config  %s\n  token   %s\n  log     %s\n", how, s.conf, creds, s.logHow())
	for _, n := range notes {
		fmt.Println("note: " + n)
	}
	fmt.Println("`pumasi service status` shows it, `pumasi status` what it does, `pumasi service remove` stops it.")
	return nil
}
