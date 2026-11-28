// watcheval scores the regression search on made-up histories:
//
//	go run ./internal/watch/cmd/watcheval [-n 100] [-merges 8] [-after 48h] [-slope 0.1] [-why] [-shift] [-young] [-forecast 0.08]
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
	shift := flag.Bool("shift", false, "score telling a host's change from the jobs' instead")
	young := flag.Bool("young", false, "score the history rule (how long each side of a change must span) instead")
	forecast := flag.Float64("forecast", 0, "score the capacity forecast on teams growing this much a week instead (0.08)")
	load := flag.Float64("load", 0.35, "forecast: the first week's busiest hours against the cores")
	weeks := flag.Int("weeks", 16, "forecast: weeks of history")
	base := flag.Int("base", 8, "forecast: weeks the forecast reads")
	flag.Parse()
	ec := synth.EvalConfig{Histories: *n, Days: *days, Merges: *merges, After: *after, Options: watch.Options{Z: *z, MinRuns: *minRuns}}
	if *forecast > 0 {
		fe := synth.ForecastEval{Histories: *n, Growth: *forecast, Load: *load, Weeks: *weeks, Base: *base, Merges: *merges}
		synth.WriteForecastReport(os.Stdout, fe, synth.EvalForecast(fe))
		return
	}
	if *young {
		synth.WriteYoungReport(os.Stdout, ec, synth.EvalYoung(ec))
		return
	}
	if *shift {
		synth.WriteShiftReport(os.Stdout, ec, synth.EvalShift(ec))
		return
	}
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
