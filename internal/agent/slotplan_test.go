package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/midagedev/pumasi/internal/api"
)

const gb = 1 << 30

func slot(class string, n int, state string) slotIn {
	return slotIn{Key: SlotKey(class, n), Class: class, N: n, State: state, Ready: state == "off", Stoppable: state == "listening" || state == "starting"}
}

var (
	big   = api.SlotClass{Name: "big", CPUs: 4, MemBytes: 8 * gb, Slots: 2}
	small = api.SlotClass{Name: "small", CPUs: 2, MemBytes: 2 * gb, Slots: 3}
)

func TestPlanSlots(t *testing.T) {
	tests := []struct {
		name        string
		in          planIn
		start, stop []string
		why         map[string]string // substrings
		over        bool
	}{
		{
			name: "classes that share room start in turn: the first now, the other what still fits beside it",
			in: planIn{Classes: []api.SlotClass{big, small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "off"), slot("big", 2, "off"), slot("small", 1, "off"), slot("small", 2, "off"), slot("small", 3, "off")}},
			start: []string{"big/1", "small/1"},
			why:   map[string]string{"big/2": "no room for one more", "small/2": "a moment: big listens first"},
		},
		{
			name: "the class whose last job is longest ago goes first",
			in: planIn{Classes: []api.SlotClass{big, small}, Order: []string{"small", "big"}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "off"), slot("small", 1, "off"), slot("small", 2, "off"), slot("small", 3, "off")}},
			start: []string{"small/1", "small/2", "small/3"},
			why:   map[string]string{"big/1": "a moment: small listens first"},
		},
		{
			name: "a listener that has only just come up still holds the other class back",
			in: planIn{Classes: []api.SlotClass{big, small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{{Key: "big/1", Class: "big", N: 1, State: "listening", Stoppable: true, Fresh: true}, slot("small", 1, "listening"), slot("small", 2, "off"), slot("small", 3, "off")}},
			why: map[string]string{"small/2": "a moment: big listens first"},
		},
		{
			name: "once the first has listened a moment without a job, every class listens with what fits, each counted by itself",
			in: planIn{Classes: []api.SlotClass{big, small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "listening"), slot("big", 2, "off"), slot("small", 1, "listening"), slot("small", 2, "off"), slot("small", 3, "off")}},
			start: []string{"small/2", "small/3"},
			why:   map[string]string{"big/2": "no room for one more"},
		},
		{
			name: "one class took a job: the other's idle listeners that no longer fit are stopped",
			in: planIn{Classes: []api.SlotClass{big, small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "busy"), slot("small", 1, "listening"), slot("small", 2, "listening"), slot("small", 3, "listening")}},
			stop: []string{"small/2", "small/3"},
		},
		{
			name: "no budget: every slot listens",
			in: planIn{Classes: []api.SlotClass{big}, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "off"), slot("big", 2, "off")}},
			start: []string{"big/1", "big/2"},
		},
		{
			name: "scale set runners on the host hold room too",
			in: planIn{Classes: []api.SlotClass{small}, BudgetCPUs: 6, BudgetMem: 12 * gb, HeldCPUs: 3, HeldMem: 3 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("small", 1, "off"), slot("small", 2, "off")}},
			start: []string{"small/1"},
			why:   map[string]string{"small/2": "no room for one more"},
		},
		{
			name: "a busy machine starts nothing and stops nothing",
			in: planIn{Classes: []api.SlotClass{small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: 1, MachineMem: 8 * gb,
				Slots: []slotIn{slot("small", 1, "listening"), slot("small", 2, "off")}},
			why: map[string]string{"small/2": "the machine is busy"},
		},
		{
			name: "a closed host starts nothing and stops its idle listeners",
			in: planIn{Classes: []api.SlotClass{small}, Closed: "in use", BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("small", 1, "listening"), slot("small", 2, "off"), slot("small", 3, "busy")}},
			stop: []string{"small/1"},
			why:  map[string]string{"small/2": "in use"},
		},
		{
			name: "reserve keeps room from the other class",
			in: planIn{Classes: []api.SlotClass{{Name: "big", CPUs: 4, MemBytes: 8 * gb, Slots: 1, Reserve: 1}, small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "off"), slot("small", 1, "off"), slot("small", 2, "off")}},
			start: []string{"big/1", "small/1"},
			why:   map[string]string{"small/2": "reserved for big"},
		},
		{
			name: "more slots on disk than slots: says",
			in: planIn{Classes: []api.SlotClass{{Name: "small", CPUs: 2, MemBytes: 2 * gb, Slots: 1}}, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("small", 1, "listening"), slot("small", 2, "listening"), slot("small", 3, "off")}},
			stop: []string{"small/2"},
			why:  map[string]string{"small/3": "slots: 1"},
		},
		{
			name: "a slot being released is stopped and not started again",
			in: planIn{Classes: []api.SlotClass{small}, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{{Key: "small/1", Class: "small", N: 1, State: "listening", Stoppable: true, Release: true}, {Key: "small/2", Class: "small", N: 2, State: "off", Ready: true, Release: true}}},
			stop: []string{"small/1"},
			why:  map[string]string{"small/2": "being released"},
		},
		{
			name: "a slot waiting to retry is not started",
			in: planIn{Classes: []api.SlotClass{small}, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{{Key: "small/1", Class: "small", N: 1, State: "off"}, slot("small", 2, "off")}},
			start: []string{"small/2"},
		},
		{
			name: "over the budget: nothing is started, it is said",
			in: planIn{Classes: []api.SlotClass{big, small}, BudgetCPUs: 6, BudgetMem: 12 * gb, MachineCPUs: -1, MachineMem: -1,
				Slots: []slotIn{slot("big", 1, "busy"), slot("small", 1, "busy"), slot("small", 2, "busy"), slot("small", 3, "off")}},
			why:  map[string]string{"small/3": "no room"},
			over: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := planSlots(tt.in)
			if !reflect.DeepEqual(out.Start, tt.start) {
				t.Errorf("start %v, want %v", out.Start, tt.start)
			}
			if !reflect.DeepEqual(out.Stop, tt.stop) {
				t.Errorf("stop %v, want %v", out.Stop, tt.stop)
			}
			for k, want := range tt.why {
				if !strings.Contains(out.Why[k], want) {
					t.Errorf("why[%s] = %q, want it to say %q", k, out.Why[k], want)
				}
			}
			if (out.Over != "") != tt.over {
				t.Errorf("over = %q, want over=%v", out.Over, tt.over)
			}
		})
	}
}

// The absolute rule: whatever the budget, the plan, the host's state or the
// operator's wish, no path stops a slot that holds a job or is taking one.
func TestNoPathStopsASlotThatHoldsAJob(t *testing.T) {
	states := []string{"off", "starting", "listening", "busy"}
	classSets := [][]api.SlotClass{
		{big, small},
		{small}, // big is not in the plan any more
		{{Name: "big", CPUs: 4, MemBytes: 8 * gb, Slots: 0}, small}, // slots lowered to nothing
		{{Name: "big", CPUs: 4, MemBytes: 8 * gb, Slots: 2, Closed: "servers first"}, {Name: "small", CPUs: 2, MemBytes: 2 * gb, Slots: 3, Reserve: 3}},
	}
	n := 0
	for _, classes := range classSets {
		for _, closed := range []string{"", "quarantined"} {
			for _, budget := range []float64{0, 1, 6, 100} {
				for _, held := range []float64{0, 50} {
					for _, release := range []bool{false, true} {
						for _, a := range states {
							for _, b := range states {
								for _, c := range states {
									in := planIn{Classes: classes, Closed: closed, BudgetCPUs: budget, BudgetMem: budget * gb, HeldCPUs: held, HeldMem: held * gb, MachineCPUs: 0, MachineMem: 0}
									for i, st := range []string{a, b, c} {
										class := []string{"big", "big", "small"}[i]
										num := []int{1, 2, 1}[i]
										s := slot(class, num, st)
										s.Release = release
										// A listener that is taking a job reports busy and is not stoppable;
										// one whose state cannot be read is idle but not stoppable.
										in.Slots = append(in.Slots, s)
										if st == "listening" {
											u := slot(class, num+10, st)
											u.Stoppable = false
											in.Slots = append(in.Slots, u)
										}
									}
									out := planSlots(in)
									for _, key := range out.Stop {
										for _, s := range in.Slots {
											if s.Key == key && (s.State == "busy" || !s.Stoppable) {
												t.Fatalf("stops %s (%s, stoppable=%v) with %+v", key, s.State, s.Stoppable, in)
											}
										}
									}
									for _, key := range out.Start {
										for _, s := range in.Slots {
											if s.Key == key && s.State != "off" {
												t.Fatalf("starts %s which is %s", key, s.State)
											}
										}
									}
									n++
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d cases", n)
	}
}
