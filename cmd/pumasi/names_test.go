package main

import (
	"strings"
	"testing"
	"unicode/utf8"
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
