package main

import (
	"strings"
	"testing"
)

func TestControllerUnit(t *testing.T) {
	text := ctlUnitText("/home/me/pumasi/dist/pumasi", "/home/me/my ci/100%/pumasi.yaml")
	for _, want := range []string{
		`ExecStart=/home/me/pumasi/dist/pumasi up -config "/home/me/my ci/100%%/pumasi.yaml"`,
		"WorkingDirectory=/home/me/my ci/100%%\n",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "TOKEN") || strings.Contains(text, "Environment") {
		t.Fatalf("the unit carries no credentials:\n%s", text)
	}
}
