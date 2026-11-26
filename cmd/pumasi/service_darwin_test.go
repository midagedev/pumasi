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

func TestControllerLaunchAgentIsKeptAlways(t *testing.T) {
	text := launchPlist(ctlLabel, []string{"/opt/p&q/pumasi", "up", "-config", "/Users/me/ci <1>/pumasi.yaml"}, true, 5, "Interactive", "/Users/me/ci <1>", "/tmp/up.log", "/usr/bin")
	var v struct{}
	if err := xml.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	for _, want := range []string{"<key>KeepAlive</key><true/>", "<string>/opt/p&amp;q/pumasi</string><string>up</string><string>-config</string><string>/Users/me/ci &lt;1&gt;/pumasi.yaml</string>",
		"<key>WorkingDirectory</key><string>/Users/me/ci &lt;1&gt;</string>", "dev.pumasi.controller"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
}
