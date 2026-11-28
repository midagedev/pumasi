# pumasi landscape survey (2026-11-24)

Goal: find out whether a controller already exists that places ephemeral GitHub Actions self-hosted runners on a mixed fleet (always-on Linux servers, Linux/Windows desktops that people use during the day, Apple Silicon Macs) and picks the machine by the host's free CPU/memory, the job's size and human activity (HTCondor-style yield).

Method: each repo below was fetched into `.research/` with `git clone --depth 1`, and its README/docs/code were read directly. Stars and last push are the values from `gh api repos/<owner>/<repo>` (as of 2026-11-24). Items that could not be read directly are marked "unconfirmed".

## 1. Comparison table

Legend: O = confirmed, X = absent (not found in code/docs), △ = partial/conditional, ? = unconfirmed.

| Project | multi-host | OS (Linux/macOS/Windows) | capacity-aware placement | job-size aware | yields to human activity | per-job ephemeral isolation | warm cache | official scale set API | License | Activity (stars / last push) |
|---|---|---|---|---|---|---|---|---|---|---|
| cloudbase/garm | △ one controller calls several providers. A provider can point at a remote host, but there is no notion of "choosing a host" | O / X / O (OSType is only `linux`·`windows`·`unknown`) | X | △ manual split by the pool/scale set's flavor and label | X | O (VM/container per provider) | △ keeps warm runners with min-idle; image cache is up to the provider | O (own implementation `util/github/scalesets`) + webhook pools alongside | Apache-2.0 | ~500 / 2026-11-23 |
| ysya/runscaler | X (one process per host, "one process per host") | O / O(Tart) / X | △ `internal/capacity` is a fixed-slot semaphore, `diskguard` looks only at disk. Does not look at CPU or memory load | X | X | O | O Tart VM warm pool, Docker cache receiver | O (`actions/scaleset` v0.5.0) | SUL 1.0 (n8n family, not OSI. Internal business and non-commercial use, redistribution restricted) | ~13 / 2026-11-19 |
| ephpm/ephemerd | X (all it does is a Windows host dispatching over gRPC to its own WSL/Hyper-V VMs) | O / O / O | X slot semaphore (`pkg/scheduler/slots.go`) | X | X | O (containerd, Hyper-V containers, macOS APFS clone VMs) | △ uses OCI images as an artifact cache | X (go-github polling/webhooks) | MIT | ~16 / 2026-11-11 |
| shapehq/tartelet | X (single Mac app) | X / O / X | X | X | X | O (Tart VM) | X | X | MIT | ~816 / 2026-02-10 |
| khoi/sand | X | X / O / X | X | X | X | O (Tart VM clone) | △ caches the runner tarball on the host | X (JIT/PAT registration) | MIT | ~69 / 2026-11-02 |
| efrecon/gh-runner-krunvm | X | O / O / X (krunvm microVM) | X | X | X | O | X | X | MIT | ~30 / 2025-01-24 (effectively stopped) |
| hostinger/fireactions | X (one server + Firecracker, by pool) | O / X / X (needs KVM) | X (keeps a pool minimum) | △ VM resources defined per pool | X | O (Firecracker microVM) | △ images | X (webhook + JIT) | Apache-2.0 | ~208 / 2026-11-17 |
| cirruslabs/orchard (+ Tart) | O (worker cluster) | X / O / X | △ compares the sum of CPU/mem the VMs declare against the worker's total (`ResourcesUsed` in `internal/controller/scheduler/workerinfo.go`). Not measured load | △ cpu/memory per VM | X | O | O Tart images | X (no GitHub Actions integration in the repo. Whether a separate commercial product exists is unconfirmed) | FSL-1.1-ALv2 (becomes Apache-2.0 after 2 years) | ~624 / 2026-11-20 |
| Veertu Anka Build Cloud (commercial) | O | X / O (mixed Intel and Apple Silicon) / X | △ a node's capacity mode is by VM count or by free CPU and RAM | △ | ? | O | ? | ? (the docs describe two GitHub Actions integrations, Controller+Registry and Controller-less. Whether either is a scale set is unconfirmed) | commercial | not public |
| eyupio/zoomies | O (1 controller + N agents, agents connect outbound) | O / △ / △ (runners are Linux containers only. On a Mac, Docker Desktop's Linux containers; on Windows, a WSL2 distro) | O ranks candidate hosts by "CPU and memory free after placement" (item 4 in `docs/architecture.md`), using usage measured in agent heartbeats | △ size set per pool (label) | X (no keyboard/user activity concept found in the docs) | O | △ registry mirror, a document on persistent cache | X (`workflow_job` webhook + JIT, polling fallback) | AGPL-3.0 | ~96 / 2026-11-24 (new, very active) |
| actions/actions-runner-controller | O (k8s nodes) | O / X / △ | △ the k8s scheduler places by pod requests | △ pod spec per scale set | X | O | △ | O (reference implementation) | Apache-2.0 | ~6.7k / 2026-11-23 |
| actions/scaleset (Go client) | n/a (library) | n/a | n/a | messages include `RequestLabels` | n/a | ephemeral by default | n/a | it is the API | MIT | ~222 / 2026-11-12 (Public Preview) |
| TuroYT/pitlane | X (one Docker host) | O / X / X | X | X | X | O + long-lived DinD slots for cache | O | O | MIT | ~0 / 2026-11-23 |
| Codemany (commercial app) | △ an agent per machine | O / O / X | X (fixed runner count per machine) | X | ? | O | ? | ? | commercial | not public |
| HTCondor (scheduler, for reference) | O | O / O / O | O (LoadAvg, slot resources) | O (ClassAd request_cpus/memory) | O (KeyboardIdle/ConsoleIdle) | △ (container universe) | X | n/a | Apache-2.0 | mature |

Sources:
- garm: `.research/garm/doc/{providers,external_provider,scale-sets,pools-and-scaling}.md`, `params/params.go:494`, `vendor/github.com/cloudbase/garm-provider-common/params/params.go:31-34`, `workers/scaleset/scaleset_listener.go:196-256`, `workers/scaleset/scaleset.go:1282-1285`. https://github.com/cloudbase/garm
- runscaler: `.research/runscaler/README.md`, `LICENSE`, `go.mod`, `internal/capacity/broker.go`, `internal/diskguard/`. https://github.com/ysya/runscaler
- ephemerd: `.research/ephemerd/README.md`, `pkg/scheduler/slots.go`, `pkg/scheduler/dispatch.go`. https://github.com/ephpm/ephemerd
- tartelet: https://github.com/shapehq/tartelet (README/metadata only, code not read). sand: `.research/sand/README.md`. https://github.com/khoi/sand
- fireactions: `.research/fireactions/README.md`. https://github.com/hostinger/fireactions
- krunvm runner: https://github.com/efrecon/gh-runner-krunvm (metadata only, no commits since 2025-01)
- orchard: `.research/orchard/internal/controller/scheduler/{scheduler.go,workerinfo.go}`, `LICENSE`. https://github.com/cirruslabs/orchard
- Anka: https://docs.veertu.com/anka/anka-build-cloud/ (only confirmed that the text mentions capacity mode and two GitHub Actions integrations)
- zoomies: `.research/zoomies/README.md`, `docs/architecture.md` (lines 104-135), `docs/hosts-and-pools.md`, `LICENSE`. https://github.com/eyupio/zoomies
- scaleset: `.research/scaleset/README.md`, `types.go` (`JobMessageBase.RequestLabels`). https://github.com/actions/scaleset
- awesome-runners matrix: `.research/awesome-runners/README.md` lines 36-78. https://github.com/jonico/awesome-runners
- HTCondor: https://htcondor.readthedocs.io/en/23.0/_sources/admin-manual/policy-configuration.rst.txt, https://research.cs.wisc.edu/htcondor/uwcs/policy.html (part of this was confirmed from search summaries, not from reading the pages directly)

## 2. Details by project

### 2.1 GARM

- Provider model: an external executable is called through the `GARM_COMMAND` environment variable. There are 7 operations, `CreateInstance/DeleteInstance/GetInstance/ListInstances/RemoveAllInstances/Stop/Start`, and bootstrap information comes as JSON on stdin (`BootstrapInstance`) (`doc/external_provider.md`). The JSON contains `flavor`, `image`, `labels`, `extra_specs`, `os_type`, `arch`, `pool_id`, so a "size class" can be passed to the provider.
- Limit 1, there is no place to choose a host: one pool/scale set is tied to one `provider_name` (`workers/scaleset/controller_watcher.go:84` looks up `c.providers[scaleSet.ProviderName]`). So "which host" has to be decided inside the provider executable. GARM neither receives nor passes on host load signals.
- Limit 2, a provider call is a stateless process exec. To track host load and human activity, the provider ends up as a thin shim talking to a separate daemon, and GARM's value (state, policy) stays outside the shim.
- Limit 3, macOS is not in the model: the shared type `OSType` is `windows`, `linux`, `unknown`. The bootstrap `tools` array does have an `osx` entry (`doc/external_provider.md`), so a workaround looks possible, but the pool's `--os-type` is documented as `linux|windows` (`doc/pools-and-scaling.md:53`).
- Limit 4, yield (moving a running job out of the way) cannot be expressed in the provider interface. `Stop/Start` work on instances, and there is no notion of cancelling or requeueing a job.
- scale set: supported. GARM does not use the `actions/scaleset` package; it implements its own client (`util/github/scalesets`). The listener receives `JobAvailable`, calls `AcquireJobs` and updates the desired count from `TotalAssignedJobs` (`workers/scaleset/scaleset_listener.go:196-256`). The target runner count is `min(MinIdle + Desired, MaxRunners)` (`scaleset.go:1285`).
- Pool meaning: `--min-idle-runners` (default 0) is the number of warm runners, `--max-runners` (default 5) the upper bound (`doc/pools-and-scaling.md:56-57`). With scale sets, GitHub does the scheduling (table in `doc/scale-sets.md`) and the name is the `runs-on` label. Labels cannot be changed after creation.
- The stack is heavy: a DB, REST API, auth, web UI, support for many forges/providers. The extension point is "add a provider", not "replace the scheduler".
- Activity: very active (push on 2026-11-23), about 500 stars, Apache-2.0.

### 2.2 runscaler

- The closest "official scale set API + Docker/Tart (macOS)" implementation. Uses `actions/scaleset` v0.5.0, a Tart VM warm pool (about 2 seconds vs 30 seconds), a Docker cache receiver.
- One process per host, with no coordination across hosts. `internal/capacity` is a fixed-slot semaphore of the form `NewBroker(limit)` and does not read measured CPU/mem. `diskguard` looks only at free disk.
- The license is SUL 1.0 (in the n8n community license family). Past releases were MIT, but the current HEAD is not OSI and is limited to "internal business and non-commercial purposes". Extending it by taking or forking the code is unsuitable as the base of an open-source project. It is used as a design reference only.
- It already has the slot concept, so it is the exact prototype of the problem "11 slots take jobs regardless of host load".

### 2.3 ephemerd

- The only project that covers Linux (built-in containerd) / Windows (Hyper-V isolated containers + a Linux VM booted with HCS) / macOS (Virtualization.framework Linux VM + APFS clone macOS VM) in a single binary. Valuable as prior art for "what is practical" on Windows and macOS.
- Works only within one host (the gRPC in `dispatch.go` goes from the host to its own VMs). Slots are the buffered channels `sem/linuxSem/macSem` (`slots.go`). No load awareness.
- Polling/webhooks based on go-github, not a scale set (no `actions/scaleset` in `go.mod`). New (about 16 stars).

### 2.4 Tartelet / sand / krunvm / Fireactions

- All are single-host tools that "keep N ephemeral VMs on one host". None has host choice, load awareness or human yield.
- Tartelet is the best known (about 816 stars) but has had no push since 2026-02. sand is Tart-based and active. The krunvm runner has effectively stopped. Fireactions is Firecracker (KVM) only, so no Mac/Windows.

### 2.5 Orchard / Anka

- Orchard is a scheduler that spreads Tart VMs across an Apple Silicon cluster. The scheduling loop (`schedulingLoopIteration` in `scheduler.go`) looks at whether a worker is offline or paused, arch/runtime compatibility and endpoint capability, and when a VM specifies no CPU/memory it fills in the worker defaults (4 CPU / 8192 MB by default) (`scheduler.go:362-383`). A worker's usage is the sum of the declared resources of the VMs assigned to it (`workerinfo.go`). So it is bin-packing by declared amounts and does not look at measured load (CPU a person is using). There is no GitHub integration in this repo; starting a runner inside the VM is up to the user. macOS only.
- Anka Build Cloud is commercial. Its docs describe a node capacity mode (by VM count or free CPU and RAM) and two GitHub Actions integrations (Controller+Registry, Controller-less). The internal scheduler code is not public, so whether it considers load or yields to people could not be confirmed.

### 2.5.1 zoomies (the closest competitor)

- 1 controller + N agents. Placement is done by the pure function `internal/scheduler.Decide`: among hosts that are healthy, not cordoned, match the backend, platform and host selector the pool asks for, and have room left, it ranks by "CPU/memory free after placement" (`docs/architecture.md` lines 113-126). Usage measured in agent heartbeats and pressure holds feed into the snapshot. It is, as the name says, "capacity-aware multi-host placement".
- Limits: (a) runners are Linux containers only. A Mac is treated as Docker Desktop's Linux containers, a Windows desktop as a WSL2 distro (`docs/hosts-and-pools.md` line 168 on). Native macOS/Windows jobs (Xcode, Windows-only .NET, etc.) cannot run. (b) No human activity yield concept found in the docs (searching for keyboard/idle/human/workstation turned up only unrelated contexts). (c) Webhook + JIT, not the scale set API (no `actions/scaleset` in `go.mod`, go-github v88). (d) AGPL-3.0, so its code cannot be absorbed into an MIT/Apache project. (e) New (about 96 stars), with a wide scope (UI, backup, AI context, MCP, etc.), so it is hard to take apart and use as a "thin controller".

### 2.6 Scan of the awesome-runners matrix

In the matrix (lines 36-78), the only entries related to multi-host, mixed machines or idle desktops are these.
- `eyupio/zoomies`: "per-pool limits across existing hosts", Linux amd64/arm64.
- `Flinner/gh-runnerctl`: several hosts over ssh, fixed-size pool (no autoscale), 2 stars.
- `Codemany`: single-use runners on users' Macs and Linux, commercial.
- `cisco-open/forge`: an ARC + AWS wrapper. Cloud-centered.
- `i2Dev-com/homefield`: Lima VMs, one Mac.
No entry stated "yield on machines people use" or "placement by measured load".

### 2.7 HTCondor and Nomad

- HTCondor's desktop policy combines, as ClassAd expressions, `START` (start condition), `SUSPEND` (pause), `CONTINUE` (resume), `PREEMPT` (evict), `KeyboardIdle`/`ConsoleIdle` (input idle time), and `LoadAvg` with `NonCondorLoadAvg = LoadAvg - CondorLoadAvg` (separating out only the human side's load). Example from the Wisconsin CS pool: start when 1-minute load is under 0.3 and the console has been idle 15 minutes; preempt 10 minutes after suspending. There is also a `POLICY : Desktop` template. Sources: https://htcondor.readthedocs.io/en/23.0/_sources/admin-manual/policy-configuration.rst.txt, https://research.cs.wisc.edu/htcondor/uwcs/policy.html. This model maps directly to pumasi's yield policy. The exact default expressions were not read directly, though, so they need to be checked again against the current docs before implementation.
- No public case of "running GitHub runners as HTCondor jobs" was found by search (an `--ephemeral`/JIT runner exits after running one job, so it could be wrapped as a condor job, but no case was confirmed). The upside is a mature policy; the downsides are the cost of running a central manager/negotiator/ClassAd, and having to build the glue with the scale set's "create JIT at JobAvailable" model yourself.
- No Nomad-based GitHub runner autoscaler was found by search. The search results point the other way: "a driver for Nomad has to be built directly on top of `actions/scaleset`".

### 2.8 Ephemeral options for Windows desktops (practicality)

- Windows containers (process isolation): the Windows host version and the container image version must match. Preparing Windows container images on desktop Windows 11 is a chore (general knowledge, no separate source confirmed in this survey).
- Hyper-V isolated containers: ephemerd implemented Hyper-V isolated containers combined with a Linux VM booted with HCS (Windows section of `.research/ephemerd/README.md`). It states as a strength that it can run as a `LocalSystem` service on a desktop people use, and this approach is the best-proven path.
- Hyper-V VM clone / checkpoint restore: technically possible, but this search found no public per-job restore case. Image size and boot time are a burden (a commercial service's docs report Windows runner boot taking over 1 minute: https://runs-on.com/runners/windows/).
- Windows Sandbox: limited to 1 instance, unsuited to automation/running as a service, and no CI case was confirmed. Not recommended.
- Realistic minimum: cover Windows desktops first with WSL2/Docker-based Linux runners (the zoomies way), and extend to Hyper-V isolated containers or VMs only where native Windows jobs are really needed.

## 3. Verdict

### 3.1 Is the gap real: partly

What already exists:
- Multi-host + placement by host headroom: zoomies (Linux containers only, AGPL, webhook model), Orchard (Mac only, by declared resources).
- Docker/Tart autoscaler on the scale set API: runscaler (single host, non-OSI license).
- Single-host ephemeral isolation on all 3 OSes: ephemerd.

What does not (not one found in this survey):
1. Yield to human activity (keyboard/console idle, separating non-runner load, a policy for running jobs).
2. Placement that reflects measured host load (including what people use), with the result fed back as the capacity reported to the scale set.
3. One controller handling native macOS/Windows hosts and Linux servers together. zoomies is Linux containers, Orchard is Mac only, ephemerd is an independent daemon per host.
4. Reading job size classes (2-core lint vs 8-core e2e + its own compose stack) from labels and using them as placement weights. Tools exist that declare size by hand per pool/VM, but there is no label -> resource request -> host-with-room pipeline. This item overlaps most with the zoomies pool model, so it is not asserted that it "does not exist".

So "capacity-aware multi-host" itself is not new (zoomies), and the differences are three: (1) human yield, (2) measured load + native Mac/Windows, (3) size classes as input. Human yield is the strongest gap, and it is the work of carrying over to GitHub Actions the policy model HTCondor solved 20 years ago.

### 3.2 Extend something vs write new

| Option | Upsides | Decisive downside |
|---|---|---|
| Add a placement provider to GARM | Apache-2.0, active, scale set support, Windows support, operations features (DB/REST/UI) already there | A pool/scale set is fixed to 1 provider (`controller_watcher.go:84`), so host choice must happen inside a provider shim. Provider calls are stateless execs, so load/activity state needs a separate daemon anyway. No macOS in `OSType`. Yield (evicting a running job) is not in the interface |
| Fork of/contribute to zoomies | Multi-host, headroom placement and agent structure already there | AGPL-3.0, Linux containers only, webhook model, a new project with wide scope, so large structural impact |
| Fork runscaler | Clean scaleset + Docker/Tart implementation | SUL 1.0 non-OSI license. Code cannot be reused |
| Extend ephemerd | Experience isolating on all 3 OSes | Assumes a single host, polling/webhooks (not a scale set), new, slot semaphore structure |
| Write new on `actions/scaleset` | MIT, no license constraints. Messages carry `RequestLabels`, so the job's size is known before placement (`JobMessageBase` in `scaleset/types.go`). The controller reports "max capacity" when polling (`scaleset/README.md` item 2), so measured headroom can be fed back as capacity. A native yield policy can be implemented directly in the host agent | The amount to build yourself (agent, providers (Docker/Tart/Hyper-V), state storage). `actions/scaleset` is Public Preview |

### 3.3 Conclusion: write new on scale sets, alongside GARM as an adapter rather than competing

The recommendation is to build a new "thin controller + host agent" on `actions/scaleset` (MIT). Reasons:

1. The needed inputs already come from the scale set side. `JobAvailable` has `RequestLabels`/`RepositoryName`, so the size class is known, and the listener can check candidate hosts' headroom before confirming "I take this job" with `AcquireJobs` (GARM follows the same flow, `scaleset_listener.go:196-256`). In the mode where runners are created JIT after the job arrives (min-idle 0), the placement decision comes after the job's information is known, so the cause of the existing problem ("GitHub hands a job to any idle slot") is removed by structure. The cost is cold-start delay, eased by a few warm runners per size class.
2. Report measured capacity instead of a slot count. runscaler and ephemerd are fixed-slot semaphores. Replacing the max capacity the controller reports on each poll with `a value computed dynamically from host headroom` directly solves the 11-slot problem. This value dropping to 0 with human activity is where yield starts.
3. Extending GARM cannot cover the core gaps (yield, macOS), for the reasons in the table above. Since GARM has many users, though, separately offering an adapter that exposes the same agent as a GARM external provider shim is left open for later (the provider interface is simple, stdin JSON + environment variables, so the cost is small).
4. Code from zoomies and runscaler cannot be reused (AGPL / SUL). They are design references only. In particular, zoomies' "pure function `Decide(snapshot) -> Plan` + reason string" is well worth borrowing as a testable scheduler structure (borrowing the idea, not copying code).
5. For Windows/macOS isolation, ephemerd's (MIT) implementation can serve as reference material (Hyper-V isolated containers, APFS clone VMs). When quoting code, keep the MIT notice obligation.

### 3.4 Risks and open questions

- `actions/scaleset` is Public Preview, so its interface may change. This may be why GARM implemented its own client; this point needs further checking.
- With a scale set, GitHub assigns jobs to idle runners, so "yield by capacity 0" only stops new jobs. A policy that interrupts jobs already running (the equivalent of HTCondor's SUSPEND/PREEMPT) needs careful design, because GitHub has no job-retry meaning (if the runner disappears, the job fails). A practical default is "block new jobs from starting + let running jobs finish, but lower their priority with nice/cgroup".
- Collecting human activity signals per OS (macOS `ioreg HIDIdleTime`, Windows `GetLastInputInfo`, Linux X11/logind idle) was not verified in code in this survey.
- Size classes must already be split by label (the `runs-on` name), so a migration is needed on the workflow side to separate scale set names into `lint`/`e2e` and so on.
- Unconfirmed: the internal scheduling of Anka and Codemany, how Orchard integrates with GitHub Actions, commercial products' human activity policies.
