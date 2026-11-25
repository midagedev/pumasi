package main

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestLaunchAgentIsWellFormed(t *testing.T) {
	text := launchAgent("/Users/me/.local/bin/pumasi", "/Users/me/.cache/pumasi/agent.log", `/opt/a&b/bin:/usr/bin:<x>`)
	d := xml.NewDecoder(strings.NewReader(text))
	var seen []string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%v\n%s", err, text)
		}
		if c, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(c)) != "" {
			seen = append(seen, string(c))
		}
	}
	all := strings.Join(seen, "|")
	for _, want := range []string{"dev.pumasi.agent", "/Users/me/.local/bin/pumasi|agent|-join", "SuccessfulExit", `/opt/a&b/bin:/usr/bin:<x>`} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %s", want, all)
		}
	}
}
