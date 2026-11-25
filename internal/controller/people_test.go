package controller

import (
	"testing"

	"github.com/midagedev/pumasi/internal/api"
)

func TestPersonAt(t *testing.T) {
	sec := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		kind string
		idle *int64
		want bool
	}{
		{"desktop", sec(3), true},
		{"desktop", sec(14*60 + 59), true},
		{"desktop", sec(15 * 60), false},
		{"desktop", nil, false}, // cannot tell: free
		{"server", sec(3), false},
	} {
		if got := personAt(c.kind, &api.Snapshot{InputIdleSec: c.idle}); got != c.want {
			t.Errorf("%s idle %v: %v, want %v", c.kind, c.idle, got, c.want)
		}
	}
}
