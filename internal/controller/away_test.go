package controller

import (
	"context"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
)

// Two hosts are asked to speak when the controller is gone, the others get
// nothing (not even the webhook); when one of the two goes, the other moves
// up and a third takes its place.
func TestTwoHostsAreAskedToSpeakForAnAbsentController(t *testing.T) {
	c, addr, _ := joinController(t)
	c.cfg.Hosts = nil
	c.cfg.Notify.Webhook = "https://hooks.example/x"
	c.cfg.Classes = []config.Class{{Name: "build"}}
	ctx := context.Background()
	stops := map[string]func(){}
	for _, n := range []string{"a1", "b2", "c3"} {
		awayTold.Delete(n)
		stops[n] = joinedHost(t, c, addr, n)
	}
	told := func(n string) api.Away {
		v, _ := awayTold.Load(n)
		a, _ := v.(api.Away)
		return a
	}
	settle := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			c.syncJoined(true)
			c.syncAway(ctx)
			if ok() {
				return
			}
		}
		t.Fatalf("%s: a1 %+v, b2 %+v, c3 %+v", what, told("a1"), told("b2"), told("c3"))
	}
	settle("three hosts", func() bool {
		_, heard := awayTold.Load("c3")
		return told("a1").Rank == 1 && told("b2").Rank == 2 && heard
	})
	if a := told("a1"); a.Webhook != "https://hooks.example/x" || a.AfterSec != 300 || len(a.Classes) == 0 {
		t.Fatalf("the first host was told %+v", a)
	}
	if a := told("c3"); a.Webhook != "" || a.Rank != 0 {
		t.Fatalf("a host that is not to speak was given %+v", a)
	}

	stops["a1"]()
	settle("after a1 went", func() bool { return told("b2").Rank == 1 && told("c3").Rank == 2 })

	// notify.away: 0 takes the webhook back from every host.
	off := time.Duration(0)
	c.cfg.Notify.Away = &off
	settle("away: 0", func() bool { return told("b2").Webhook == "" && told("c3").Webhook == "" })
}
