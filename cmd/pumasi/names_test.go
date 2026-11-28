package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/midagedev/pumasi/internal/watch"
)

func TestShortNamesFitAndStayApart(t *testing.T) {
	names := []string{
		"lint",
		// A matrix: only the first and fourth values differ.
		"deploy (web-app, dev, assets-bucket-0123456789abcdef, web, true) / build-and-deploy",
		"deploy (admin-app, dev, assets-bucket-0123456789abcdef, admin, true) / build-and-deploy",
		"deploy (docs-site, dev, assets-bucket-0123456789abcdef, docs, false) / build-and-deploy",
		// As GitHub cuts a long one: no closing parenthesis.
		"copy-artifact-for-skipped (web-app, dev, assets-bucket-0123456789abcdef-web...",
		"copy-artifact-for-skipped (admin-app, dev, assets-bucket-0123456789abcdef-a...",
		// Alike at both ends, apart only in the middle.
		"login-suite-v2-email / release/2026-10 / self-hosted",
		"login-suite-v2-sso / release/2026-10 / self-hosted",
		"login-suite-v2-split / release/2026-10 / self-hosted",
		// Not ASCII.
		"unit (python — strict, every file, slow mode)",
		"통합 테스트 (전체 모듈, 느린 모드, 야간)",
	}
	for _, width := range []int{16, 22, 30, 60} {
		short := shortNames(names, width)
		seen := map[string]string{}
		for _, n := range names {
			s := short[n]
			if !utf8.ValidString(s) {
				t.Errorf("width %d: %q is not UTF-8", width, s)
			}
			if o, ok := seen[s]; ok {
				t.Errorf("width %d: %q and %q both read %q", width, o, n, s)
			}
			seen[s] = n
			if utf8.RuneCountInString(n) <= width && s != n {
				t.Errorf("width %d: %q fits but was cut to %q", width, n, s)
			}
		}
	}
	short := shortNames(names, 22)
	if s := short["lint"]; s != "lint" {
		t.Errorf("lint: %q", s)
	}
	// The matrix keeps what tells its jobs apart.
	if s := short[names[1]]; !strings.Contains(s, "web-app") || utf8.RuneCountInString(s) > 22 {
		t.Errorf("matrix job: %q", s)
	}
	if s := short[names[4]]; !strings.Contains(s, "web-app") || utf8.RuneCountInString(s) > 22 {
		t.Errorf("cut matrix job: %q", s)
	}
	// Alike at both ends: the middle is kept in view.
	for _, n := range names[6:9] {
		mid := strings.TrimPrefix(strings.Fields(n)[0], "login-suite-v2-")
		if s := short[n]; !strings.Contains(s, mid[:2]) {
			t.Errorf("%q cut to %q loses %q", n, s, mid)
		}
	}
	// At a roomy width, a matrix drops only the values its jobs share.
	if s := shortNames(names, 60)[names[1]]; s != "deploy (web-app, …, web, true) / build-and-deploy" {
		t.Errorf("roomy: %q", s)
	}
}

// The printed table keeps to the width, every row, with long matrix names
// (down to where the other columns leave a name 16 runes: about 95).
func TestTableFitsWidth(t *testing.T) {
	rep := watch.TrendReport{Grain: "day"}
	start := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	add := func(wf, job string) {
		s := watch.Series{Workflow: ".github/workflows/" + wf, Job: job}
		for d := range 15 {
			s.Buckets = append(s.Buckets, watch.Bucket{Start: start.AddDate(0, 0, d), Runs: 3, Succeeded: 3, Sec: watch.Stats{P50: float64(60 + 7*d)}})
		}
		s.All = watch.Bucket{Runs: 1234, Succeeded: 1200, Sec: watch.Stats{P50: 3725, P90: 7322}, MemBytes: watch.Stats{P90: 15 << 30},
			CPUSec: watch.Stats{P50: 9000}, WaitSec: watch.Stats{P90: 4000}}
		rep.Series = append(rep.Series, s)
	}
	add("a-very-long-workflow-file-name-for-deploys.yml", "")
	for _, app := range []string{"web-app", "admin-app", "docs-site", "mobile-backend", "reports"} {
		add("a-very-long-workflow-file-name-for-deploys.yml", "deploy ("+app+", dev, assets-bucket-0123456789abcdef, "+app+", true) / build-and-deploy")
	}
	for _, w := range []int{100, 120} {
		var b strings.Builder
		printTable(&b, rep, w)
		for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > w {
				t.Errorf("width %d: %d runes: %q", w, n, line)
			}
		}
	}
}
