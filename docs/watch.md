# Watching CI: trends, regressions, limits

pumasi already measures every job it runs: its time, CPU and memory, on which host, next to what else. This page covers what it does with those measurements: how long each job takes day by day, the commit where a job got slower, limits and budgets that open an incident when crossed, and answers to "did this pull request make CI slower, and where?" for a person or a coding agent.

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
```

```
CHANGED (newest first)
  ci.yml / integration: time +34% (8m01s -> 10m45s), cpu +36% (18m29s -> 25m11s), since commit b94ae72 on main, Thu Nov 12 18:33 (pull request #1109 ran at the new level before it); still so
      step "Run integration" 5m30s -> 7m58s; https://github.com/OWNER/REPO/commit/b94ae72…

JOB                     RUNS  TIME p50  p90     CHANGE  MEM p90  CPU p50  WAIT p90  TIME p50 BY DAY
ci.yml (workflow)       434   12m54s    17m29s  -0%     11.3G    2h04m    30s       ▄█▅▅▆▅▆█▅▅▅▅▆▄█▅▆▆▇█▆▃▃▁
  integration           434   8m51s     12m44s  +35%    6.3G     19m42s   27s       ▂▃▁▁▁▁▂▁▁▂▁▂▆▆▅▅▆▅▆▇▇█▅▅
```

Numbers are of runs that succeeded. A workflow's time is a run's, from its first job's start to its last job's end.

**How a change is found.** For each job and each of time, memory and CPU, on runs that succeeded and were not contended:

1. Values are put on one scale across hosts (each host's median against the job's), as logs.
2. The series is split where its median level moves (binary segmentation on L1 cost), and each split is kept only when the two sides differ by 10% or more and a rank test (Mann–Whitney) gives z ≥ 3.5, with at least 8 runs on each side. A move that goes back within three days is dropped as transient.
3. The commit: the best split among the main branch's own runs near it. Main's runs that were not counted there (contended or failed) are named as possible too ("or 85f834f just before it").
4. The pull request: one whose runs before the change were all at the new level, at least two of them, the last within two days before it. When more than one fits, none is named.
5. A job that crept up over many merges, rather than at one commit, is shown as crept ("no single commit"), not pinned on whichever commit happened to cross the line.

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
integration is slower: "Run integration" 5m23s -> 9m14s (+3m50s)

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
| `unknown` | main has fewer than 5 usable runs of the job before it |

The same words go the other way (`faster`), and for memory (`bigger`/`smaller`) and CPU (`more`/`less`).

- **slow host.** When the other jobs that ran on the target's host around the same time were 10% or more over their own usual (three or more of them), the target's time is divided by that, and the row says `slow host x1.4`.
- **main changed at this target.** For a commit (or a run of one on main): main's changes that begin at that commit. For a pull request: main's changes that began right after it was merged, when the pull request's own runs were already at the new level. When other pull requests fit just as well, it says "one of 3 pull requests that fit: #…". This is the strongest evidence why-slow gives, since it rests on main's runs after the merge, not only on the pull request's few runs.
- **main changed during the baseline.** When main itself changed in a job between the start of the baseline and the target, only main's runs since that change are the job's usual, and the change is listed after.

### How well it answers (measured)

On made-up histories (`go run ./internal/watch/cmd/watcheval -why -n 100`): 28 days of a repository with seven CI jobs on two hosts of different speed, a realistic noise model (job spread, busy hours, cold-cache outliers, contended runs mostly but not always flagged, hours when one host is slow for everyone without saying so, natural growth of the suite), and one change injected with the pull request that carried it. The culprit pull request is asked about three days after it was merged.

| Change in the culprit's job | Runs of it | `slower` | `maybe slower` | main's change shown as its own (only one that fits) | (one of several) | Answered | Neighbours wrongly shown as the cause |
|---|---|---|---|---|---|---|---|
| time +20% | 2.3 | 8% | 28% | 58% | 13% | **74%** | 0% (1% as one of several) |
| time +40% | 2.5 | 48% | 34% | 88% | 4% | **97%** | 0% |
| one step +40% | 2.4 | 24% | 31% | 79% | 8% | **88%** | 0% (1%) |
| memory +30% | 2.4 | 72% | 28% | 93% | 2% | **100%** | 0% |

With nothing injected, of 2000 pull requests: 2% had a job called `slower`, 13% had one called `maybe slower` and none slower (2% of all jobs asked), and none was shown as the cause of a change on main.

In a quieter repository (2 merges a weekday instead of 8) main has fewer runs to show the change by: answered 60% (time +20%), 89% (+40%), 70% (one step), 100% (memory); 2% of pull requests with nothing injected got a `slower`, 15% a `maybe slower`.

Read it this way: `slower` is rarely wrong but needs a large change or several runs; a pull request with one or two runs gets `maybe` at best, and the line under it ("main changed right after #N was merged") is what settles it once main has run a few times since.

"Answered" means the affected job got `slower` or `maybe slower`, or main's change was shown as the pull request's own. "Neighbours" are the other pull requests whose last run came in the two days before the change, asked the same: how often why-slow wrongly showed main's change as theirs.

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
| time +20% | 94% | 81% | 86% | - | 61% (98%) | 0.02 |
| time +40% | 99% | 95% | 99% | - | 73% (100%) | 0.03 |
| memory +30% | 100% | 96% | 97% | - | 75% (100%) | 0.00 |
| cpu +25% | 97% | 90% | 92% | - | 65% (100%) | 0.01 |
| one step +40% | 99% | 91% | 92% | 100% | 57% (93%) | 0.01 |
| creep +25% over 30 merges | 66% | (no single commit) | | | | 0.09 |
| nothing injected | | | | | | 0.01 |

"Commit among those named" counts the right commit given as the alternative ("or 85f834f just before it"), when the merge commit's own run was contended or failed. Of the creeps found, 56% were shown as crept; the rest were pinned on a commit, which is the error to expect from a slow climb. The `slope` alarm is the tool for creep: at 10% a week, checked daily, it caught 88% of these creeps within 7 days (5.4 days on average) and the steps in 3–4 days, with no false alarm in 100 histories with nothing injected.

How soon: a +20% change was found in 84% of histories within 24 hours of its merge and 93% within 48. In a quiet repository (2 merges a weekday) recall for +20% is 82% and the commit exact 71%; the slope alarm caught steps 96–100% of the time but creep only 5%, with about 0.08 false alarms a week.
