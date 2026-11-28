# Watching CI: trends, regressions, limits, capacity

pumasi already measures every job it runs: its time, CPU and memory, on which host, next to what else. This page covers what it does with those measurements: how long each job takes day by day, the commit where a job got slower (or faster), jobs slowly growing, limits and budgets that open an incident when crossed, answers to "did this pull request make CI slower, and where?" for a person or a coding agent, and when CI's growth will fill the hosts.

Everything here is computed from the ledger on the controller (`pumasi.db`). Nothing is sent anywhere, and nothing is estimated that was not measured.

## What is recorded

For each finished job, besides what pumasi always kept (time, CPU-seconds, peak memory without page cache, host, wait):

- **Event, branch and pull request**, from the workflow ref GitHub sends with the job (`refs/pull/45/merge`, `refs/heads/main`). No API call.
- **Commit and step timings.** One call to GitHub's REST API per finished job (`GET /repos/{repo}/actions/runs/{id}/jobs`), shared by the jobs of a run that finish together, about 20 seconds after the job ends. The call is matched to our runner by name. If GitHub does not answer, the job keeps its measurements and simply has no steps.
- **Contention.** While a job runs, pumasi notes the seconds its host was saturated (CPU at 90% or more of its cores, or CPU or memory pressure) while others took at least a quarter of the cores, or two or more runners shared the host. A job saturated for 20% or more of its time is *contended*: it is left out of baselines and regression search, because its time says more about its neighbours than about its code.

Step timings are kept for 90 days (`watch.keepSteps`), the job's own row as long as the ledger.

## `pumasi trend`

```sh
pumasi trend                       # every job, the last 14 days, and where they changed
pumasi trend --job unit-tests      # jobs whose name matches (a glob or part of it)
pumasi trend --since 6w --week     # a row a week
pumasi trend --days --job build    # every day of the jobs that match
pumasi trend --json                # the same as JSON; GET /api/trend takes the same keys
pumasi trend --demo                # a made-up repository, no controller needed
pumasi trend --min-side 0          # list a job's change at once, not after a day of runs on each side
```

```
CHANGED (newest first)
  pr-checks.yml / build-image: memory +36% (8.5G -> 11.5G), since commit 0c83298 on main, Thu Nov 19 19:00 (pull request #2246 ran at the new level before it); still so
      https://github.com/OWNER/REPO/commit/0c83298…
  pr-checks.yml / typecheck: time -22% (1m23s -> 1m05s), cpu -22% (1m25s -> 1m07s), since commit 6e197ba on main, Tue Nov 17 19:09 (pull request #2104 ran at the new level before it); still so
      step "Install" 25s -> 20s, step "Typecheck" 48s -> 37s; https://github.com/OWNER/REPO/commit/6e197ba…
  pr-checks.yml / unit-tests: time +41% (1m26s -> 2m02s), cpu +42% (2m47s -> 3m58s), since commit 85979f9 on main, Mon Nov 16 19:01 (pull request #2077 ran at the new level before it); still so
      step "Run tests" 47s -> 1m20s; https://github.com/OWNER/REPO/commit/85979f9…
  pr-checks.yml / e2e (chromium): slowly growing, time +24% over 28d along its trend (2m38s -> 3m16s, at least +23%), no single commit

JOB                       RUNS  TIME p50  p90    CHANGE  MEM p90  CPU p50  WAIT p90  TIME p50 BY DAY
pr-checks.yml (workflow)  6250  4m04s     5m34s  +0%     14.4G    46m17s   30s       ▂▂▂▂▂▂▂▂▂▂▂▂▂▂
  build-image             6249  2m10s     2m32s  +3%     11.8G    10m38s   28s       ▂▂▂▂▂▂▂▂▂▂▂▂▂▂
  e2e (chromium)          6249  3m07s     3m58s  +24%*   10.3G    10m04s   28s       ▁▂▂▃▃▄▄▆▆▆▆▇▇█
  typecheck               6250  1m08s     1m33s  -22%*   2.3G     1m09s    28s       ███▇▁▂▂▂▂▂▁▁▁▁
  unit-tests              6249  1m59s     2m26s  +41%*   3.3G     3m41s    28s       ▁▂▃▇▇▇██▇█▇▇▇▇
  …
```

(`pumasi trend --demo`. The demo's six weeks queued on its dashboard's 66 cores, its jobs end as the dashboard runs them (each the middle of the dashboard's time and memory, the weeks before worked back through its changes), and they end at the start of today, so the same day shows the same numbers.) Numbers are of runs that succeeded. A workflow's time is a run's, from its first job's start to its last job's end. CHANGE is what the list above says of the job (its time steps and creep together, marked `*`); for a job with nothing listed, the median of the span's last third of days against its first.

The table is fitted to the terminal, or to 100 columns when the output is not one. A long job name is cut in the middle. A matrix job's name first loses the values that every job of its matrix shares, so `build (linux, dev, bucket-1, x) / deploy` reads `build (linux, …, x) / deploy`, and names that would read the same are cut less until they differ. `--json` keeps the full names.

When hosts changed under the jobs, a section comes first:

```
MACHINES (steps of hosts, not of jobs; newest first)
  server2: 5 jobs 35–60% faster since Thu Nov 19 17:20 — the machine, not the code
      ci.yml / lint -60%, ci.yml / unit-tests -52%, and 3 more
  4 jobs 50–120% slower when they moved from server1/large, mac1/default to server2/medium, Thu Nov 19 13:25 to 14:45 — another kind of machine, not the code
```

A ledger younger than two days says so once, at the top, instead of listing steps: `only 30h of history: a change is listed once the runs on each side of it span 1d, so not before Sat Nov 21 08:00`.

**How a change is found.** For each job and each of time, memory and CPU, on runs that succeeded and were not contended:

1. Values are put on one scale across machines (each machine's median against the job's), as logs. A machine is a host *and* a runner class: a class sets the cores, memory and disk a runner gets, so one host in two classes is two machines.
2. The series is split where its median level moves (binary segmentation on L1 cost), and each split is kept only when the two sides differ by 10% or more and a rank test (Mann–Whitney) gives z ≥ 3.5, with at least 8 runs on each side. A move that goes back within three days is dropped as transient. A level held for less than a day between two steps is not a level: the later step is dropped and the levels are measured again, so a regression that a slow spell hid at first becomes one step from its start.
3. The commit: the best split among the main branch's own runs near it. Main's runs that were not counted there (contended or failed) are named as possible too ("or 85f834f just before it"). Steps of one job within three days of each other, the same way, are named by the clearest of them, and are one line; two steps of the same number become one change from the level before the first to the level after the last ("memory -60% (2.0G -> 0.8G, in 2 steps)").
4. The pull request: one whose runs before the change were all at the new level, at least two of them, the last within two days before it. When more than one fits, none is named.
5. A step that turns out to be part of a slow climb (the drift around it, over a week of runs on each side at least, is half the step or more) is shown as crept ("no single commit"), not pinned on whichever commit happened to cross the line.
6. Like with like: the step is measured again on the runs of the machines the job used both before and after it (a day of runs on each side at least: in a busy job the runs next to a step are an hour or two, and one host's slow hour is not its step). If a third or more of a side's runs were on other machines and the shared ones do not show the step, the job *moved* to another kind of machine, and the step is not the job's.
7. A host's step, not a job's: see below. What is left is listed once the runs on each side of it span a day (`--min-side`).

**Steps of the machines, not the code.** A step is set aside as its machine's, not the job's, when:

- the job moved (6 above);
- the job runs on several hosts, and the step showed on one host's runs (z ≥ 3, twice the 10% minimum or more) while another host's runs of the same days stayed flat (z < 1.5, a quarter of that step or less);
- three or more jobs on one host (or, for jobs spread over hosts, on all of them) stepped the same way at the same moment, and something says it was not one commit. "The same moment" means the spans from each job's last run before to its first run after all overlap, give or take an hour. The evidence against a commit is any of: the jobs are from more than one workflow; their first runs at the new level belong to different runs; or jobs of the same workflow on other hosts ran before and after and did not step.

Such steps are listed once under MACHINES, not as changes of each job, and are not announced as `ci-regression`. They wait for the same day of runs on each side as a job's change (below). A host's step followed by one the other way, on most of the same jobs, is one spell: "slower from … to …, back since".

Measured with `go run ./internal/watch/cmd/watcheval -shift -n 100`. Each history has a host's change in the third week and one job made 20% slower: three days or more away from the host's change ("apart"), or within 12 hours of it ("inside"). "Shared" histories spread every job over both hosts (65/35). "Pinned" histories give each job one host, as runner classes do.

| Host's change | Jobs | False job changes at it, per history (before → after) | +20% found, apart (before → after) | +20% found, inside | Shown as the host's |
|---|---|---|---|---|---|
| a host 1.5× slower (disk filling) | shared | 1.34 → 0.58 | 64% → 64% | 93% → 92% | 47% |
| | pinned | 3.60 → 0.21 | 93% → 92% | 92% → 62% | 93% |
| a host 0.7× (caches cleared) | shared | 5.31 → 0.65 | 66% → 65% | 7% → 2% | 93% |
| | pinned | 3.50 → 0.08 | 95% → 93% | 59% → 46% | 89% |
| a host 2× for 6 hours (a neighbour) | shared | 0.00 → 0.00 | 97% → 95% | 94% → 91% | 0% |
| | pinned | 0.01 → 0.01 | 97% → 94% | 95% → 95% | 0% |
| every host 1.3× (a slow mirror) | shared | 6.80 → 2.11 | 87% → 87% | 98% → 37% | 88% |
| | pinned | 6.77 → 0.57 | 87% → 87% | 97% → 27% | 95% |

A spell of a few hours was already dropped as transient, before and after: each job's search drops a step that comes back within three days, so only a spell longer than that reaches the "slower from … to …, back since" line. The cost is a real regression on the same host within hours of the host's change: it is folded into the host's line. With pinned jobs and a host 1.5× slower, 62% of such regressions were still found instead of 92%. When every host changed, 27–37% were found instead of 97–98%. A regression days away is found as often as before, give or take two histories in a hundred. When every host changes at once and the first runs at the new level are all of one run, it cannot be told from a commit, and it is listed as the jobs' changes. The +20% found less often in the shared histories, before and after, is a host's change of 30–50% on a third of the job's runs, which blurs the search for the regression. The made-up histories give each job one runner class, so machines keyed by class as well as host are not measured here. They were checked on a real ledger: a job that moved to a new class on the same host had shown as −56% by host alone, and with classes it is not a change.

**Too little history.** A job's step is listed only once the runs on each side of it span a day (`--min-side`, default 1d). With less, a few hours of a host's mood (a slow spell, a busy afternoon) cannot be told from the job. A ledger younger than two days lists no job's change and says so once at the top. The day was picked from `watcheval -young -n 100`. Made-up histories with nothing injected were cut at each age, and each row shows the share with a false change listed:

| Each side spans (before / after) | 30h | 2d | 3d | 5d | 7d | 14d | 28d |
|---|---|---|---|---|---|---|---|
| no rule | 4% | 3% | 5% | 13% | 4% | 1% | 0% |
| 12h / 12h | 2% | 2% | 4% | 8% | 2% | 0% | 0% |
| **1d / 1d** | 0% | 2% | 3% | 4% | 2% | 0% | 0% |
| 2d / 2d | 0% | 0% | 0% | 0% | 1% | 0% | 0% |
| 2d / none | 3% | 1% | 3% | 11% | 3% | 1% | 0% |

The price is time: a +20% (+40%) change merged in the third week, listed within

| Each side spans | 1 day | 2 days | 3 days | 7 days |
|---|---|---|---|---|
| no rule | 84% (91%) | 92% (99%) | 91% (99%) | 93% (99%) |
| **1d / 1d** | 27% (24%) | 92% (99%) | 91% (99%) | 93% (99%) |
| 2d / 2d | 2% (0%) | 22% (26%) | 91% (99%) | 93% (99%) |

The criterion was the shortest span that keeps false changes at 5% or less at every age up to a week. That is a day: 12 hours still leaves 8% at five days. Asking only the side before to be long does not help (2d / none: 11% at five days), because the false steps are mostly fresh ones at the young end of the history. Two days would hold every listing back three days. So a change, and its `ci-regression` notice, comes a day after its merge rather than within hours.

**Slowly growing.** A job with no step in its time over the last 28 days (or the span, when longer) whose daily median time grew 12% or more along a Theil–Sen trend, with the trend's 95% interval above zero, is listed as slowly growing. 28 days because less cannot tell a creep from noise. Measured on made-up histories of six weeks (7 merges a weekday): a +25% creep over 150 merges was listed in 39 of 40; with nothing injected, 5 of 700 job-windows (100 histories) were listed falsely, at 12–13%. In a quiet repository (2 merges a weekday) 1 of 100 histories had a false one, and the same creep, spread over 75 days, was caught in 15 of 100.

## `pumasi why-slow`

```sh
pumasi why-slow https://github.com/OWNER/REPO/pull/45     # a pull request (or --pr 45, #45)
pumasi why-slow https://github.com/OWNER/REPO/actions/runs/123   # a run (or its number)
pumasi why-slow 4f2a1b3                                    # a commit
pumasi why-slow --json --pr 45                             # also GET /api/why-slow?pr=45 (run=, commit=)
```

```
pull request #1109, 4 runs (ci.yml, commit 1961da2)
against the last 20 runs of each job on main before it

JOB             RUNS  TIME    USUAL   p10-p90        CHANGE  VERDICT  MEM  CPU
integration     4     11m12s  8m05s   7m10s-8m50s    +38%    slower   +2%  +41% more
e2e (chromium)  4     12m12s  11m36s  10m13s-13m40s  +5%     usual    -1%  +2%
…
integration is slower: "Run integration" 5m49s -> 8m59s (+3m09s)

main changed right after #1109 was merged, and its runs were already at the new level:
  ci.yml / integration: time +34% (8m01s -> 10m45s), cpu +36% (18m29s -> 25m11s), since commit b94ae72 on main, …
      step "Run integration" 5m30s -> 7m58s; https://github.com/OWNER/REPO/commit/b94ae72…
```

Each job of the target is set against its last 20 runs on the main branch before it that succeeded and were not contended, on the scale of the host the target ran on (a slower host is not a slower job). The target's contended runs are left out when it has others.

| Verdict | Means |
|---|---|
| `slower` | it ran twice or more, the median of its runs is 10% or more over main's median, and even its fastest run is two robust standard deviations over main's usual |
| `maybe slower` | the median is 10% or more over and z ≥ 2, with every run above main's p84; or it ran once and is three deviations over. One run is never more than maybe: a cold cache or a slow mirror makes a few percent of single runs far slower |
| `usual` | neither |
| `uneven` | (the table only; `usual` in JSON) neither, but the median is 10% or more off: a run of it was within main's usual spread, or too few ran to tell |
| `unknown` | main has fewer than 5 usable runs of the job before it |

The same words go the other way (`faster`), and for memory (`bigger`/`smaller`) and CPU (`more`/`less`).

- **slow host.** When the other jobs that ran on the target's host around the same time were 10% or more over their own usual (three or more of them), the target's time is divided by that, and the row says `slow host x1.4`.
- **main changed at this target.** For a commit (or a run of one on main): main's changes that begin at that commit. For a pull request: main's changes that began right after it was merged, when the pull request's own runs were already at the new level. When other pull requests fit just as well, it says "one of 3 pull requests that fit: #…". This is the strongest evidence why-slow gives, since it rests on main's runs after the merge, not only on the pull request's few runs.
- **main changed during the baseline.** When main itself changed in a job between the start of the baseline and the target, only main's runs since that change are the job's usual, and the change is listed after.

### How well it answers (measured)

On made-up histories (`go run ./internal/watch/cmd/watcheval -why -n 100`): 28 days of a repository with seven CI jobs on two hosts of different speed, a realistic noise model (job spread, busy hours, cold-cache outliers, contended runs mostly but not always flagged, hours when one host is slow for everyone without saying so, natural growth of the suite), and one change injected with the pull request that carried it. The culprit pull request is asked about three days after it was merged.

| Change in the culprit's job | Runs of it | `slower` | `maybe slower` | main's change shown as its own (only one that fits) | (one of several) | Answered | Neighbours wrongly shown as the cause |
|---|---|---|---|---|---|---|---|
| time +20% | 2.3 | 8% | 28% | 58% | 12% | **74%** | 0% (1% as one of several) |
| time +40% | 2.5 | 48% | 34% | 88% | 4% | **97%** | 0% |
| one step +40% | 2.4 | 24% | 31% | 79% | 8% | **88%** | 0% (1%) |
| memory +30% | 2.4 | 72% | 28% | 93% | 2% | **100%** | 0% |

With nothing injected, of 2000 pull requests: 2% had a job called `slower`, 13% had one called `maybe slower` and none slower (2% of all jobs asked), and none was shown as the cause of a change on main.

In a quieter repository (2 merges a weekday instead of 8) main has fewer runs to show the change by: answered 60% (time +20%), 89% (+40%), 70% (one step), 100% (memory); 2% of pull requests with nothing injected got a `slower`, 15% a `maybe slower`.

Read it this way: `slower` is rarely wrong but needs a large change or several runs; a pull request with one or two runs gets `maybe` at best, and the line under it ("main changed right after #N was merged") is what settles it once main has run a few times since.

"Answered" means the affected job got `slower` or `maybe slower`, or main's change was shown as the pull request's own. "Neighbours" are the other pull requests whose last run came in the two days before the change, asked the same: how often why-slow wrongly showed main's change as theirs.

## `pumasi forecast`

```sh
pumasi forecast                 # the running up's hosts (each its budget, else its cores) and the ledger
pumasi forecast --cores 48      # the ledger, against 48 cores
pumasi forecast --weeks 12      # read 12 weeks (default 8)
pumasi forecast --json          # also GET /api/forecast (weeks=, cores=)
pumasi forecast --demo          # the demo's history, which queued on the demo dashboard's 66 cores
```

```
CI used 2482 cpu-hours in the last 7 days (2368 the week before); over the last 5 weeks it grew +7% a week (+5% to +11%, 80% interval). At the busiest hours it keeps 29 of the 66 cores the hosts lend busy (45%); at this rate they need every core in ~12 weeks (8 to 18 weeks); one job in ten waits a minute or more for room in ~8 weeks (now under 1%, 0s on average).
```

- **Growth** is the Theil–Sen slope of the weekly cpu-hours (on logs), with its 80% interval. A week the ledger does not reach the start of is shown but not fitted; a ledger whose last job is more than two days old ends the weeks there.
- **The busiest hours** are the 95th percentile of the hours' cores in use (each job's cpu-seconds spread over its time).
- **Cores the hosts lend**: what each host lends runners, its budget or else all its cores, the way `pumasi forecast` and the CI panel count them when they come from the hosts. The map counts a shared server's cores again for placement ("16 cores count as 24"); the forecast does not, as a job that shares a core only takes longer. `--cores N` (or `cores=`) is taken as given and says only "cores".
- **The waits** come from replaying the last week's jobs on the cores, first come first served, each taking the cores it used: a run brings all its jobs at once, which is most of why CI waits. CI growing is the same week with more runs, each a copy of a run of that week at the time another run came (give or take an hour). "Need every core" is when the replay's busiest hours want all the cores; it comes later than the cpu-hours alone say, since more runs fill the quiet hours too. The replay goes up to eight times the runs; past that the hours are smooth and their busiest grow with the runs, so it scales the busiest hours at eight times up to the cores (on a made-up week at 11% of its cores, within 3% of replaying on to the cores).
- Every date is "at this rate": the growth, carried forward. The ranges are the same dates at the ends of the growth's interval.

What the replay leaves out, so real waits come **sooner** than it says: hosts are not one pool (a job needs room on one host), memory is not counted, and pumasi reserves a job's largest recent size plus 20%, more than the cores it used on average.

### How well it forecasts (measured)

`go run ./internal/watch/cmd/watcheval -forecast 0.08 -n 100`: a team whose merges grow 8% a week, 16 weeks; the cores set so the first week's busiest hours are at 35%. The jobs wait in a queue simulation (first come first served on one pool of cores). The forecast reads weeks 1–8; what came is read from weeks 9–16, each week smoothed with its neighbours.

| | Growth 8%, 8 merges a weekday | Growth 5% (first week at 45%) | Growth 8%, 2 merges a weekday |
|---|---|---|---|
| Growth read from 8 weeks (p10 – p90) | +5.4% – +11.8% | +1.8% – +8.7% | +1.9% – +12.7% |
| Share of jobs waiting a minute or more: the replay against the queue | 21% / 26% | 38% / 46% | 67% / 73% |
| Busiest hours need every core: off by (median) | 1.9 weeks (35% of the time to it), 23 of 100 came within the history | 1.9 weeks (48%), 15 came | 2.9 weeks (39%), 5 came |
| The mean wait for room doubles: off by (median) | 1.9 weeks (42%), 95 came | 2.3 weeks (52%), 63 came | 2.7 weeks (38%), 29 came |
| Actual date inside the 80% range | 57% (full), 50% (double) | 66%, 54% | 60%, 63% |

The replay gets today's waits about right (5 to 8 points under the queue). The dates are off by about two weeks, and most of that is the growth: eight weeks of a team whose merges come at random tell the growth only to within a few percent a week, and a date three or four weeks out moves by weeks with it. The 80% ranges hold the actual date only 50–66% of the time, because the actual weeks have their own luck as well. Read a forecast as "a month or two", not as a date. A fleet far from full is a longer reach: with the first week's busiest hours at 4% of the cores and merges growing 12% a week (`-forecast 0.12 -load 0.04 -weeks 42 -n 12`), the cores filled within the history in all 12, a median 20.4 weeks after the forecast; the forecast was off by 2.7 weeks (median, 13% of the time to it), late by 1.8 on average, and the 80% range held 75%. Before the replay scaled past eight times the runs, all 12 said "not within a year". A first try with Erlang C (jobs coming one at a time at random) said 11% of the busiest hours' jobs waited where the queue had 53%.

## Limits, budgets and alarms (`watch:`)

```yaml
watch:
  limits:
    - match: {workflow: ci.yml, job: "e2e*"}
      maxJobTime: 20m          # the median of its last 5 runs that succeeded
      maxMemory: 12G
    - match: {workflow: ci.yml}
      maxRunTime: 25m          # a run, first job's start to last job's end
    - match: {workflow: "deploy*.yml"}
      maxWait: 5m              # a job waiting longer than this for a runner, now
    - match: {workflow: nightly.yml, event: schedule}
      finishBy: "07:00"        # a run started in the day before, finished by then
  cpuHoursPerDay: 300          # all jobs together, from midnight
  slope: 10%                   # a job's median time grew more than this in a week
  regressions: 20%             # announce a change this big once (default; "off" = never)
  keepSteps: 90d
```

`match` takes the keys priority rules take (`workflow`, `job`, `repo`, `branch`, `event`, `class`, `ref`), and every key set must match.

Each crossed limit is an incident: it shows in `pumasi status` and the dashboard's problems, and goes to `notify` when it opens and when it resolves, like every other incident. Time and memory are the median of the last five runs that succeeded, so one slow run is not a problem and a job that is now slow is. Kinds, for webhook filters:

| kind | when |
|---|---|
| `ci-too-slow` | `maxJobTime` or `maxRunTime` crossed |
| `ci-too-big` | `maxMemory` crossed |
| `ci-wait` | a job waited longer than `maxWait` |
| `ci-late` | a `finishBy` run had not finished, or had not run, by then |
| `ci-over-budget` | `cpuHoursPerDay` used up today |
| `ci-growing` | `slope` crossed (it stays open until growth falls under half the slope) |
| `ci-regression` | a change found by trend, of at least `regressions`, that began in the last 7 days and still holds; sent once, not kept open |

### How well regressions are found (measured)

`go run ./internal/watch/cmd/watcheval -n 100`: 100 histories per scenario, one change injected at a random merge of the third week on a random job, searched over the whole history.

| Scenario | Found | Commit exact | Commit among those named | Right step first | Pull request named (right) | False alarms a week |
|---|---|---|---|---|---|---|
| time +20% | 93% | 82% | 87% | - | 61% (98%) | 0.01 |
| time +40% | 99% | 95% | 99% | - | 73% (100%) | 0.02 |
| memory +30% | 100% | 96% | 97% | - | 75% (100%) | 0.00 |
| cpu +25% | 97% | 90% | 92% | - | 65% (100%) | 0.01 |
| one step +40% | 99% | 90% | 91% | 100% | 57% (93%) | 0.01 |
| creep +25% over 30 merges | 66% | (no single commit) | | | | 0.09 |
| nothing injected | | | | | | 0.00 |

"Commit among those named" counts the right commit given as the alternative ("or 85f834f just before it"), when the merge commit's own run was contended or failed. Of the creeps found, 62% were shown as crept; the rest were pinned on a commit, which is the error to expect from a slow climb. The `slope` alarm is the tool for creep: at 10% a week, checked daily, it caught 88% of these creeps within 7 days (5.4 days on average) and the steps in 3–4 days, with no false alarm in 100 histories with nothing injected.

How soon: a +20% change was listed in 27% of histories within 24 hours of its merge (84% without the one-day rule above) and 92% within 48. In a quiet repository (2 merges a weekday) recall for +20% is 82% and the commit exact 71%; the slope alarm caught steps 96–100% of the time but creep only 5%, with about 0.08 false alarms a week.
