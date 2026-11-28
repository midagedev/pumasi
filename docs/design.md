# The design as it stands

This describes what the code does now. What was checked on real hosts is in [verified.md](verified.md), and the background research is in [research/](research/).

## The problem

These things keep happening when you spread self-hosted runners over several machines.

- GitHub puts a job on any idle runner, **whatever the load on its host**. The only handle is the number of runners.
- Jobs differ in size. One lint run and one e2e run that starts a JVM, a database and a browser each count as one runner.
- Containers and directories a job leaves behind eat into the next job's resources.
- When a machine someone works at is lent as a runner, the person and CI get in each other's way.

So: place by job size and host headroom (when there is no room, the job waits in GitHub's queue), end each runner after one job, and give no new work to a machine someone is sitting at.
Kubernetes is not required.

## Scale sets are the only handle GitHub gives

pumasi is built on GitHub's official `actions/scaleset` client. The properties of that API decide the design.

| Fact | What it means for the design |
|---|---|
| Within a scale set, GitHub gives a job to any idle runner. A given job cannot be sent to a given runner | **One scale set per class (size, OS).** It is the only way to sort jobs |
| When a job is assigned, its repo, workflow and job name arrive before any runner exists | pumasi sees what is waiting and decides how many runners to start, and where |
| A job goes to a scale set when all of its `runs-on` labels are among the scale set's labels. Labels are set when the scale set is created, and can include `self-hosted`, OS and architecture | Default labels are `[self-hosted, pumasi-<class>]`; a class's `labels` changes them |
| When a matching scale set exists, ordinary registered runners with the same labels get no jobs even when idle. Jobs don't fall through to them even when the scale set has no runners | Moving off fixed runners happens all at once, per label set. Going back means deleting the scale set (`down`) |
| There is no API to list runners. A dead runner's registration stays until deleted | **The controller keeps a runner ledger (SQLite).** It is reconciled with the agents' snapshots |
| Reporting capacity 0 hides the waiting jobs | Capacity is always reported generously, and jobs are made to wait **by not starting runners**. Only a class that says its jobs need not be visible and should flow to other runners (`overflow: true`) reports its real room |
| An assignment nobody takes is taken back after 5 minutes and comes again; after three times the messages stop | Deferred demand is kept in the ledger, and when room frees up pumasi starts runners on its own, without a message |
| GitHub refuses to delete the registration of a runner holding a job | Before removing a runner that is up, pumasi **first asks to delete its registration** (see Robustness below) |
| When a runner dies, GitHub closes its job as failed only after about 10 minutes, and does not rerun it | The agent sees runner deaths directly; reruns are done with `rerun:` |

## Parts

```
                 GitHub (scale set API)
                        │  listener (one per class)
              ┌─────────▼─────────────────┐
              │ controller                │  pumasi up
              │ classes · ledger · Decide │  listen :7411
              └───▲───────────▲───────────┘
                  │ one TLS connection each (the host dials)
          ┌───────┴───┐   ┌───┴────────┐
          │ agent     │   │ agent      │   one per host, a service of that user
          │ docker    │   │ native     │   runner = container or process
          └───────────┘   └────────────┘
```

- **controller**: takes demand through one listener per class, decides placement from the hosts' snapshots, and orders "one runner of this class on this host".
  It makes a JIT config for one runner and hands it over. The GitHub token exists only on the controller.
- **agent**: one per host. It starts and removes runners and answers with host and runner metrics. It has no GitHub credentials.
- There is one binary (`pumasi`). A host fetches the build for its OS from the controller.

## Classes and placement

A class = one scale set = a runner's OS, driver and labels. Its keys are in the table in the [guide](guide.md#classes). Size belongs to the job, not the class (see "Size" below).

Placement is a pure function `Decide(snapshot) -> Plan`, and every decision leaves a reason string (that is the wait reason).

1. **Demand**: waiting jobs per class, minus that class's runners already starting or idle. Each remaining job carries its size.
2. **Eligibility**: a host that matches the class's OS, `hosts` and `mounts`, is not closed (connection lost, quarantined, low disk, in use by a person, CPU pressure (PSI) 40% or more, memory pressure (PSI) 10% or more, closed by the host itself),
   and has `room − other classes' reserve ≥ job size`. For CPU room, see "CPU is shared" below.
3. **Ranking**: servers first, then desktops. Within the same kind, the one with more headroom.
4. **If there is no room, it does nothing.** The job waits in GitHub's queue, and after 10 minutes (`waitAlert`) it becomes an incident.
5. No idle runners are kept. A runner starts when a job comes (a few seconds to boot), and a runner that got no job is removed after 5 minutes.

There is no cap on runners per class. The only cap is the hosts' room.

### The line: the order when room is short

`Decide` puts waiting jobs in one line and gives room from the front. By default the order is arrival. When `priority` rules give a job a level (high · normal · low), the line is ordered by `first seen − level × headStart`.

- **It moves jobs up by time, not by level.** With levels as strict tiers, low jobs starve. Counting in time needs no separate aging: anyone who waits long enough is in front.
- **GitHub's handle exists only between classes.** A scale set on github.com never gets jobs to choose from: jobs arrive already assigned with `JobAssigned` (`JobAvailable` and `AcquireJobs` were never used),
  and assigned jobs go to that scale set's runners in assignment order. There is no way to tie a runner to a given job. All pumasi decides is which class's runner starts first.
- **So a level is passed on to the jobs in front.** If a high job is further back in a class's line, the jobs ahead of it count at the same level (`inherit`). The high job can only go once the jobs in front of it go, so this is the fastest way to get it out. The reason says who pulled them forward.
- **Room is kept for a higher job that waits.** On hosts where a job deferred for lack of room could go, no job of a lower level is newly placed in that evaluation. Within one level, small jobs fill the gaps as before. A lower job is held back no longer than headStart × the level difference (after that it is ahead in the line).
- **Nothing is pushed out.** Running jobs and runners already up have nothing to do with the line.
- **Applied at once.** On every evaluation the controller checks the config file's modification time, and if it changed, rereads only `priority` and `priorityHeadStart`. If it can't read them, it uses the previous rules. When the rules change, a `priority` row is left in `decisions`.
- **Runtime rules live in the ledger.** Rules from `pumasi priority add` are in the ledger (`priority_rules`) and are checked before the file's rules. Those with an end are left out of evaluation after it. The file is not rewritten because when a program edits a file people also edit, one of the two loses; to move a rule into the file, a person writes the one line `pumasi priority` shows.

**Expedite is room, not a level.** An expedited job ranks above every level (`1<<20 − expedite order`), and the one expedited earlier is in front. With this rank the mechanisms above work unchanged:
the jobs in front inherit it (`inherit` passes on the expedited mark too), and when there is no room, the hosts that job can go to are kept for it. One more condition was added to keeping a host: **a host the job would not fit even with every runner gone is not kept.** Keeping a host empty for a job that will never come, however long it waits, only blocks (the same holds for high jobs).

- **The key is (repo, run, job name).** The job id in scale set messages changes every time GitHub reassigns the job (5 minutes with no runner). The GitHub job id and the pumasi job id given at expedite time are used only to find the job.
- **A job GitHub still holds** (one an `overflow` class has not taken) is put in the line as "a job to come": it stands at the back of that class's line (jobs already taken go first), starts no runner, keeps room if there is room, and opens that class's number taken to at least 1.
  Once the job is actually assigned, it becomes an ordinary job in the line. Even then GitHub assigns the oldest job of those labels first, so there is no guarantee the "job to come" is exactly that job. pumasi says so at expedite time.
- **Jobs taken for an expedited job are not given back.** An `overflow` class gives back jobs it can't place (it lowers its number so GitHub reassigns them), but an expedited job and the jobs it pulled are held until room frees up.
- **Released**: when that job's row in the ledger is closed by a runner (it started), when it is closed because the run ended, or when it is gone from the line. For a job not in the line, GitHub is asked once a minute (`in_progress` means another runner took it, `completed` means it ended). The time from expedite to start and the runner that took it are kept in the `expedites` table.

**When will it start (forecast).** Each waiting job gets `starts in ~3m on server1, when e2e (chromium) ends`. `decide.Forecast` runs the current snapshot forward: a running job runs for the median of its last 10 run times and ends (if it is already past that, a tenth of it later, at least 15 seconds), `Decide` is called again at each end, and runner boot time is the median of recent runners. It runs placement itself, so the line, reserve, expedited jobs and blind holds are all reflected.
  What it doesn't know: jobs that will come later, hosts that will close or open (a person sits down, a connection drops). CPU pressure is assumed to ease once one job on that host ends. When a job with unknown run time is in the way, when it waits for a closed host, or when the time is past 2 hours, no time is given; only the wait reason remains.
  Why the median: for size, too little kills the job, so the max plus a margin is used; for a start time, being early and being late are equally wrong, which is better.
  How accurate (`go run ./internal/race/cmd`, 10 seeds): every job that waited more than 20 seconds got a time; the median error of the first time given was 2–16 seconds, p90 14–70 seconds, and 90–100% were "within 60 seconds or 30% of the time waited" (even with a long tail added where a tenth of run times double and a twentieth fail early). It mostly says too early (mean +1 to −21 seconds). In the demo, p90 is a little over 100 seconds because of desktops people sit at and hosts that drop off.

There is one **mode** now: during `high-only`, `Decide` does not place jobs that are neither expedited nor high (they stay in the line). It always has an end (default 1 hour, at most 24 hours), because a forgotten switch blocking every job is the worst accident this feature can cause. It is kept in the ledger's `settings`, so it survives restarts.

A handle not used: the `maxCapacity` sent when receiving messages caps the jobs assigned to that scale set (running ones included), and jobs over it stay unassigned and go to another scale set or fixed runners with the same labels (measured in verified.md).
pumasi takes jobs with no cap for now. Taking only what it can would let overflow jobs flow to fixed runners with the same labels, but that changes how "moving over" behaves, so it is decided separately.

### Size: nobody edits numbers

When people write limits, a job dies every time it grows, and someone raises the number. So the numbers are split in two: the **size placement counts** is measured, and the **limit that is enforced** is set by the host, not the job.

- **The job's key** = repo · workflow file (ref stripped) · job name · platform (OS/driver, `linux/docker`). The first three come in the waiting job's message, and the platform is that of the class that took the job, so the size is known before a runner starts.
  The class name is not in the key: move a job from `linux-s` to `linux-l` and its measured size follows.
  **Why the OS is in it**: a job with the same name on Linux and on Mac is a different program (a different toolchain, different binaries). Mixed, the larger one inflates both.
  **Why the driver is in it too**: what is measured differs. A container runner's sample is that container's whole cgroup (with `docker: inside`, including the daemon started inside and its containers); a native runner's sample is the runner process and its descendants, and what the job started with the host's docker is not included. The tools differ too (what is in the image vs what is installed on the host).
  Splitting is cheap: the same job rarely moves between two drivers on one OS, and when it does, its first run on the other side is placed once as a "job never measured". The cost of not splitting is a quietly wrong number.
  Architecture (arm64/amd64) and whether `docker: inside` is set are not split: differences within the same OS and driver are left to the max × 1.2 margin (no measurement backs this).
  **Old ledger**: rows from before the platform was recorded are filled in when the controller starts, with the OS/driver of the row's class if that class is in the current config (a class's OS is assumed not to change). Rows of classes gone from the config stay without a platform and are not samples, and that job is learned again.
  A matrix job has its values in its name (`build (linux)`), so each combination is separate. A job called through a reusable workflow comes with the name `calling job / inner job`, and its workflow is **the called file**.
  Calling the same file twice within one workflow still keeps them apart, but different workflows calling the same file with a job of the same name are merged under one key (it is just placed at the larger size).
  A job whose `name:` holds an expression that changes every run gets a new key every time and builds up no samples of its own; it is placed by "borrowing from the same workflow" below.
- **Sample** = the ledger row of a runner that finished and was cleaned up: memory is the peak without page cache (`mem_work_peak`), CPU is `cpu seconds / run time`.
  cgroup's `memory.peak` includes cache, so it is not used: a build that reads many files fills whatever it is given (real case: a job holding 300MiB that read a 1.5G file had a peak of 1950M, and 449M without cache).
  On Linux, `memory.current − (file − shmem)` is read every 5 seconds to catch the peak. On Mac and Windows it is a sum of process memory, so there is no cache in it to begin with.
  Old rows without the no-cache value are left out of samples. A run that lost a process at a memory limit only tells us "it needed more than the limit", so its sample is raised to at least the limit.
  **One runner's memory never exceeds the machine's physical memory.** The agent clips every sample to that value (on every OS). The Mac value is the sum of each process's footprint (the "Memory" column in Activity Monitor),
  which counts copy-on-write pages shared after fork, shared mappings and GPU buffers once per process, and counts compressed memory at its original size. Real case: on a 32G Mac, one job was measured at 113.8G.
  libproc has no value that measures one process tree without overlap (checking each region for sharing is too expensive for every sample), so the sum is kept and only capped. A clipped value can still be larger than the truth; that job is then placed alone next time as a "size no host can lend", by the rule below.
  **Such samples already in the ledger** (written by a version before the clipping) are left out of the estimate: the controller does not count a sample larger than the physical memory it last heard from each host, and notes `1 run that held more than its host's memory left out` in the estimate's source.
  The ledger keeps what the agent said (it is a record), and the filtering happens where learning happens (`size.Learn`): that is the only place estimates are made, and only the controller knows the hosts' memory. A sample cut off at a limit is stored clipped to the host's memory.
- **Estimate** = the max of the last 10 samples × 1.2 (× 1.5 for samples cut off at a limit), memory rounded up to 64M, floor 0.1cpu/256M. With no samples, the class's `cpus`/`memory` (default 2/4G).
  Why the max: a job that grew is believed at once, a job that shrank is believed after watching 10 runs. When wrong, it leans to the cheaper side.
- **A job never measured** is handled one of two ways.
  **If the same workflow has something known, it borrows**: the finished samples of the workflow's other jobs (last 30), and what jobs running now for over 3 minutes hold right now; the larger of the max of those × 1.2 and the class's first estimate. Since it borrows the largest, a small job is placed big for its first run (it learns its own size when it ends).
  **If the workflow has nothing known either, it is an "unknown job"**: the class's first estimate is only a guess, so that runner holds 3× the first estimate in memory until the job has run 3 minutes (CPU unchanged: a busy CPU only slows things down), and a host watching an unknown job takes the next unknown job only when that 3× is free. A host watching nothing takes one if the first estimate fits, because even on a small host where 3× does not fit, a job has to be measured once.
  After 3 minutes the runner goes back to max(estimate, current use), and what that job holds then becomes the size the rest of the workflow's jobs borrow.
  Why: a JVM build job guessed at 4G can pass 8G on its first run. When several such jobs come at once (a new workflow's matrix, the first day with an empty ledger), they are placed side by side by the guess and the host overflows. 3× and 3 minutes are chosen, not measured: a margin over that case (a little over 2×); a job that has not finished growing within 3 minutes is caught after that by "current use" and pressure detection. The price is that when unknown jobs bunch up, the later ones wait up to 3 minutes longer (or until the earlier job ends), and only once per job.
- **What a runner holds** = max(estimate, current use). The estimate is written to the ledger when the runner starts, and replaced with that job's estimate when GitHub reports the runner's job (JobStarted): a runner started for the longest-waiting job can get a different job.
- **A host's room** = `machine − Σ held by runners − current use by non-runners − memory reserve`; with a `budget`, the smaller of that and `budget − Σ held by runners`.
  "Use by non-runners" is measured by the agent (Linux `MemTotal − MemAvailable − runners' working memory`, Mac memorystatus, Windows free physical memory).
  The memory reserve is 10% of the machine (1–4G): memory does not stretch, and the measured values are a few seconds old.
- **A size no host can lend.** If the estimate is one that none of the hosts the class can go to (matching the class's `hosts` and OS, and having answered at least once; counted even if closed now) can **ever** lend,
  that is, larger than the largest `min(machine − memory reserve, budget)` (CPU: `min(cores, budget)`), waiting does not help that job. So the size on that axis (CPU and memory separately) is
  cut to **what that host can lend with no runners** ("a host's room" above with all runners removed) and placed: when the host is empty, the job runs there alone. If several hosts are empty, it goes to the one that can lend the most.
  While it waits, that host is kept for it as for any other waiting job (lower jobs are not let in). The reason carries `its size 6.2cpu/127.2G is more than any host of mac-diag can ever lend (10.0cpu/29.0G at most): it runs alone on mac-a`,
  and the waiting job's entry in `explain` says the same, plus "the measured value may count shared memory twice". An `overflow` class's "how many more can be placed" is counted by the same rule (one per empty host).
  If a larger host is only closed for now, the size is not cut and the job waits for that host. Hosts that never answered (size unknown) are not counted, so one large host that is off does not make a job wait forever.
  Why: in a real case, one job's mismeasured value (113.8G) made the estimate 127.2G, which never fit the class's only host (32G), so the jobs GitHub gave that class waited 140 minutes. The job's key has no class, so a value measured in another class blocked this class.
- **Limits.** The estimate is enforced nowhere. All a runner gets is (a) the class's `limit`, or without one, (b) the memory limit that protects the host = 80% of the machine (or the `budget` memory if smaller). CPU is not limited without (a).
  **(b) is the line between "never kill a running job" and "never kill the host"**: one runner passing 80% of the machine is not a problem of that job's size but one that affects every job on the machine, so only then does the kernel kill the process that went over.
  Several runners growing together until the machine runs out cannot be stopped by limits; the memory reserve and pressure detection only stop new runners first. Mac has no way to apply either (a) or (b).
- **CPU is shared (`cpuOvercommit`, default 1.5).** Memory room is as above. CPU room is `cores × cpuOvercommit − CPU used outside pumasi − CPU held by runners`.
  But only on a host that meets all four conditions: it is a server, has no cpu `budget`, measures CPU pressure (PSI) (Linux), and has a CPU pressure threshold (40). Everywhere else it is the cores as they are.
  Why: when CPU is short, jobs share it and slow down but lose nothing, and CPU pressure says directly whether it is short. When memory is short, jobs die, and current use does not tell the peak to come. So only memory is held strictly to the estimate.
  The basis for 1.5: an estimate is the largest average of the last 10 runs plus 20%. The 20% is margin for memory's momentary peak; CPU's momentary peak only slows the job.
  Even in the worst case, where every job placed up to 1.5× uses its own peak average, demand is 1.5 / 1.2 = 1.25× the cores. Then each job is at most 20% slower, and if CPU pressure rises before that, nothing more is placed.
  In the `go run ./internal/race/cmd` simulation (a model where sharing cores slows jobs proportionally), going past 1.5 (2, 3) did not cut waits further (memory fills first). Even in the worst flow, where jobs use all their CPU from start to end, run time grew by 2% at p90.
  Desktops and a cpu `budget` are a person's share or a promise, so they are not exceeded (the agent also refuses a start beyond its budget). Mac and Windows cannot measure CPU pressure and so have nothing to stop it, so they are not exceeded.
  The start reason says when cores were exceeded (`cores shared: runners hold …cpu of … there, cpu pressure …`). The `Xcpu free` in a wait reason is counted with this room.
- **What you see.** A start decision's reason states the size and its source (`takes 0.4cpu/1.8G of the free … for build (learned from 3 runs, …)`), and when an estimate moves by more than a quarter, an `estimate` decision is left in `decisions`. `status` and the dashboard show each runner's estimate and current use, and each job's next size, last run, recent max and sample count.

### Keeping runners up ahead (`warm`): taking runner boot time out of the wait

pumasi starts a runner after a job comes. So even with plenty of room, every job waits for a runner to boot (JIT registration + container start, 8–10 seconds). Fixed runners that are already up have no such wait.
So each class keeps a few runners up without a job. GitHub gives a job straight to a listening runner of that class.

- **How many.** Not set by people. It is learned from how jobs of that class came in the last 30 minutes (`decide.WarmWindow`): jobs that came within 15 seconds of each other (one push's workflows, one matrix) count as one batch, and the count is the size of the largest batch (`decide.WarmCount`).
  A class that had only one batch (a once-a-day deploy) gets 0: one arrival says nothing about the next. After 30 quiet minutes it becomes 0, and the remaining runners are reclaimed by the usual idle timeout (5 minutes).
  `warm` (default 8) is the cap. `warm: 0` turns it off. Overflow classes don't have it.
- **Size.** Which job will come is unknown, so each holds the size of the largest job the class got recently (the same value as an overflow class's next-job size). When a job comes, it switches to that job's size. Memory is held strictly.
- **Where.** Only in room no waiting job wants: not on a host kept for a waiting job, not for a class that has jobs waiting for room, and not during high-only. `maxStarting` and `reserve` still apply.
- **Giving way.** When another class's waiting job would fit only in room held by runners without a job, those runners are stopped and the job is placed in the same plan (start reason `once 1 spare runner(s) without a job give it up`). The number an overflow class can take also counts that room as free.
  Only "spares" give way: the class's runners without a job, minus the jobs GitHub holds for that class. A runner whose worker is running (the job started but GitHub's notice is late) is not a spare.
  Stopping goes through deleting the runner on GitHub first, so if it got a job at that moment, it cannot be deleted and is left alone.
- **People first, and pressure.** When a host stops taking that class (a person sits at the desktop, disk, CPU or memory pressure), its spare runners leave at once. If they stayed, GitHub would give the next job to that host without asking.
- **idle timeout.** The `Warm` runners per class that most recently started listening have no idle timeout. Any beyond that, and those on hosts not taking jobs, stop after 5 minutes as usual.
- **`warmMin`.** Set on a class, it keeps that many up even when the learned count is smaller (sized at the largest of the jobs the class got last). These runners do not give way: the operator wrote that "this class must not wait". On a host that stops taking jobs (a person, pressure) they leave like other spares.
  In the harness, with `warmMin: 1` on the deploy class (native boot, `needs`, 10 seeds): deploy-api's wait went from 8–11 seconds on average to 0.6 seconds, and at most 1 second even in the busiest flow (15–25 seconds). The cost is the 6–7G that runner holds, and other jobs waiting that much longer when room is short: overall wait p90 went 18.7 → 23.5 seconds in the 25–40 second flow (16.3 → 18.9 seconds with `warm` on), and 94.5 → 100.5 seconds in the 15–25 second flow. When it was allowed to give way, deploy-api again waited up to 36–37 seconds when busy, because after it gave way another job took the room.

**Why hold the size ahead of time.** If a runner started without holding room, by the time a job came the host's memory might already be given to other jobs. Held room is given back by giving way, so the cost falls not on waiting jobs but on room nobody wants.

**Measured.** `go run ./internal/race/cmd`, the same 4 hosts and the same jobs, mean of 10 seeds, 8 seconds for a runner to boot:

| Push interval | Fixed per class (tuned after the fact) | pumasi, without warm | pumasi |
|---|---|---|---|
| 50–70 s | 0:01 / 0:02 / 1:20 | 0:09 / 0:10 / 0:18 | 0:01 / 0:08 / 0:14 |
| 35–55 s | 0:01 / 0:02 / 1:42 | 0:09 / 0:10 / 0:39 | 0:01 / 0:09 / 0:21 |
| 25–40 s | 0:01 / 0:12 / 2:19 | 0:09 / 0:25 / 1:26 | 0:05 / 0:24 / 1:18 |

(Wait median / p90 / max.) In flows with room to spare, the median matches fixed runners, and the max is shorter in every flow. In the busiest flow, room is full and there are few places to start runners ahead, so the median only gets to 0:05. p90 is still 8–9 seconds:
the first push (nothing learned yet), jobs that come once a day, and jobs on a host (the Mac) that cannot hold room for one more of the class's largest job next to a big job that is running.
Cost: room held by runners without a job averages 16–28% of memory and 21–36% of CPU (2–4% without warm). 81–92% of the runners started ahead got a job, and 0.5–4.6 runners per run (124 jobs) stopped without one.
The quieter it is, the more room is held, because it uses room that is left over. The simulation takes GitHub's delay in giving a job to a listening runner as 0 (the same for fixed runners).

**Delayed jobs.** Even with giving way, some jobs start late because of runners without a job: a runner still booting cannot give way, and sometimes what a giving-way runner holds is enough memory but not enough CPU.
The same moment's snapshot is placed again with only the runners started ahead without a job removed, and the waiting jobs that would then have started are counted one tick (2 seconds) at a time (`warmCost`). 10 seeds, about 124 jobs per run:

| Push interval | Delayed jobs / total seconds delayed (most-delayed job) | Total wait cut by `warm` |
|---|---|---|
| 50–70 s | 0.1–0.7 / 0–4 s (4–6 s) | 712–838 s |
| 35–55 s | 0.7–1.6 / 2–8 s (4–8 s) | 679–823 s |
| 25–40 s | 2.3–2.9 / 9–14 s (6–22 s) | 435–541 s |
| 15–25 s | 0.5–1.6 / 2–6 s (6–10 s) | 264–343 s |

(The ranges are across flows with an 8-second runner boot, the native distribution, and `needs`.) The seconds delayed are at most 3% of the wait cut, so this was not fixed. Shrinking the size from the class's largest job to a typical size was not done either: a big job landing on a runner that held little would hold more than the host's memory.

### Starting ahead (`ahead`): jobs that come through `needs:`

A job with `needs:` (e2e after unit tests, a web deploy after an api deploy) gets a runner from GitHub only after the job before it ends. A runner started then means that job always waits for the boot. `warm` sometimes cannot cover this: a class with only one batch, like a once-a-day deploy chain, and the first push.
So pumasi watches when the earlier job will end and starts that much earlier.

- **Who comes after whom.** Learned from the ledger (`decide.LearnNeeds`, the last 7 days, every 10 minutes). In one workflow run, a job that came more than 15 seconds after the first jobs is taken to have waited for a job in the same run that ended just before it (within 1 minute). If that happened more than twice, and in more than half of the runs where the earlier job ended, the pair is kept, and the median time from that end to the arrival is used as the delay. Manual reruns and jobs that waited for approval take over 1 minute, so they don't become pairs.
- **When.** When a running job will end is the same value as the wait ETA (the median of that job's recent run times, `decide.EndOf`). If the job after it is likely to come within runner boot time + one round (`evaluateEvery`), one runner is started for that job's class. Jobs already here and jobs more than one step away are not counted.
- **Size and room.** It holds that job's size. Where, giving way, pressure and idle timeout are as for `warm`: it starts only in room no waiting job wants, and when the job comes or the expectation lapses after 1 minute, it is reclaimed like a normal runner. A runner already up through `warm` is counted.
- The start reason shows `ahead: e2e expected in ~6s, when unit ends (1 of 1 expected), …`. How many are expected now is `classes[].soon` in `/api/state`. To turn it off, `ahead: false`.

**Measured.** The same harness with `needs:` added (the two e2e branches after unit-tests, size-report after label-check, deploy-web after deploy-api, 1.5–4.5 seconds after the end), with runner boot time drawn from the distribution of native runners measured on real hosts (median about 4.8 seconds, one in five 5.7–12 seconds, one in thirty-three 12–36 seconds). Pairs are learned from the previous 3 days of flow. Mean of 10 seeds:

| Push interval | Setup | Wait median / p90 / max | `needs` jobs median / p90 | Memory held by runners without a job | Runners stopped without a job |
|---|---|---|---|---|---|
| 35–55 s | Fixed per class | 0:01 / 0:02 / 1:21 | 0:01 / 0:03 | - | - |
| | pumasi, both off | 0:07 / 0:11 / 0:36 | 0:07 / 0:11 | 3.9G | 0 |
| | `ahead` only | 0:06 / 0:09 / 0:35 | 0:02 / 0:09 | 5.8G | 0.9 |
| | `warm` only | 0:01 / 0:07 / 0:25 | 0:01 / 0:07 | 48.6G | 6.2 |
| | Both (default) | 0:01 / 0:06 / 0:19 | 0:01 / 0:05 | 49.2G | 7.1 |
| 25–40 s | Fixed per class | 0:01 / 0:08 / 2:18 | 0:01 / 0:25 | - | - |
| | pumasi, both off | 0:07 / 0:21 / 1:00 | 0:08 / 0:35 | 4.5G | 0 |
| | `ahead` only | 0:06 / 0:20 / 1:00 | 0:05 / 0:34 | 6.2G | 0.9 |
| | `warm` only | 0:03 / 0:18 / 1:07 | 0:06 / 0:32 | 37.4G | 12.7 |
| | Both (default) | 0:02 / 0:19 / 0:57 | 0:04 / 0:32 | 38.5G | 12.8 |

98–100% of the runners started ahead got that job. `ahead` alone cuts the `needs` jobs' median from 0:07 to 0:02, and holds an eighth of the memory `warm` holds. With `warm` on, what it adds is small (p90 0:07 → 0:05, max 0:25 → 0:19). The once-a-day deploy-web, which `warm` cannot cover, waits 8.0 seconds on average, and 3.2–3.7 seconds when started ahead.
The `needs` jobs' p90 does not drop as far as with fixed runners, for two reasons: when the earlier job ends sooner than its median, the runner is late, and the tail of boot time (12–36 seconds) cannot be known ahead. Starting earlier (3–18 seconds more, taking the end at the 10th percentile of run time) moved p90 only within 1–2 seconds and only held more room, so the default was left as it is. In the busiest flow, p90 is time spent waiting for room, which this does not cut.

### When the jobs change: against fixed runners tuned once

The fixed per-class runners above were tuned knowing the flow in advance, so they are hard to beat on that same flow. But CI changes. So the fixed runners were tuned to the flow "before the change", and pumasi, with the runs before the change already measured, ran the flow after the change (`race.DriftTable`).
The changes are four that CI often goes through, fixed before the runs: every job 30% bigger (time, CPU, memory), heavy jobs (webkit e2e, image build) on every push, a new 5–7G job in pr-checks, and PRs halved with a nightly matrix (8 e2e + 2 builds) twice.
The comparison also includes "per-class, retuned" (retuned knowing the changed flow in advance): the case where someone noticed and fixed it.

Pushes every 35–55 seconds, mean of 10 seeds. Wait median / p90 / max, and the number of jobs that went over the memory their runner got (per run):

| Change | Per-class, tuned before the change | Per-class, retuned | pumasi |
|---|---|---|---|
| None | 0:01 / 0:02 / 1:42 | (same) | 0:01 / 0:09 / 0:21 |
| Jobs 30% bigger | 0:01 / 0:25 / 3:44, 44 jobs over, host memory exceeded for 83 s | 0:19 / 1:51 / 5:16 | 0:09 / 2:00 / 6:00 |
| Heavy jobs every time | 0:01 / 3:26 / 20:30 | 0:01 / 1:03 / 5:46 | 0:09 / 1:13 / 2:23 |
| New 5–7G job | 0:02 / 0:21 / 4:03, 18 jobs over | 0:03 / 0:29 / 2:25 | 0:08 / 0:30 / 1:11 |
| Nightly | 0:01 / 2:53 / 8:33 | 0:59 / 2:57 / 5:17 | 0:18 / 2:41 / 3:48 |

- Fixed runners tuned once do not get slower when jobs change. Instead something else breaks quietly: bigger jobs and new jobs go over their runner's memory (if that runner has a memory limit the job dies, otherwise the host overflows), and the line of a class that comes more often than the slots it was sized for grows to 20 minutes.
- pumasi did not go over memory on any host even though nobody changed a number (bigger jobs went over their estimate the first few times, but placement counts current use).
  Its max wait was shorter than per-class tuned before the change in 6 of the 8 cases (two speeds × four changes); the other two were jobs 30% bigger.
  Among all five setups it was the shortest in 4 cases (new job, nightly), and in the other 4 split was the shortest; in the case of bigger jobs, split got there with 35 jobs per run going over their runner's memory.
- Where it loses: the median. The changed flow has more work, so less room is left, and with nowhere to start runners ahead, boot time (8 seconds) shows again. Per-class tuned once keeps a 0:01 median while going over memory. pumasi's median is shorter than per-class retuned.
- With jobs 30% bigger, pumasi's max (6:00) is longer than per-class retuned (5:16). The setups that keep to memory are similar to each other; only those that go over memory are faster.
- With pushes every 25–40 seconds the shape is the same. The full table is the last table printed by `go run ./internal/race/cmd`.

## How hosts join

**The host opens the connection to the controller.** That is the only path. The controller opens one `listen` port, and the agent opens no ports.

- **One connection.** The host connects over TLS and opens several streams on it (yamux). The controller calls the agent's HTTP contract (`/v1/snapshot`, `/v1/runners`) over those streams.
- **When it drops, the host dials again**, backing off from 1 second to 30 seconds. Keepalive every 10 seconds; after 20 seconds with no answer, both sides drop the connection. While the controller is down, runners and jobs keep running.
- What the **invite line** does: fetches the binary for its OS from the controller into `~/.local/bin/pumasi`, runs the checks before joining, and brings the agent up as a service of that user.
  The host states only its name, budget and kind (defaults: hostname, no cap = whatever is free, desktop for Mac, Windows and Linux with a graphical session).
- **`hosts: - ssh: X`** has the controller do the same: it logs in over ssh and runs the invite line with that host's name, budget and kind (the script goes on stdin).
  ssh is used only then, and when `down` cleans up that host while it is not connected (running `pumasi leave`). If that host's agent keeps failing to connect, the invite line is run again every minute.
- **Keeping versions in step.** A host states its version (the binary's sha256) when it joins. If it differs, the host fetches the controller's version, checks the sha, and replaces itself and runs that. Runners are not touched.
  If it can't replace itself, it says why and joins with the old version, and an `agent version` incident opens.

### How each side trusts the other

- The controller makes a TLS key and certificate on first start (`join.key`, `join.crt` in the state directory).
- **Host → controller**: the invite line carries the certificate's public key fingerprint (`--pinnedpubkey`). The agent also connects only with that fingerprint. The host runs the images it is told to, so this check is needed.
- **Controller → host**: the invite token. A new one is made for each `pumasi invite`, by default for one host and one hour. It goes in the environment (`PUMASI_TOKEN`), not an argument, so it doesn't show in the process list.
  When joining, the host makes its own id and secret and sends them, and the controller stores only the hash (`hosts.json`). After that, connections use the id and secret.
- **A host that already joined can join again without a token** (to change its name, budget or kind). The token's one-time limit applies only to new hosts.
- Names in the config's `hosts:` are reserved. A host can join under such a name only with the token the controller gave that host.
- The `listen` port accepts only joining, binary downloads and sessions. The dashboard and status listen on loopback only.
- **Keeping credentials apart from jobs**: the GitHub token and `join.key` are only on the controller machine, and hosts get only one runner's JIT config. This separation breaks the moment a job can run as the controller's user:
  when the host is the controller's own machine and the runner is a process of that user (or a process of a user who can use docker, a privileged container, or a container that mounts the state directory).
  The agent puts a hash of the boot id and the user id in its snapshot, and when they match the controller's own, the controller warns with the class and the reason. Why it does not block, and the key that turns the warning off: the README's "Security model".

### What differs by OS

| | Linux | Mac | Windows |
|---|---|---|---|
| Invite line | sh | sh (the same line) | PowerShell |
| Checks before joining | docker, cgroup v2 | fetching the runner release, disk, login session | same |
| What keeps the agent running | systemd user unit (with linger) | LaunchAgent `dev.pumasi.agent` | HKCU `Run` value: a supervisor process started at logon |
| When that is not possible | a process that restarts itself (does not come back after a reboot; it says so when joining) | same (a Mac where no one is logged in to the screen) | — |
| Version update | exec with the same pid | exec with the same pid | swaps the file and exits → the supervisor starts the new version |

The agent on Mac and Windows runs in **the logged-in user's session** because of jobs: signing uses that user's keychain, and browser tests use the screen. That is why on Windows it is not a service (session 0).
A Windows host joined from an ssh session loses its agent when that session ends. Join from a window on that machine.

### Removing a host

- `pumasi leave` on the host: removes the agent, runners (containers, native runners' scopes, the image for inner docker), `~/.cache/pumasi`, the runner root, the binary and the service, and tells the controller.
  If runners are up, it lists them and refuses (`--force`).
- `pumasi down`: has the connected hosts do the same and deletes the scale sets. For a host that is not connected: if it is in `hosts:`, `leave` is run over ssh; if it joined on its own, pumasi says "run `pumasi leave` there".
- Runners of a host the controller has forgotten are closed in the ledger with `host … is not configured`.

## Runners

A runner ends after one job. Its work directory and tool cache belong to that runner too.

| | docker (Linux default) | native — Linux | native — Mac | native — Windows |
|---|---|---|---|---|
| Runner | container | a process in a systemd user scope | process group + tracking of descendants | a named Job Object |
| Limits (host protection, the class's `limit`) | cgroup: memory (CPU too if `limit` is set) | same (`MemoryMax`, `CPUQuota`, swap 0) | **none** | Job Object: allocations past it fail |
| Over the limit | only that process dies, and an OOM incident | same | — | counts the refusals, the same incident |
| Over the estimate | nothing happens: the next run is placed bigger | same | same | same |
| Metrics | cpu seconds, memory peak, I/O, pressure (PSI) | same (io only on hosts with delegation) | cpu seconds, memory peak (5-second samples) | cpu seconds, memory peak, I/O |
| Cleaning up leftover children | container removal | `cgroup.kill` | SIGTERM → SIGKILL to the group, children that escaped it are tracked too | Job end |
| If the agent dies | the runner keeps running; a new agent adopts it and the metrics continue | same (the scope belongs to systemd) | same | same (within the same logon session) |
| The host's files | not visible (only what `mounts` adds) | all visible | all visible | all visible |

- **The agent knows first whether a runner got a job.** It reads the `Running job` line in the runner's output and reports busy (GitHub's message comes about 30 seconds later).
- **Runner environment** = the agent's environment (native; names that look like credentials are dropped) or the image's environment (container) + the class's `env`. Native runners get `path` in front of `PATH`. `env` wins.
  `{runner}` in a value is that runner's directory (native: `<runner root>/<name>`, container: `/home/runner`). Other variable references (`$PATH`) are not expanded, which is why `path` exists.
  The agent's environment is what the service manager gives it: Mac's LaunchAgent only the `PATH` at join time, Linux's systemd user unit the user manager's defaults.
- **The JIT config is passed in an environment variable** (arguments show in `ps`). Anything in the agent's environment that looks like a credential (`*TOKEN*`, `*SECRET*`, `PUMASI_*`) is not passed to runners.
- **The runner version** is the tag of `image` (`ghcr.io/actions/actions-runner:2.338.2`). Containers use that image. Native runners read the version number from the tag and fetch the release of the same version
  once (`~/.cache/pumasi/dist`, unpacked only after its sha256 matches the digest of the GitHub release). **If the tag is `:latest`, native runners cannot start.** The runner's self-update is off, so people bump the version.

### Giving a container runner parts of the host

- **`mounts`**: attached at the same path with `docker run --mount type=bind` (`:ro` = read-only; docker does not create a missing path as root).
  The host answers which of the asked paths it has, and the class is placed only on hosts that have them all. Otherwise `no host has /srv/ci-cache`. Making them writable for the user inside the container (`runner`, uid 1001) is the host's job.
  Written on a native class, nothing is attached (it is already visible); it only picks the hosts to place on.
- **The work directory goes inside the first writable mount** (`<mount>/.pumasi-work/<runner>`, removed at the end). Between the container's overlay and a mount, and between different mounts,
  `link()` fails with `EXDEV`, so the hard links of pnpm, uv and ccache quietly turn into copies. If every mount is `:ro`, it goes inside the container.
- **`docker: inside`**: starts a docker daemon inside the runner container. The job's `docker run`, `services:` and compose land there, so they are inside the class limit and the metrics,
  and the containers, volumes, networks and images the job leaves go away with the runner. There are two costs. The runner container is **privileged**: don't use it for a class that takes unfamiliar repos and relies on container isolation.
  And **images are pulled again for every runner** (the inner store is a volume removed with the runner). Keeping the store between runners would save about 10 seconds per job, but one job's things would be visible to the next, and disk cleanup would need its own rules, so it is not kept.
  The default runner image has no iptables, so each host builds an image with it added, once (`pumasi-inner-docker:<version>`). The job installs the compose plugin. Not allowed on native classes.

### Native runners

- **Linux**: `run.sh` inside `systemd-run --user --scope --unit=pumasi-<runner>`. systemd-run puts itself in the scope and then becomes the runner's shell, so the runner is under the limits before it starts anything.
  A host that can't start them says so itself (no systemd user session, cpu/memory not delegated, linger off, libraries the runner reads missing; it asks `ldconfig -p`). No native class is placed on that host, and the wait reason shows what it said.
  Containers a job starts with the host's docker are outside this cgroup.
- **The runner root is outside the home directory**: Linux `/var/tmp/pumasi-<uid>`, Mac `/Users/Shared/pumasi-<uid>`, Windows `C:\pm\<last part of the SID>`. Job logs don't show the host user's home path,
  and on Windows the path stays short (the 260-character limit). If that place already belongs to someone else, `~/.cache/pumasi/runners` is used.
- **The release is cloned**: APFS clone on Mac, read-only hard links on Linux and Windows (a copy on a different filesystem). Runners share the files, so write permission is removed,
  and if it has come back to writable, it is taken as tampered with and fetched again.
- **What can't be removed is moved aside and reported.** Directories where the job removed write permission get it back and are removed. What that user can't remove, like root's files a job left through a container,
  is moved with the whole runner directory to `<runner root>/.trash/`, and a `leftover` incident states the path and the reason. Removing it is left to people.
- **Mac's free room**: CPU used by others from Mach `host_statistics`, memory from `kern.memorystatus_level`. When the kernel's memory pressure is warning or above, the host closes itself.
- **Windows**: the Job gets a name and the runner's shell inherits the Job handle, so if the agent dies the Job lives on and the new agent reopens it by name. The name is scoped to the logon session, so an agent in another session can't find it.
  `shell: bash` resolves to WSL's bash on a machine with WSL installed, and `shell: powershell` is blocked by the `Restricted` execution policy (`shell: powershell -NoProfile -ExecutionPolicy Bypass -File {0}`). This is how Windows runners behave, not pumasi.
- **Reboots are detected** by the host's boot id (Linux boot_id, Mac `kern.bootsessionuuid`, Windows kernel start time). **Turning Windows off and on with fast startup is not a new boot**: runners that ended then show as `died`, not `rebooted`.

### `{slot}`: places that stay

In a class's `env` and `path` values, `{runner}` is a directory removed with the runner, and `{slot}` is a number. It gives ephemeral runners the **persistent directories** fixed runners kept per runner, such as a home or a gradle home,
as "places only one runner uses at a time" (`HOME: /srv/ci-home/{slot}`).

- The **agent** gives the number: the smallest number not used by the runners of the same class the agent knows on that host. It is a matter within the host, so it is right even if the controller doesn't know,
  and it is stored in the state file with the runner record, so a running runner keeps its number when the agent or the controller restarts.
- A number is released when its runner is **removed**. A runner that finished but isn't removed yet still holds its number: processes it left may still be using that place.
- Native runners create the path up to the element with the number (`/srv/ci-home/3`) if it is missing. What is below it (`.gradle`) is up to the job. Why people aren't asked to create them in advance: how many runners start at once is decided by the host's free room,
  so having people match the number of directories on each host would be one more number that goes wrong. It is the same user creating an empty directory, so no new permission opens. If it can't be created, the runner is not started and the reason is given. It is never removed.
- On container runners only the `env` value changes (mounts are fixed per host).

### `overflow`: taking only what fits

The `maxCapacity` in the request that asks for messages is the cap on jobs assigned to that scale set (running jobs count). By default it is sent as large as possible, so the class takes every job of its labels: that is what makes waits visible and lets jobs be put in line.
A class with `overflow: true` sends its real room instead.

- **Number taken** = jobs held (taken but not yet picked up by a runner + runners running jobs) + how many more can be placed.
  "How many more can be placed" is how many of the largest estimate among the class's last 20 jobs fit in the room left after the placement plan (`Room` in `decide`). If jobs of this class are waiting for room, that many are subtracted.
  Why count with the largest: take one too many, and that job waits here when another runner could have taken it. Take too few, and another runner just takes it.
  A class with no measured jobs counts its next job as an "unknown job" (one per host at the first estimate, then more when 3× is free).
- **When the number changes, it asks again.** GitHub holds a request for about 50 seconds, so to pass on room that freed up or ran out meanwhile, it withdraws the request and asks again with the new number (`measuredPoll`). Messages stay on GitHub until deleted, so a withdrawn request loses nothing.
- **`after`** only pushes the number to 0: while the earlier class has room to place more, the later class sends only the number it holds. There is no way to tell GitHub an order between scale sets, so the order comes from one side not taking jobs.
- **What it does not do.** Send back jobs it took (there is no way to), or hand jobs over at once when the controller is off (while there is no session, GitHub holds a job for 5 minutes and then gives it to another runner, as measured in verified).

### What changes when moving off fixed runners

- Jobs that relied on what was left in the workspace (`clean: false`, `node_modules`) start from scratch every time.
- `setup-node` and `setup-java` download again for every runner.
- The runner name differs every time. Workflows that split cache directories by runner name should split them by class or job name.
- The idle runner count from GitHub's API is always 0.
- Order: start with one label set = one `driver: native` + `hosts:` class (write the old labels in `labels` and workflows don't change) → lift `hosts:` first on classes that pass nothing between jobs → move jobs that don't use the host to container classes.

## People first

A desktop is someone's desk. While that person is sitting there, it takes no new runners. Running jobs are not touched.

- The agent reports only "how many seconds ago was the last keyboard or mouse input". Mac `HIDIdleTime`, Windows `GetLastInputInfo` (readable only inside the user session),
  Linux the `IdleHint` of the logind graphical session. If it is unknown it is left empty, and a host whose signal is unknown is open.
- When a `desktop` host's last input is within **15 minutes**, the host is closed (`in use` in STATE and in the wait reason). Servers are not closed. There is no setting.
- While a person is there, the Mac agent moves runner processes to the background band (`PRIO_DARWIN_BG`). Mac has no way to limit CPU, so this is the only means. It is not done on Linux or Windows.
- CPU and memory used by others are taken out of free room whether a person is there or not.

## Robustness

Three principles.

1. **State can be rebuilt.** The truth is the runners on the hosts (the agents see them) and GitHub. The ledger is restored from those two. The controller may die at any time.
2. **Running jobs are not touched.** The only exceptions are what the user asked for explicitly (`down --force`, `leave --force`, a class's `maxDuration`).
   Every path that removes a runner that is up (idle timeout, boot timeout, a runner not in the ledger, Ctrl-C) **first asks GitHub to delete that runner's registration.** GitHub refuses to delete a runner holding a job,
   so a runner that just got a job and still looks idle is not removed. If refused, the runner is brought back as busy; if GitHub can't be asked, it is not removed that round.
3. **It does not fail silently.** What it fixed on its own and what it couldn't are both recorded as incidents and reported.

An **incident** is a row in the ledger, and the same `key` means the same incident (`host-unreachable/server1`). It is reported when it opens, every 30 minutes, and when it closes. More than three opens and closes within 5 minutes are grouped into one flapping incident.
If sending a notice fails, placement doesn't stop, and the notice is sent again the next round.

| Failure | Detection | What it does | Notice |
|---|---|---|---|
| Controller dies | service manager | restarts and takes the lock → waits for hosts → adopts runners. Missed messages are filled in from snapshots and GitHub job status | `controller is back: was down …, adopted N runners, N jobs finished meanwhile` |
| Agent dies | session lost | the host's service brings it back. Runners stay; metrics are on disk and continue | incident if not connected for over 2 minutes |
| Host connection lost | keepalive or snapshot failure | stops new placement, its runners still count in capacity, reconciled from the snapshot when it is back | incident past `hostLostAfter` (2 minutes) (with the reason it dropped), cleared when back |
| Host reboot | the boot id changed | closes its runners as lost and deletes their registrations. Jobs that were running are infrastructure failures | incident + list of lost jobs |
| Runner fails to start | `provisionTimeout` (3 minutes), early exit | asks to delete the registration, then removes it and places again | quarantine after 3 in a row on the same host |
| Host quarantine | 3 boot failures in a row | stops new placement, checks every 5 minutes, back when it passes | on quarantine and on return |
| Runner dies mid-job | exit code ≠ 0 with no job completion | deletes the registration (retrying until GitHub lets go). Infrastructure failure | incident + job, host, exit code |
| Memory limit | cgroup `oom_kill`, Job Object refusal | not rerun. That job's next estimate goes above the limit | `hit the memory limit that protects the host` or `hit class <class>'s memory limit` |
| Job waits long | first seen + `waitAlert` (10 minutes) | keeps re-evaluating | incident + reason, cleared when it starts |
| Job runs long | the class's `maxDuration` | stops the runner, only if set | incident the moment it passes |
| Low disk | free space on the fullest filesystem runners live on < `diskMinFree` (10%), see "Where disk is measured" below | stops new placement, cleans up finished pumasi containers and unused runner images | incident, cleared on recovery |
| Files that can't be removed | when removing a runner | moves the runner directory to `.trash/` | `leftover` incident |
| GitHub API failure | call error | retries with backoff. Stops on 401/403 | incident after 2 minutes, at once for 401/403 |
| Ledger corrupt | on open, integrity check | moves it aside and starts with an empty ledger, restored from snapshots and GitHub | incident (history is lost) |
| Orphan runner | a pumasi runner not in the ledger | removes only those whose registration deletion was accepted | log |
| Two controllers | state directory lock | the second exits at once | one error line |

**Reruns**: if the cause of a failure is on our side (host reboot, runner killed from outside), with `rerun: 1` the run's failed jobs are rerun once (default 0 = report only).
Code failures, memory limits and runs past `maxDuration` are not rerun.

**Notices**: `notify.webhook` (a JSON POST; for a Slack or Teams incoming webhook, in that format) or `notify.command` (the same JSON on stdin). `level`: `problem` (default) · `recovered` (also what it fixed on its own) · `all`.

**The controller's absence**: the controller cannot report its own death. The hosts know: their connection does not come back. So the hosts report it.

- **Only two.** If every host said the same thing in the same minute it would be noise, and hosts don't know each other (they all dial only the controller), so they can't agree on who already spoke.
  So the controller picks in advance which hosts speak: two of the connected hosts (servers first; a host once picked stays picked while it is connected), the first after `away`, the second after twice that.
  With only one, nothing would be said when that host goes down with the controller. The second one's message is not a repeat but a confirmation from another place.
- **The webhook URL goes only to those two** (`PUT /v1/away`, on every connection and when the picks change). The other hosts get an empty value. `notify.command` is not sent: there is no reason to run a command of the controller machine on a host.
- **Once each.** The host keeps "spoke" and "since when" in a file (`away.json`). If the agent restarts meanwhile it doesn't speak again, and the all-clear on return goes where it spoke (even if the picks changed meanwhile).
  An absence that was never delivered has no all-clear either.
- Time is measured as time the agent was awake. A laptop host waking from sleep is not an absence.

The rows of the failure table are triggered on real hosts by `make chaos` (`scripts/chaos.sh`).

## Operation

- **State directory** `~/.local/state/pumasi/`: the ledger (`pumasi.db`), the event log, the TLS key, joined hosts, invite tokens. One controller per directory.
- **Dashboard**: one page the controller serves on loopback (three static files built into the binary). It reads `/api/state`, `/api/jobs` and `/api/series` every 5 seconds. Anyone can read; writes (expedite, mode, runtime rules) are taken only from loopback, or, if it is opened to the outside, only from requests carrying `dashboardToken` (guide.md, "Dashboard"). `/metrics` is for Prometheus.
- **Host columns of status**: `SEEN` (last answer), `RTT` (snapshot round trip), `AGENT` (the agent's version), `LAST DROP` (why it last dropped: `keepalive timeout`, `connection reset`, `agent exited`).
- **`diskMinFree`**, default 10%. A machine whose disk is always full (a laptop) used as a host hits this value and gets no runners; the incident says so. It can be lowered in the config.
- **Where disk is measured.** The agent measures every filesystem runners live on: docker's data root (docker driver), the agent's directory, the native runner root (when the host can run native runners), and, for each runner there now (started or adopted), its work directory (the class's `work`; for container runners, the one recorded in its label) and its `{slot}` directories (`{slot}` in `env` and `path`, e.g. a per-slot `HOME`). A directory that doesn't exist yet is measured at the parent it would be created in.
  The same filesystem (device number; volume on Windows) is counted once, and **the one with the lowest free ratio** is reported (on a tie, the one with fewer bytes free). That path goes along as `diskPath`, so close reasons and incidents say where, like `disk 2% free on /nvme/ci/work/1`.
  Why: in a real case, the agent directory was on a large HDD (81% free) and the runners and work directories on an NVMe that was 98% full (6.5G), but the agent measured only the HDD and kept placing.
  Other paths set only through `env` (a `HOME` without a slot number, and so on) can't be known and are not measured. While there are no runners, the work directory is unknown; that disk is included from the samples after the first runner starts. Native runners started by an agent of an earlier version have no directory in `pumasi.json`, so they are left out until they end.

### Keeping the controller running

`pumasi service install` hands `up` to the service manager. There is no new mechanism: `up` adopts everything when it comes back after dying, so the only instruction needed is "restart it when it dies".

- **As a user service** (systemd user unit, LaunchAgent). No root is needed, and everything the controller uses (the state directory, ssh keys) belongs to that user.
  The costs differ by OS: on Linux with linger off, it stops at logout and doesn't start at boot (`install` and `status` say so). Mac's LaunchAgent runs only while that user is logged in to the screen.
- **The token is one file** (`<state directory>/github-token`, 600). Units and plists sit where anyone can read them, so it is not written there. `up` reads this file when the environment variable is absent, and after reading removes the variable from its own environment (so ssh and notify commands don't inherit it).
  The file is created as 600 from the start. A file in a mode others can read is not used: a hand-made file left like that is the most common accident.
- **`Restart=always` / `KeepAlive`**: `up` exits with 0 on a signal, so with "only on failure", a controller someone stopped by mistake would not come back. The only way to stop it is `service remove`.
- Mac's `ProcessType` is `Interactive`. Without it, launchd may push CPU and IO back on a busy machine.
- A dead controller's message session is released on GitHub's side after 30–90 seconds. Only the dead process knows the session id, and the library has no way to close someone else's session, so the restarted controller waits, asking again every 15 seconds.

## What it does not do

- A relay through NAT. Hosts must reach the `listen` address.
- Hosts choosing classes or images.
- Buttons on the web to join or remove hosts, or a per-class cap on runners.
- Stopping running jobs to make room (an expedited job also waits for running jobs to end), or placing while ignoring the estimated size (the only exception is a size no host can lend: it is placed alone on an empty host).
- Moving jobs to another host, or stopping running jobs because a person came back.
- Keeping tool caches or docker image stores between runners.
- VM drivers (Tart, microVMs), CI systems other than GitHub, or creating cloud instances.
