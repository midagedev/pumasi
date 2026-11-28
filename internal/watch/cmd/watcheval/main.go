// watcheval scores the regression search on made-up histories:
//
//	go run ./internal/watch/cmd/watcheval [-n 100] [-merges 8] [-after 48h] [-slope 0.1] [-why]
package main

import (
	"flag"
	"os"

	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

func main() {
	n := flag.Int("n", 100, "histories per scenario")
	merges := flag.Float64("merges", 8, "merges to main on a weekday")
	days := flag.Int("days", 28, "days of history")
	after := flag.Duration("after", 0, "look only this long after the injected change (0 = to the end)")
	z := flag.Float64("z", 0, "rank test z threshold (0 = default)")
	minRuns := flag.Int("minruns", 0, "runs on each side (0 = default)")
	slope := flag.Float64("slope", 0, "score the growth alarm at this limit instead (0.10 = 10% a week)")
	why := flag.Bool("why", false, "score why-slow on the culprit pull requests instead")
	flag.Parse()
	ec := synth.EvalConfig{Histories: *n, Days: *days, Merges: *merges, After: *after, Options: watch.Options{Z: *z, MinRuns: *minRuns}}
	if *why {
		noise, per := synth.EvalWhy(ec)
		synth.WriteWhyReport(os.Stdout, ec, noise, per)
		return
	}
	if *slope > 0 {
		synth.WriteSlopeReport(os.Stdout, *slope, synth.EvalSlope(ec, *slope))
		return
	}
	noise, per := synth.Eval(ec)
	synth.WriteReport(os.Stdout, ec, noise, per)
}
