# What has been verified

How far each behavior in [design.md](design.md) has been checked. There are three levels.

- **Real**: seen on real hosts with real GitHub jobs.
- **Unit**: seen only in tests (a fake agent and fake GitHub, or at the driver level).
- **Not run**: the code exists but has not been run.

Real hosts: a Linux server (Docker), a WSL2 Ubuntu desktop (systemd 255), an Apple silicon laptop, a Windows 11 x64 desktop. 2026-11-24 to 25.
The real checks in the "Size" section were made on 2026-11-26, on the laptop and a Linux VM on it (aarch64, 8cpu/32G, Docker, cgroup v2).
The real checks of expedite, mode and runtime rules in the "Line" section were made on 2026-11-27, with one laptop as the host (native, `--cpus 3 --mem 4G`, jobs that hold 2500MiB, so one at a time), in a personal repo.

## Not run, at a glance

The "Not run" rows scattered through the tables below, gathered in one place. The details are in that row of each section.

| Group | Not run |
|---|---|
| Windows | estimates and measurement (only build and vet) · enforcing the CPU limit · a session logged on at the desk (input detection, starting at logon, windows) · the `died` verdict after fast startup · updating itself on meeting a controller of another version |
| Reboots | a real host reboot (only simulated by changing the boot id) · a controller run as a service, after a reboot · Linux with linger off, a Mac with no one logged in on its screen |
| Limits and pressure | actually hitting the limit that protects the host (80% of the machine) · actually closing a host on Linux memory pressure (PSI 10%) · a controller on a loaded Mac for hours |
| Auth and permissions | creating a scale set and running jobs with a real GitHub App · looking up and rerunning runs with only the fine-grained permissions (Actions: Read / Read and write) |
| Dashboard | the Job sizes table and the runners' now/est columns · the credential exposure warning — the screen was not opened for either · writing mode and rules from the priority view (only in the demo; expedite was real) · writing with a token to a dashboard open to the network, the token field that opens after a 401 |
| Expedite | keeping an expedited job's host from smaller, lower jobs · the order when several are expedited · release on cancel or run end · carrying on after GitHub reassigns every 5 minutes — all unit only |
| Notifications | the absence notice: when only the host's network is cut, a laptop host waking from sleep, with the default 5 minutes as is |
| People first | Linux screen session, Windows input time |
| Config combinations | native runners when `image` is `:latest` · joining hosts for you with `hosts: - ssh:` from the service |
| Measurement (11-27) | capping runner memory at the machine's size, on a real Mac · leaving large samples in the ledger out and placing a job that fits no host alone, on a real host · measuring disk by the fullest filesystem runners live on, on a real Linux host with two disks · telling Windows volumes apart — all unit only |
| Causes not pinned down | files coming back after `down` on the WSL host (they have not come back since it was blocked, but it was not reproduced with the version before the fix) · the 31 seconds when a Mac was the class `after` another (whether pressure caused it) · keys merging when different workflows call the same reusable file with the same job name (it only follows from the naming rule) |

## Placement and the ledger

| Behavior | Level | What was seen |
|---|---|---|
| Packing jobs in never takes the sum of runner sizes over the host's budget | Real | 2 hosts, 39 jobs: in 622 snapshots the largest sum was equal to or under the budget |
| A deferred job starts when room frees up, without a new message | Real | Deferred by lowering the budget and restoring it; placed at the next evaluation (17 minutes after it was first seen) |
| Each job's cpu-seconds, peak memory, boot time and result go into the ledger | Real | Checked against `/metrics`: peak matches, cpu within 0.6 seconds |
| Runner boot (placed → Listening) | Real | Container 4.3–11 seconds (a busy server varies 2–5x), native Linux about 3 seconds, Mac 3.4–5.4 seconds, Windows about 3 seconds, `docker: inside` 5.0–5.6 seconds |
| Knows a runner is busy before GitHub's message | Real | All 10 jobs, about 30 seconds earlier |
| `hosts`: only on the bound hosts, waits when they are full | Real (one host) / Unit | "Does not go to another, empty host" with two hosts is unit only |
| `reserve` | Real | Budget 6cpu: of 7 jobs of other classes only 4 started and 3 waited with `reserved for …`; the reserved class's job started at once |
| `mounts`: only on hosts with the path, read and write at the same path, a wait reason when it is missing | Real | With the work directory inside the mount, hard links work (link count 2) |
| Rejecting unknown config keys and classes with the same name | Unit | |
| Hosts that joined on their own are included in the dashboard's 24-hour history | Unit | |

## Size (placement without numbers to edit)

The config had no numbers (`classes: [{name: grow}, …]`, hosts joined with no cap).

| Behavior | Level | What was seen |
|---|---|---|
| A job never seen before is placed at the class's first estimate | Real | `takes 2.0cpu/4.0G of the free 6.4cpu/22.7G for hold (1) (the class's first estimate, …)` |
| Several jobs from a workflow never seen before, arriving together, are not placed side by side | Real + Unit | A VM (memory it can lend 22.9G), an empty ledger, 3 jobs holding 5000MiB at the same time. Only one was placed: `takes 0.5cpu/4.0G … (the class's first estimate; nothing is measured of this job or its workflow yet); holds 12.0G while it is watched`; the other two waited with `vm1 6.0cpu/10.9G: it watches 1 unmeasured job there and takes the next with 12.0G free or when that one has shown its size`. After the first job had run 3 minutes (start 12:09 → 12:12:30), the two were placed with `takes 0.5cpu/6.0G … (no measured run of this job yet; sized like the largest known of its workflow's other jobs, 0.0cpu/5.0G)`. Whether 3x and 3 minutes fit real JVM builds was not checked (only that the rule works) |
| Borrowing from finished samples of the same workflow, a small host that can't fit 3x still takes one, a measured job is placed as usual next to a watched job | Unit | `internal/decide/blind_test.go`, `internal/controller/size_test.go`, `internal/size` |
| Keys for matrix and reusable-workflow jobs | Real | One workflow calls a reusable file twice (200MiB, 1200MiB), with a matrix job and a plain job inside: the names in the messages are `small / inner (a)` · `big / inner (a)` · `big / plain` …, and the workflow is the called file (`reuse.yml`). Seven jobs learned separately under seven keys (`big / inner (a)` 1.6G, `small / inner (a)` 0.4G). Merging when different workflows call the same file with the same job name follows from the naming rule and was not run |
| A real job on a native Linux runner | Real | WSL2 host (has libicu), 2 jobs holding 800MiB in a `driver: native` class: both succeeded, learned at an estimate of 1.1G, `EST 2.0cpu/4.0G (first, watched)`. After `down`, nothing of pumasi's on that host, the 6 fixed runner units untouched |
| The estimate follows a job whose memory grows | Real (Linux container, Mac native) | 4 runs of a job holding 300 → 600 → 1200 → 2400MiB: estimate 4.0G (first) → 0.6G → 0.9G → 1.8G → 3.2G. Mac: 1.1G → 1.9G at 700 → 1400MiB |
| A job bigger than its estimate finishes without being killed | Real (Linux container, Mac native) / driver test (native Linux, real systemd) | Runs that used 746M on an estimate of 576M, 1476M on 960M and 2767M on 1792M all succeeded, OOM 0. Mac: 1584M on an estimate of 1088M. Native Linux by a driver test in a VM: a scope with an estimate of 32M held 300M and ended with its exit code as is, `memory.max` = `max` |
| Page cache is not counted in the size | Real (Linux container) | A job holding 300MiB that wrote and read a 1.5G file: cgroup peak 1950M, working memory in the ledger 449M, next estimate 0.6G |
| New runners are deferred under outside load and placed when it goes | Real (Linux) | In the VM, a process outside pumasi held 4G and room went 22.9G → 18.4G: a job with a first estimate of 20G waited with `no host with 2.0cpu/20.0G free (vm1 6.5cpu/18.6G)`; when the process was killed it started at the next evaluation with `free 6.4cpu/22.7G` (16 seconds after the wait was recorded) |
| A measured job follows its measured size over the class's numbers | Real | A job sent to a class with a first estimate of 20G was placed at 3.2G, because it had been measured in another class (the key has no class) |
| Measured sizes are kept per platform (OS/driver) (11-26) | Unit + Real | Before, a job learned at 0.2G in a Mac class (`macos/native`) placed a Linux class (`linux/docker`) runner at 0.1cpu/0.2G. After the change, restarting with the same ledger filled the old rows with the class's platform, and the next Linux runner was placed from the one Linux sample alone (0.1G). Unit: samples from another OS or driver are not borrowed, nor from other jobs of the same workflow; old rows without a platform are not samples until a class fills it in |
| A job that hits the class `limit`: an incident, and the next estimate above the limit | Real (Linux container) | A 1500MiB job on `limit: {memory: 1G}`: the job failed, incident `hit class tight's memory limit (1.0G)`, estimate 0.2G → 1.5G. In the version before runs that hit the limit counted as at least the limit, the sample was 96M (the last sample before it died) and the estimate went down — the values above are from checking again after the fix |
| Actually hitting the limit that protects the host (80% of the machine) | Not run | Hitting it meant holding 80% of the VM's memory (25G), so it was not done. Computing the limit's value is unit; what happens when the limit is hit is the same path as `limit` above |
| Estimate calculation, what a runner holds = max(estimate, measured), room calculation, closing on memory pressure | Unit | Table tests in `internal/size`, `internal/decide` |
| No runner is stopped because of its size | Unit | Stop 0 in every combination of 1–1000x the estimate, pressure 0 and 99, and three states |
| One runner's memory is capped at the machine's physical memory (from the first sample, and the last sample at cleanup too) (11-27) | Unit | `internal/agent/measure_test.go`. Prompted by a real case: on a 32G Mac one job's tree was measured at 113.8G and the estimate went 12.8G → 127.2G. The fixed version was not run on the Mac again |
| Samples in the ledger larger than the host's memory are left out of the estimate (`left out` in the source), samples cut off at the limit are kept, capped at the host's memory (11-27) | Unit | `internal/size` `TestLearnLeavesOutARunAboveItsHostsMemory`, `internal/controller` `TestEstimateLeavesOutARunAboveItsHostsMemory` |
| A size no host can lend is placed alone on an empty host, at what that host can lend, the largest host first; it waits if a larger host is only closed; `overflow` room too (11-27) | Unit | `internal/decide/alone_test.go`; the wait wording in `explain` is `TestExplainJobLargerThanAnyHost`. The dashboard speech bubble (`util.js`) was only checked by having node read one reason line |
| A config without numbers works, `limit` parsing | Unit + Real | Real is the config above |
| EST, NOW and JOB SIZE in `status`, the `estimate` line in the log | Real | `EST 2.0cpu/4.0G (first)  NOW 0.0cpu/0.4G`, `hold (1)  labels.yml  0.4cpu/3.2G  0.3cpu/2.7G  0.3cpu/2.7G  4` |
| The dashboard's Job sizes table and the runners' now/est columns | Not run | Code only. The screen was not opened |
| Estimates and measurement on Windows | Not run | Only build and vet |
| Actually closing a host on Linux memory pressure (PSI 10%) | Not run | Unit only |
| Native classes are not placed on a host without the runner's libraries | Real | A VM without libicu: on joining, `note  driver: native classes will not be placed here: the runner cannot start here, it needs libicu (…)`; jobs of native classes wait without trying to start a runner, with the same words in `WHY`. `ok` on a host that has all the libraries is unit only (reading `ldconfig -p` output) |
| The agent and scope runners inherit a `BindPaths=` drop-in on a service account's `user@<UID>.service` | Real | 11-26, on a service account with linger on |

## The line (job priority)

| Behavior | Level | What was seen |
|---|---|---|
| Jobs of a high class get runners before jobs of other classes that came earlier, low ones later | Real (Linux VM, one runner at a time) | Queued build 3 · nightly 1 · deploy 1 in this order, with `{class: deploy} high`, `{job: hold (21)} low`: start order build → **deploy** → build → build → nightly |
| Changing the rules while running takes effect at once | Real | After jobs had queued, adding rules to the file gave `priority: 2 rules read again …` within 9 seconds, and the line in `status` went build·nightly·deploy → deploy·build·nightly |
| Jobs ahead of a high job in the same class are pulled along with it | Real | Both build jobs (the later one high) went before a nightly job that came earlier. `LINE` showed `ahead of hold (42) in its class, which is high by rule 3` |
| `pumasi priority`, the LINE column in `status` | Real | |
| A job that has waited over 30 minutes goes ahead of high ones, and low ones go ahead after 30 minutes too (no starving) | Unit | Table tests. Waiting 30 minutes for real was not done |
| Keeping a waiting high job's host from lower jobs, filling gaps within the same level | Unit | |
| Running jobs are not touched | Unit + Real | In the real check, jobs running when the rules changed ran to the end |
| A broken rules file keeps the previous rules | Unit | |
| The line on the dashboard (Priority view) (11-27) | Real | On a running controller's screen, `#1 hold (2) prio-a`, `#2 hold (1) prio-b`, and `#-` for a job whose runner is coming up — the same as the LINE in `pumasi priority` at the same moment |
| **Expediting from the dashboard** (11-27) | Real | 2 prio-a jobs, then 1 prio-b job 20 seconds later (one runner at a time). In a headless browser, opened the prio-b job from the line on the screen and pressed Expedite: request `POST /api/expedite {"job":"<pumasi job id>"}` (`Content-Type: application/json`, `X-Pumasi-Client: web`), answer `place 1 in line`, controller log `expedite hold (1) of run … by web 127.0.0.1: waits in class prio-b; 0 of its class go first`. Start order prio-a → **prio-b (expedited)** → prio-a, 0 console errors on the page |
| **Expedite**: first in line, earlier jobs of the same class go with it (11-27) | Real | Sent 3 prio-a jobs, then 2 prio-b jobs, and while one prio-a ran, expedited the later prio-b job by its job URL: `position 2 in line` · `1 older job of class prio-b goes first, with it`. Start order prio-a → **prio-b (pulled along) → prio-b (expedited)** → prio-a → prio-a (without expedite, the three prio-a first). The line showed `ahead of hold (1) in its class, which is expedited` |
| An expedited job is released on its own when it starts, and the time taken is kept | Real | `expedited hold (1) of run …: started on prio-b-… 3m49s after it was expedited (it waited 4m0s in all)`. Of the 3 minutes 49 seconds, about 80 seconds were a time when another process on the same laptop used all the CPU (load 55) and no job was placed |
| Expediting by run URL + `--name`, releasing by GitHub job id, the `pumasi expedite` list and recent ones | Real | A released one is kept as `cleared by cli <user>@127.0.0.1` |
| Expediting a job of an `overflow` class that GitHub still holds (11-27) | Real | One job for an `overflow` class with no other runner on its label (`TAKES 0`): not in pumasi's `status`. Expediting it by job URL gave `GitHub still holds it for class prio-o`; 2 seconds later GitHub assigned it to that class (`TAKES 1`), and 7 seconds later the runner started — before prio-a jobs that had waited 7 minutes |
| Keeping an expedited job's host (even when a smaller, lower job could fit), not keeping a host it would not fit even with every runner gone | Unit | In the real check all jobs were the same size, so this case did not come up |
| The order when several are expedited, release on cancel, run end or another runner taking it, carrying on after GitHub's reassignment (the job id changes) | Unit | `internal/decide/expedite_test.go`, `internal/controller/ops_test.go` |
| `mode high-only` and ending on its own when time is up (11-27) | Real | `--for 2m`: meanwhile 2 prio-a jobs waited with `mode high-only until 17:35 (by cli …)` (the running job ran to the end), and the evaluation 3 seconds after `until` gave `mode high-only is over … every job gets room again`. `--for 30s` was rejected with `for: between 1m and 24h0m0s` |
| Runtime rules (`priority add`, `rm`) (11-27) | Real | `class=prio-a job='hold (1)' --level high --for 10m` → `r-1`, the line to copy into the file `- {match: {class: "prio-a", job: "hold (1)"}, level: high}`, and the earlier job was pulled along and started with `ahead of hold (1) in its class, which is high by runtime rule r-1 (…)`. Removing it twice gave `no runtime rule r-1 in force` |
| Runtime rules before file rules, expiry, kept across restarts | Unit | |
| Writes only from loopback by address, and only as JSON (11-27) | Real | On a running controller, `Content-Type: text/plain` → 415, `Host: evil.example` → 403 |
| Writes from outside (403 without a token, 401 with a wrong one), and this machine needs the token too once it is open to the network | Unit | The dashboard was not opened to the network |
| Expediting a job that doesn't exist gives 404 and the reason | Real → fixed → Unit | In the first real check, GitHub's 404 was answered as 502. After the fix, 404 in unit tests |

## Taking only what fits (`overflow`)

A personal repo, one class with `overflow: true` on `labels: [self-hosted, ovf]`, one regular registered runner (`fixed-1`) with the same labels, and one VM host lending 8G of memory.

| Behavior | Level | What was seen |
|---|---|---|
| With no hosts it takes 0 and jobs go to the fixed runner | Real | `OVERFLOW build TAKES 0`. One job started on `fixed-1` 2 seconds later. The job was not in pumasi's `status` |
| With room, pumasi takes the job | Real | One job at `TAKES 2` → `build-vm1-…` (the fixed runner was idle) |
| It takes only what fits and the rest flows on | Real | 5 jobs of 90 seconds holding 3000MiB at the same time (learned size 3.7G, room 8G → `TAKES 2`). 2 started on the VM, 1 on `fixed-1` 2 seconds later. The other 2 stayed `queued` on GitHub (pumasi's `status` said `no jobs waiting`). When the earlier jobs ended 100 seconds later, `fixed-1` took one and pumasi took the other (started 6 seconds after the earlier job ended) |
| When `pumasi up` is off (after it went down closing its session) | Real (once) | 2 jobs sat `queued` for 5 minutes 3 seconds while `fixed-1` was idle, then went to `fixed-1`. Same in a class that is not `overflow` (takes every job) (11-26, once): 2 jobs created 9 seconds after the controller went down went to `fixed-1` 5 minutes 3 seconds later (in the previous day's 90-second observation they did not move because 5 minutes had not passed) |
| Two classes sharing one label, `after` | Real | Two classes, each bound to one of two Linux hosts (VM 8G, WSL host 6G), the same `labels`, `after` on the second class. 4 jobs learned at 3.7G at the same time: the first class took 2 (03:49:58.517), the second class's count opened 0 → 1 (.534) and the third job was assigned there (.823); the fourth waited on GitHub and was taken after the second class's job ended |
| Two classes sharing one label, without `after` | Real | With room on both, 6 jobs sent one at a time all went to the same class (the scale set created first, whose count was also larger, 2 to 1 — which of the two decides is unknown). 3 sent at once split 2 + 1 and all started within 9 seconds |
| A Mac class as the second class | Real (once) + Not run | The Mac class took the third job 31 seconds after the Linux class took 2. After that the Mac was closed with `memory pressure (warning)` and its count was 0 (jobs waited on GitHub and the Linux class took them), so whether the 31 seconds came from pressure could not be told |
| Computing the count, `after`, withdrawing and asking again when the count changes | Unit | `internal/controller/overflow_test.go`, `internal/decide` (`TestRoomOfAnOverflowClass`) |

## How hosts join

| Behavior | Level | What was seen |
|---|---|---|
| Joining with the invite line | Real | Linux, Mac, Windows (from an ssh session) |
| The controller joining hosts for you with `hosts: - ssh:` | Real (Linux, WSL) | A host with `shell: wsl -e sh -s` joined too (11-26), and a native runner ran one job as a user scope |
| A `listen` address that can't be reached | Real | `up` ends with `cannot reach this controller at …` |
| An unknown token, a used token, a wrong fingerprint | Real | `FAIL token` / `FAIL token` / curl ends on a pinned key mismatch. Nothing is left on the host after a failure |
| The token is not left in process arguments or files | Real | Linux, Mac |
| `--uses`, `--ttl` | Unit | |
| Reserving host names from the config, tokens bound to a name, checking the Host header | Unit | |
| A host without docker | Real | `FAIL docker: not installed` (in the version before the path was shortened) |
| Default join kind (Mac desktop) | Real | The Linux screen-session check is unit |
| A controller of another version → the host updates itself | Real (Linux, Mac) / Not run (Windows) | A new version under the same pid, running runners untouched |
| Killing the agent | Real | Back up and adopting its runners within 2 seconds on Linux, within 1 second on Mac, within 5 seconds on Windows |
| Nothing left after `leave`, `down` | Real | Linux (container, native scope), Mac, Windows. Including `down` cleaning up, over ssh, an ssh host that is not joined |
| `leave` and `down` refuse while runners are up | Real | |
| `down` after a host ended up with two agents (11-26) | Real (Linux VM) | Joined as a unit, then joined again from an ssh session that can't reach the user's systemd, making two agents (unit + restart loop), then `down`: the directory, binary and unit were gone, and the remaining agent saw `join.json` was gone and exited on its own within 10 seconds |
| Joining from an ssh session without `XDG_RUNTIME_DIR` (11-26) | Real (Linux VM) | Before, `systemctl --user` failed and it joined as a restart loop. Now it finds the user's systemd at `/run/user/<uid>`, joins as a unit, and nothing is left after `down` |
| A leaving agent writes nothing after deleting its directory | Unit | A sample after `Forget` does not recreate the record file |
| Leftovers after `down` on the WSL host (11-26, again) | Real | Joined a WSL2 host (systemd user unit, a machine running 6 fixed runners), ran 2 jobs, then `down`: 40 seconds later no agent directory, binary, unit, `/var/tmp/pumasi-*` or process. Runner images there from before joining stayed (image count 159 → 159), all 6 fixed runner units active. On joining, `ok driver: native classes can run here (…)` |
| `join.state` and `runners.json` coming back after `down` on the WSL host | Not run | The symptom itself did not reproduce on a Linux VM (in both the unit and loop ways). After blocking the three things above (the second agent, the session variable, writing while leaving), it was checked once more on the WSL host (the row above) and they did not come back. It was not reproduced on WSL with the version before the fix to pin down the cause |

## Runners

| Behavior | Level | What was seen |
|---|---|---|
| Runner images on leaving: only those pumasi pulled are removed | Real | VM. A pulled image → gone after `leave` (image count same as before joining). An image there from before joining → not recorded, still there after `leave`. A pulled image in use by someone else's container → `down` says `kept: image … stays: … container … is using its referenced image` and leaves it |
| Memory limit (container, native Linux, `docker: inside`) | Real | Only the process that went over is killed with 137, the job reports failure, OOM incident, peak = limit. This was seen in the version where the limit was the class size; the current version (`limit`) was checked again for containers only |
| Windows memory limit | Real | The allocation is refused and an incident opens, peak = limit |
| Enforcing the Windows CPU limit | Not run | |
| Mac: a runner over its estimate | Real | The job succeeded, no incident (the previous version's `ran over its class` incident was removed) |
| Cleaning up children a job left behind | Real | Linux native (including `setsid`), Mac (including `setsid`), Windows |
| Native Linux: using the host's paths and docker, passing files between jobs | Real | |
| Native Linux: without cgroup delegation it doesn't start and says why | Unit | Driver test |
| `docker: inside`: `docker run`, `services:`, compose stacks | Real | Afterwards the host docker's containers, volumes and networks are the same as before the job |
| Runner root (outside home) | Real (Linux, Windows) / Unit (Mac) | On Mac, tests up to creating the directory and cloning |
| A shared distribution read-only, fresh copies of what was touched | Real | Linux, Windows |
| Files that can't be deleted → `.trash/` and a `leftover` incident | Real | A root-owned directory a job left through a container |
| Class `env`, `path`, `{runner}` | Real (Mac native) / Unit | The values printed in the job match the config, `path` before the agent's `PATH`. Variables of the shell that ran the invite line are not in the job. Containers and Linux native are unit only |
| Wide label warning | Real | When a job with only `runs-on: [self-hosted]` came, one warning line with the job's name |
| Native runners when `image` is `:latest` | Not run | By the code, it can't read a version number and the runner doesn't start |
| Mac figures (cpu-seconds, peak memory) | Real | Match `ps` and `top` |
| Windows: a session logged on at the desk (input detection, starting at logon, windows) | Not run | Only joined from ssh sessions |
| The verdict after Windows fast startup (`died`) | Not run | Only that the boot id value is the same on every run |
| `{slot}`: concurrent runners of the same class get different numbers, and the next runner reuses a freed number | Real (Mac native) / Unit | 4 jobs of 1cpu on a budget of 3cpu: the three that started together got 1, 2 and 3, and the fourth got 2 again, which was freed first. The number went into the job's env, and three numbered directories were created (nothing below them). Restarting the controller while they ran left the numbers in `status` as they were. Keeping the number across an agent restart and "two runners never share a number" (200 concurrent starts and deletes) are unit. Container runners, Linux and Windows are unit only |

## People first

| Behavior | Level | What was seen |
|---|---|---|
| Mac: reading input time, lowering runners to background and back | Real | PRI 31 ↔ 4 in `ps`, children started later too |
| The 15-minute rule (only desktops close) | Unit | |
| Linux screen session, Windows input time | Unit / Not run | No real Linux desktop |

## Robustness

All 13 rows of `make chaos` passed on a Linux server joined by the invite line, on the current path (the host dials out). The `controller_kill` row first failed on a missing notification; after the fix only that row was run again.
**11-26, again**: after placement changed (placing by measured size, separating estimates from limits, holding unknown jobs, priority, overflow), all 13 rows were run again in one go and all 13 passed (64 minutes). What changed in the script was one phrase the `oom` row looks for (`hit class linux-s's memory limit (1.0G)`), and writing `limit: {memory: 1G}` on that row's class — the limit is no longer the class size.

| Behavior | Level | What was seen |
|---|---|---|
| **A busy runner is not deleted by any path** (except `maxDuration`) | Unit + Real | Table tests per path. Real GitHub refusing to delete the registration of a busy runner is real |
| Nothing placement does stops a runner running a job | Unit | `TestNothingPlacementDoesStopsARunningJob`: Stop 0 in 288 combinations of 1 · 3 · 50x the estimate, being watched, holding unknown jobs, overflow, closed hosts, pressure, waiting jobs of higher and lower levels, and other classes' reserve |
| Not deleting when deleting the registration can't be asked, runners that came up late, a launch at the moment a host leaves | Unit | |
| Killing the controller (kill -9) | Real | Came back with `adopted 2 runners`, the job succeeded |
| Two controllers | Real | The second names the first's pid and exits |
| Agent death and restart | Real | Runners untouched, cpu-seconds carry on |
| Host connection lost (agent paused for 60 seconds) | Real | An `unreachable for … (keepalive timeout)` incident opened and closed, the job succeeded. A real network cut was not tried |
| Host reboot | Real (simulated) | By changing the boot id and killing the runners. Incident + lost jobs, rerun with `rerun` and succeeded. A real reboot is not run |
| A runner dies mid-job | Real | Incident; GitHub closed it about 10 minutes later, then it was rerun and succeeded |
| A runner fails to start → quarantine | Real | Quarantine incident after 3 in a row |
| Memory limit incident, no rerun | Real | |
| `maxDuration` | Real | Incident the moment it is exceeded, runner stopped, no rerun |
| Low disk | Real | By raising the threshold: incident, new placement stops |
| Disk is measured by the fullest filesystem runners live on (the data directory, the native runner root, the `work` and `{slot}` directories of runners started or adopted — the parent if missing), once per device number, with the path in the close reason, incident and `explain` (11-27) | Unit | `internal/agent/disk_test.go` (a fake statfs with an HDD at 81% and NVMe at 2%, and this machine's real statfs with two directories on one filesystem), `native_dirs_test.go` (a restarted agent adopts directories from `pumasi.json`), `internal/controller` `TestDiskLowNamesThePath`. Prompted by a real case (a Linux host, data HDD 81% free · work NVMe 98% full), but the fixed version was not run on a real host. The docker driver's data root goes the same way as before (`docker info`). Telling Windows volumes apart (`GetVolumePathName`) and the error check for missing paths are only build and vet |
| A broken ledger | Real | Moved aside, started with an empty ledger |
| A wrong GitHub token | Real | `up` ends naming it |
| **45-minute job** | Real | On Linux (container) and Mac each: while it ran, controller kill -9, restart for a version swap, agent paused for 60 seconds, load 120 on the controller machine — the job succeeded |
| **45-minute job, controller run as a service (11-26)** | Real | While one job (45 minutes) ran, in the same order: controller kill -9 (the service brought it back) → agent kill -9 → outside load on the host for 4 minutes (6 cpus + 3G) → controller and agent kill -9 at the same time. **Mac** (LaunchAgent controller, Mac host): 46 minutes 44 seconds, succeeded — the controller came back after 8 and 10 seconds with `adopted 1 runners`. **Linux** (LaunchAgent controller, container runner on a Linux VM host, agent as a systemd user unit): 45 minutes 6 seconds, succeeded — the controller came back after 4 seconds, and 20 seconds for the simultaneous kill, with `adopted 1 runners, 0 jobs finished meanwhile`. Both agents had a new pid within 20 seconds. Because of the dead session left on GitHub, the returning controller could not take new jobs of that class for 46 seconds to 1 minute 17 seconds (unrelated to the running job) |
| The controller as a Mac LaunchAgent | Real | nice 0; after kill -9, launchd brought it back and it adopted the runners. Hours on a loaded Mac are not run |
| `pumasi service install` — Mac (11-26) | Real | Came up as a LaunchAgent. kill -9 → new pid 2 seconds later. kill -9 while a job ran: `controller is back: was down 9s, adopted 1 runners`, that job succeeded, and a job queued while it was dead also ran and succeeded after the session reopened. Comes back after `SIGTERM` too (5 seconds). No token in the plist or the process environment, token file 600 |
| `pumasi service install` — Linux (11-26) | Real (VM) | Came up as a systemd user unit. kill -9 → new pid 5 seconds later (`started again 1 times`), session reopened 77 seconds later. No token in the unit or the process environment, token file 600. Joining hosts and running jobs was seen only on the Mac side |
| `service remove` | Real (Mac, Linux VM) | Returns after the controller has ended. The unit/plist and token file are gone and it does not come back. After that, `down` removed the rest |
| `up` and `down` while the service runs | Real | Refused, pointing to `service remove` |
| Token file: the environment variable first, refused if others can read it | Unit | |
| Hosts report the controller's absence (11-26) | Real (Linux VM + Mac host, `away: 1m`) | kill -9 on the controller: the first host posted to the webhook once at 1 minute 3 seconds, the second once at 2 minutes 2 seconds, and nothing more up to 4 minutes. On restarting it, the two hosts each posted the recovery once. When it was paused for 35 seconds and restarted, and after `down`, nothing was sent. Webhook file 600 |
| Absence notice: picking only two and succession, only once, agent restart, absences not delivered, the second at double the time | Unit | With three or more hosts, the third staying quiet, and succession when the host that was speaking drops, are unit only |
| Absence notice: when only the host's network is cut, a laptop host waking from sleep, with the default 5 minutes as is | Not run | |
| Credential exposure warning (11-26) | Real (Mac) | A host joined on the same Mac as the controller, as the same user: class and reason in the `up` log and in `WARNINGS` in `status`. A Linux VM host joined to the same controller: no warning. Restarted with `jobsMayReadCredentials: true`: none. Also confirmed that the same user can see the controller's token variable with `ps eww` |
| Credential exposure verdict: native on Linux, a user with docker, `docker: inside`, mounting the state directory, a Mac of another user | Unit | Table tests. Controller and host on one Linux machine was not seen for real |
| The credential exposure warning on the dashboard | Not run | The screen was not opened |
| GitHub App auth (11-26) | Unit + partly Real | Unit: gets an installation token with a JWT signed by the App's key and gets it again 5 minutes before expiry (fake GitHub), PKCS#1 and PKCS#8 keys, config, environment variables, both given, a key not at 600. Real: starting `up` with an App that doesn't exist on github.com (id 1) ended with `404 Integration not found` on `POST /app/installations/1/access_tokens` — the signing and the request reached real GitHub. **Creating a scale set and running jobs with a real App is not run** (creating an App has to be done by the account owner in a browser) |
| The token (PAT) path unchanged after adding the App | Real | `up` → session → `down` |
| Whether run lookups and reruns work with only the permission table's Actions: Read / Read and write | Not run | Only run with a classic PAT (`repo`). The runner registration permissions are copied from the table in GitHub's docs |
| A controller run as a service, after a **reboot** | Not run | Only the laptop and a VM used for other work were available, so neither was rebooted. Linux with linger off and a Mac with no one logged in on its screen only have the guidance messages and were not seen |
| Joining hosts for you with `hosts: - ssh:` from the service | Not run | The service has no ssh agent. Seen only with hosts joined by the invite line |
| A host thrashing under someone else's load (load 200, swap full) | Real | The agent couldn't run for over 10 seconds, the connection dropped and reconnected. An incident with the reason, the job succeeded |
| A job waiting long (`waitAlert`), GitHub API failures, runners with no owner | Unit | |

## Measured on GitHub's side

| | Value |
|---|---|
| `runs-on` matching | A job comes only when all its labels are in the scale set's labels. `self-hosted` must be put in the scale set |
| A scale set with just one trial label (`labels: [trial]`, `runs-on: [trial]`) | Jobs come. You can try someone else's workflow on a branch by changing only `runs-on`, without touching existing labels (an organization repo's gradle job and a browser e2e on hardware WebGPU were checked this way on a Mac host, 11-26) |
| A scale set with a fixed runner's labels (`labels: [self-hosted, macOS, ARM64, ci-pool]`) | It is created (the default labels aren't rejected either). `runs-on` with the full labels, a subset, or different case all come to the scale set |
| A regular registered runner with the same labels idle alongside | All 14 jobs went to the scale set (0 to the fixed runner). Regardless of whether the scale set or the fixed runner was created first |
| The scale set exists but `up` is off | Jobs wait `queued` even when the fixed runner is idle (observed for 90 seconds) |
| After deleting the scale set | The fixed runner takes new jobs at once. Jobs that were waiting at deletion went to the fixed runner after about 3 minutes |
| Waiting with no runner | After being reassigned three times at 5-minute intervals, the messages stop. The job stays `queued` |
| A runner disappears mid-job | The job turns `failure` after about 9 minutes 40 seconds. Meanwhile the registration can't be deleted. No automatic retry |
| A deprecated runner version | Ends with exit code 0 right after connecting |
| `JobStarted` message | 30–50 seconds after the runner has taken the job |
| SIGINT to a runner (listener) (11-26, measured with registered runners) | Listening with no job: ends within 1 second and deletes its own session (dozens of times, each time the next listener listened at once). At the moment of taking a job (`Acknowledging runner request`): ends within 1 second, the job stays in the queue, and another runner that started listening 2 seconds later took it. After `Running job`: the job dies and turns `failure` about 10 minutes later. While opening the session (4–5 seconds into boot): once, the session stayed on GitHub and that registration could not listen for over 2 minutes because of Conflict |
| A dead controller's message session (11-26) | After dying (kill -9) without closing its session, opening a new session on the same scale set gives `409 Conflict`. It opened 34, 79 and 84 seconds after the kill (three times, asking every 15 seconds). Jobs that came meanwhile were not assigned; they waited and came after the session opened |
| `JobAvailable` and `AcquireJobs` (11-26) | Never sent on github.com. In about 250 messages so far and in sessions opened by hand, jobs always came straight as `JobAssigned` (`runnerRequestId` 0, `totalAvailableJobs` always 0) and there was nothing to acquire. **There is no way to acquire only some jobs and choose** |
| The order assigned jobs go out to runners (11-26) | The order they were assigned. 4 jobs assigned in the order 1 · 2 · 4 · 3, then runners started one at a time, took them in the order 1 · 2 · 4 · 3. A runner pumasi started "for this job" also takes the class's oldest job (the order within one scale set can't be chosen) |
| `maxCapacity` in `GetMessage` (11-26) | A cap on the number of jobs assigned to that scale set, and running jobs count. Set to 1 with 3 jobs: only one assigned, and after it ended the next was assigned on the next poll. For the 66 seconds a 100-second job ran, the next job was not assigned |
| Jobs not assigned because of the cap (11-26) | They stay `queued` and are not in the statistics either (`totalAvailableJobs` 0). Opening a second scale set with the same labels got them assigned there 1 second later, and attaching a regular registered runner with the same labels got it to take them 3 seconds after it started listening. (Jobs assigned with no cap do not go to fixed runners, as in the rows above) |
