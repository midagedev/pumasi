package watch

import (
	"strings"
	"testing"
)

// The percent beside two values is what the two values make as written:
// 86s -> 122s is +42% (not the +41% the unrounded medians give), and
// 8.48G -> 11.53G reads 8.5G -> 11.5G, +35%.
func TestPercentIsWhatTheShownValuesMake(t *testing.T) {
	if got := changeText(Change{Metric: "time", Before: 86.4, After: 121.6, Ratio: 121.6/86.4 - 1}); !strings.HasPrefix(got, "time +42% (1m26s -> 2m02s") {
		t.Errorf("time: %s", got)
	}
	g := float64(1 << 30)
	if got := changeText(Change{Metric: "memory", Before: 8.48 * g, After: 11.53 * g, Ratio: 11.53/8.48 - 1}); !strings.HasPrefix(got, "memory +35% (8.5G -> 11.5G") {
		t.Errorf("memory: %s", got)
	}
	if r := ShownRatio("time", 3725, 3770); r != 0 {
		t.Errorf("past an hour a time reads to the minute: 1h02m both, got %+.3f", r)
	}
}
