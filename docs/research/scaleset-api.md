# actions/scaleset API survey

Date: 2026-11-24. Subjects: `github.com/actions/scaleset` (HEAD `716908e`, 2026-11-07), `actions/actions-runner-controller` (HEAD `b27dac7`, 2026-11-19, `cmd/ghalistener`). Local clones: `.research/scaleset`, `.research/arc` (gitignored).

Notation: `S:` = the scaleset repo, `A:` = the ARC repo, `#N` = an actions/scaleset issue. **[inferred]** = a judgment with no direct basis in the code, drawn from the code + issues. **[server behavior]** = cannot be confirmed from client code; behavior on the GitHub service side.

## Summary

| Question | Answer |
| --- | --- |
| Does job information arrive before a runner is assigned | It does, on every path confirmed today. `JobAvailable` and `JobAssigned` both carry the repo, workflow ref, job name, run id and `requestLabels` (`S:types.go:47-62`). |
| Is per-job placement possible | Where to start a runner can be decided from job metadata. But there is no guarantee that runner gets that job. It is a heuristic with no guarantee. |
| Can a specific job be sent to a specific runner | No. A JIT config is tied to a scale set, not to a job (`S:client.go:780`). The only lever is splitting scale sets. |
| Scale set labels | Several are possible (github.com). They cannot be changed after creation, though (#140). |
| Can one process handle several scale sets | Yes. One `Client`, with one `MessageSessionClient` + `Listener` per scale set. |
| Runner list API | None. Only `GetRunner`, `GetRunnerByName`, `RemoveRunner`. |

---

## 1. End-to-end flow

### 1.1 Authentication

| Item | Content | Source |
| --- | --- | --- |
| 3 methods | GitHub App (PEM key), GitHub App + custom `JWTProvider` (KMS/HSM), PAT | `S:client.go:115-268`, `S:README.md:149-151` |
| Required for an App | `ClientID`, `InstallationID`, `PrivateKey`, all three required | `S:client.go:117-141` |
| Both PAT and App given | Error | `S:client.go:155-166` |
| Scope | Parsed from the `GitHubConfigURL` path. 1 segment = org, `enterprises/<x>` = enterprise, 2 segments = repo | `S:config.go:32-73` |
| Hosting | `github.com`, `www.github.com`, `github.localhost`, `*.ghe.com` use `api.<host>`; anything else is GHES (`/api/v3`). Can be forced with env `GITHUB_ACTIONS_FORCE_GHES` | `S:config.go:75-109` |

Token exchange in 3 steps:

1. Get a PAT or an App installation token (`POST /app/installations/{id}/access_tokens`) (`fetchAccessToken` in `S:client.go`, around line 964).
2. Get a registration token with `POST /{orgs|enterprises|repos}/.../actions/runners/registration-token` (`createRegistrationTokenPath`, `S:client.go:1078-1097`).
3. Send `Authorization: RemoteAuth <regtoken>` to `POST /actions/runner-registration` and get the Actions service URL and an admin JWT (`S:client.go:1008-1045`). Later calls go under `_apis/runtime/runnerscalesets` and `_apis/distributedtask/pools/0/agents` with `api-version=6.0-preview` (`S:client.go:23-24,394-395`). The admin token is renewed automatically before it expires (`S:client.go:1122`).

The list of required permissions is not in the code; it links to the GitHub docs (link in the authentication section of `S:README.md`, https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api).

### 1.2 Creating and deleting a scale set

- `CreateRunnerScaleSet`: `POST .../runnerscalesets` (`S:client.go:585-620`). The name must be unique within the runner group (`S:client.go:584`). If `Labels` is empty, `Name` becomes the only label (`S:client.go:571-583`).
- A runner group ID is needed. The example uses `1` for `default`, and otherwise looks it up with `GetRunnerGroupByName` (`S:examples/dockerscaleset/main.go:67-75`).
- The example uses `RunnerSetting{DisableUpdate: true}` (`S:examples/dockerscaleset/main.go:~82`). Runner auto-update is off, so we have to manage the runner version **[inferred]**.
- `DeleteRunnerScaleSet` expects 204 (`S:client.go:657`). The example deletes the scale set on exit. Issue #78 points out there is no UI for cleaning up scale sets left behind by an abnormal exit.

### 1.3 Message session and listener loop

- `Client.MessageSessionClient(ctx, scaleSetID, owner)` creates a session with `POST .../{id}/sessions` (`S:client.go:750-778`, `S:session_client.go:44-72`). The response carries `MessageQueueURL`, `MessageQueueAccessToken` and the initial `Statistics` (`S:types.go:124-131`).
- `listener.Listener.Run` (`S:listener/listener.go:126-182`):
  1. Passes a synthetic message (`MessageID = -1`) built from the initial session statistics to `Scale` (lines 138-143).
  2. Loop: `GetMessage(lastMessageID, maxCapacity)` then `Scale(msg)`, and on success `DeleteMessage` (ack) (lines 160-180).
  3. If `Scale` returns an error, it exits without ack. The message is redelivered, so `Scale` must be idempotent (`S:listener/listener.go:114-120`).
  4. `Scale` is not called concurrently (comment at the same place). The caller only has to implement the one `Scaler` interface (lines 121-123).
- `GetMessage` is a long poll. The server waits about 50 seconds; the client limit is `MessagePollTimeout` = 2 minutes (`S:README.md:91-103`, `S:http_client.go:73`). On 202 it returns `(nil, nil)` (`S:session_client.go:171-172`). `maxCapacity` is sent in the `X-ScaleSetMaxCapacity` header (`S:client.go:39`, `S:session_client.go:163`). It can be changed while running with `listener.SetMaxRunners` (`S:listener/listener.go:72`).
- When the session token expires, it is renewed with `PATCH .../sessions/{id}` and retried once (`S:session_client.go:81-113,115-141`).

### 1.4 Desired runner count

- The server includes `Statistics` in every message. `TotalAssignedJobs` is the scaling signal. It includes both waiting and running jobs (`S:README.md:38-46`, `S:types.go:133-141`).
- The README says explicitly not to count individual job messages, because each response is cut off at 50 messages at most (`S:README.md:44-47`).
- The official formula is `min(maxRunners, minRunners + TotalAssignedJobs)` (`A:cmd/ghalistener/scaler/scaler.go:339`, `S:examples/dockerscaleset/scaler.go:70`).
- "Reporting" is not a separate API. The `maxCapacity` header is the report, and the server appears not to assign jobs beyond that value **[inferred]** (observation in #133: with `maxCapacity=0`, `TotalAssignedJobs` is 0 even when jobs are queued).

### 1.5 Starting a runner (JIT)

- `GenerateJitRunnerConfig(ctx, {Name, WorkFolder}, scaleSetID)` calls `POST .../{id}/generatejitconfig` and returns `{Runner{ID,Name}, EncodedJITConfig}` (`S:client.go:780-813`, `S:types.go:93-96,158-161`). This call creates the runner registration on the server. If the same name exists, it fails with `RunnerExistsError` (`S:errors.go:305-306`).
- All the runner side needs is the official `actions/runner` binary, run with `run.sh` and env `ACTIONS_RUNNER_INPUT_JITCONFIG=<EncodedJITConfig>` (`S:examples/dockerscaleset/scaler.go:138-149`, `A:controllers/actions.github.com/constants.go:13`). On Windows it would presumably be `run.cmd`, but there is no basis for it in this repo **[server behavior/runner side, unconfirmed]**.
- JIT runners are ephemeral. They run one job and the registration disappears (`S:README.md:22`).

### 1.6 Removing a runner

- Normal: after the job ends, the runner leaves by itself and `JobCompleted` arrives. The example deletes the container then (`S:examples/dockerscaleset/scaler.go:113-122`). There is deliberately no scale-down logic. The design is that `JobCompleted` handles removal (lines 94-98 of the same file).
- Forced: `RemoveRunner(runnerID)` sends `DELETE .../agents/{id}` and expects 204 (`S:client.go:881-905`). If a job is running, it returns `JobStillRunningError` (`S:errors.go:309-310`). ARC treats `RunnerNotFound` as success and `JobStillRunning` as retry (`A:controllers/actions.github.com/runner_unregistration.go:311-325`).

---

## 2. Message types and fields

There are 4 types: `JobAvailable`, `JobAssigned`, `JobStarted`, `JobCompleted` (`S:types.go:14-19`). There is no separate statistics message. `Statistics` is a field of `RunnerScaleSetMessage` (`S:types.go:105-112`). The top-level `messageType` must be `RunnerScaleSetJobMessages`, the body comes as an array inside a JSON string, and unknown types are silently ignored (`S:client.go:677-748`).

Common fields, `JobMessageBase` (`S:types.go:47-62`): `runnerRequestId`, `repositoryName`, `ownerName`, `jobId`, `jobWorkflowRef`, `jobDisplayName`, `workflowRunId`, `eventName`, `requestLabels[]`, `queueTime`, `scaleSetAssignTime`, `runnerAssignTime`, `finishTime`.

| Type | Beyond the common fields | Job information before runner assignment |
| --- | --- | --- |
| `JobAvailable` | `acquireJobUrl` | Yes. On this message you must call `AcquireJobs([runnerRequestId])`. Otherwise the job stays unassigned (`S:listener/listener.go:109-112`). |
| `JobAssigned` | none | Yes. No runner information. |
| `JobStarted` | `runnerId`, `runnerName` | After a runner is chosen. Find our runner by `runnerName` and mark it busy. |
| `JobCompleted` | `result`, `runnerId`, `runnerName` | `result` holds values such as `canceled`. |

**Conclusion: the job information placement needs (repo, workflow, job name, `requestLabels`) is already in messages that come before runner assignment.** So a per-job input signal exists. With these caveats:

1. Depending on the server mode, `JobAvailable` may not come. In an experiment with github.com repository scope, `JobAssigned` came directly without `JobAvailable`, with `runnerRequestId` 0 (#107, no maintainer answer). On GHES, acquire was reported to be required (#75, restored by #90). Both paths must be handled.
2. When `JobAssigned` arrives, the job has already been assigned to the scale set. With `maxCapacity > 0`, the server assigns on its own (#133). Only the `JobAvailable` mode lets us choose "take it or not", and in that mode the recovery contract for a job we did not acquire is not documented (#107 comments).
3. With `maxCapacity=0`, jobs waiting in the queue are not visible (#133, unresolved). There is no official way to observe "waiting demand" while capacity is full.
4. Each message is cut off at 50 at most (`S:README.md:46`). You cannot assume you see every job message. The statistics are always exact.
5. Requests for added fields (`repositoryId`, `jobWorkflowPath`, etc.) are open in #114. For now the `jobWorkflowRef` string has to be parsed (`A:cmd/ghalistener/metrics/workflow_ref_parser.go`).

---

## 3. Labels and matching

- Several labels can be given with `RunnerScaleSet.Labels []Label` (`S:types.go:86`). It is `Label{Type, Name}`, and an empty `Type` becomes `System` (`S:client.go:563-569`). Without `Labels`, `Name` is the only label (`S:client.go:571-583`).
- The README says "the scale set name is the `runs-on` label"; several labels are supported on github.com; on GHES, 3.18+ needs the feature flag `DistributedTask.AllowRunnerScaleSetCustomLabels` turned on, and when it is off, extra labels are silently dropped (`S:README.md:15,189-203`).
- The example CLI takes several labels with `--labels` (`S:examples/dockerscaleset/config.go:130-141`).
- Matching rule: the README only says matching is "by labels and runner group policy, the same as regular self-hosted runners" (`S:README.md:17`). The regular self-hosted meaning (every label in `runs-on` is a subset of the runner's labels) seems to apply as is, but this cannot be confirmed from code **[server behavior, needs measuring]**. The reproduction code in #140 assumed that `runs-on: [self-hosted, new-label]` targets a multi-label scale set.
- **Labels should be treated as unchangeable after creation.** `UpdateRunnerScaleSet` returns 200 but the labels do not change (#140, open). To change labels, delete and recreate.
- **One process can handle several scale sets.** `Client` is not tied to a scale set (`GenerateJitRunnerConfig` and `MessageSessionClient` both take `scaleSetID` as an argument, `S:client.go:750,780`). A `Listener` is tied to one `Config.ScaleSetID` (`S:listener/listener.go:17-19`). So create one `MessageSessionClient` + `Listener` per scale set and run them as goroutines. The HTTP client is designed for concurrent use (HTTP Transport section of `S:README.md`, "SDK calls `Do` concurrently"). ARC, by contrast, uses one listener process per scale set (`A:cmd/ghalistener/main.go:100`).
- A session has an `ownerName` (the example uses the hostname) (`S:examples/dockerscaleset/main.go`, `S:types.go:126`). What it means to open 2 sessions on the same scale set is not in the code, and #107 only has observations across different scale sets. **[unconfirmed]**

---

## 4. Assignment semantics

- Runners belong to a scale set. A job is assigned to a scale set, and any idle runner in that scale set takes it (`S:README.md:20` "GitHub assigns a pending job to any idle runner in the scale set"). The only arguments to `GenerateJitRunnerConfig` are a name and a work folder; there is no way to name a job (`S:types.go:93-96`).
- Issue #107, which asks for pinning (stickiness) a job to a specific agent, is open with no maintainer answer. How jobs are distributed among several scale sets with the same labels is not documented either (#107).
- So **the only supported lever is splitting scale sets.** The axes to split on:
  - OS/architecture class (told apart by `runs-on` label)
  - size class (told apart by `runs-on` label)
  - scope (repo, org, enterprise) and runner group
- Design within one scale set **[inferred]**:
  - Do not start runners ahead (min=0); start JIT runners only as far as `TotalAssignedJobs` rises. Picking the host from a waiting job's metadata then makes "a runner started for this job". But another job in the same scale set can take it first. Jobs in the same scale set are the same size class, so host-fit errors happen only within the class.
  - When a job starts, `JobStarted.runnerName` tells the actual match. Putting the host in the runner name allows accounting and verification afterwards.
  - "One scale set per job" is the PoC approach of issue #107, but distribution among scale sets with the same labels is arbitrary, so it is not guaranteed.
- If a runner does not take the job in time after assignment: the job's assignment is cancelled (`result: "canceled"` in `JobCompleted`) and requeued, repeating up to 3 times with growing delay (`S:README.md:114-118`).

---

## 5. Failure semantics

| Situation | Basis in code/docs | Notes |
| --- | --- | --- |
| Runner disappears before receiving a job (host down while idle) | The client has no way to know. The registration stays on the server and shows in the statistics' `TotalRegisteredRunners`, `TotalIdleRunners` (`S:types.go:138-140`). | We must detect it ourselves by heartbeat and delete the registration with `RemoveRunner(id)`. ARC's controller also calls `RemoveRunner` when a pod fails (`A:controllers/.../ephemeralrunner_controller.go:535-560`, `runner_unregistration.go`). |
| Host down while a job runs (desktop sleep) | No handling in the scaleset code. ARC deletes the EphemeralRunner when the pod of a runner with a job fails and queues removal of its registration (`A:ephemeralrunner_controller.go:535-558`). `RemoveRunner` can return `JobStillRunningError`, so it retries. | The job's own fate (when the server fails the job after losing contact with the runner) is server behavior. Not confirmed in this repo **[server behavior]**. For requeueing, only the retry at the job assignment stage (up to 3 times) is documented. |
| Runner provisioning fails | There is no API to tell the server about the failure. Relies on a server timeout (#124, open). | |
| Listener restart | Un-acked messages are redelivered (`S:README.md:103-113`). Runners started before the restart vanish from local state. The example panics on an unknown runner name (#150, open). ARC ignores "not found" as normal. | A recovery loop that reconciles a local state DB with the server statistics is needed. |
| Duplicate name | `RunnerExistsError`. ARC calls `GetRunnerByName`, deletes the existing registration and recreates (`A:ephemeralrunner_controller.go:860-880`). | |
| Dead HTTP/2 connection | A bug that stalled for up to about 25 minutes was fixed (#131 closed, `S:http_client_health_test.go`, commit 88a9274). | |

**Full list of lookup and cleanup APIs**: `GetRunner(id)`, `GetRunnerByName(name)`, `RemoveRunner(id)`, `GetRunnerScaleSet`, `GetRunnerScaleSetByID`, `ListRunnerScaleSets(runnerGroupID)` (`S:client.go:431-905`). There is no runner list API. Feature request #89 for runner state reconciliation was closed with no implementation. Reconciling can only be done by comparing `Statistics` (totals) with a local ledger. That compares only runner counts; which runner disappeared has to be checked by calling `GetRunner(id)` per runner **[inferred]**.

---

## 6. Limits and stability

| Item | Content | Source |
| --- | --- | --- |
| Status | Public Preview. "The API is stable, but interfaces and examples may change" | `S:README.md:3` |
| License | MIT | `S:LICENSE`, GitHub API `license: MIT` |
| Go version | `go 1.27.3` (same for scaleset and ARC) | `S:go.mod:3`, `A:go.mod:3` |
| Dependencies | `docker/docker` and `cobra` are in the same `go.mod` (for the examples). Library consumers may see them as transitive dependencies. | `S:go.mod` |
| Repo age | Created 2025-10-29, releases v0.1.0 (2026-02-09) to v0.5.0 (2026-10-23) | GitHub API (`gh api repos/actions/scaleset`) |
| Activity | Recent commits: 2026-11-07 (#145), 11-06 (#142, #144), 10-29 (#143), several on 10-23. Last push 2026-11-12. 222 stars, 17 open issues (whether PRs are included is unconfirmed) | GitHub API |
| API version | Fixed at `api-version=6.0-preview` (private Actions service API) | `S:client.go:395`, `S:session_client.go:160` |
| Retries | By default: transport errors, 5xx (except 501) and 429, up to 4 times, exponential backoff (1 second to 30 seconds) | `S:README.md:259`, `S:http_client.go:254-256` |
| Rate limit | 429, and 403 with `Retry-After` or `X-RateLimit-*`, are classified as `*RateLimitError`. Policy can be changed with the `OnRateLimit` hook. The limit numbers themselves are not in the code | `S:README.md:257-323`, `S:errors.go` |
| Polling | Server waits about 50 seconds, client limit 2 minutes | `S:README.md:91-103`, `S:http_client.go:73` |

The calls with rate limit risk are `generatejitconfig` (once per runner) and the registration token exchange (REST, `api.github.com`). At a scale of a few dozen hosts, this is unlikely to be a problem **[inferred]**. The numbers were not measured.

Related open issues (those that matter for non-k8s use):

- #107 the job pinning/assignment contract and the `JobAvailable` vs `JobAssigned` paths (no answer)
- #133 queue demand cannot be observed when `maxCapacity=0`
- #124 no signal for provisioning failure
- #140 label updates have no effect
- #122 the long poll does not react to `JobStarted`
- #150 the example panics on messages for unknown runners
- #75 (closed) empty message queue on GHES, fixed by restoring acquire

---

## 7. Blockers and complications for a multi-host, multi-OS placement controller

1. **A job cannot be pinned to a runner.** Per-job placement ends at "where to start a runner", and GitHub decides which runner gets which job (§4). Splitting size classes and OS classes into scale sets is the only guaranteed lever. As the number of scale sets grows, listeners and sessions grow with it.
2. **Labels are immutable.** Changing labels needs the scale set recreated (#140). The size class scheme has to be settled first.
3. **Demand is not visible when `maxCapacity=0`** (#133). If a machine people use is busy and reports capacity 0, the waiting jobs of that scale set cannot be seen. Ways out: report the hosts' combined capacity but place at the moment of starting, and when starting is impossible rely on the path where the job's assignment is cancelled and requeued (up to 3 times); or keep a minimum capacity of 1. Both designs lean on server behavior, so they need measuring.
4. **Two paths, `JobAvailable`/`JobAssigned`.** On github.com, `JobAssigned` can come directly (`runnerRequestId=0`), while GHES needs acquire (§2). The `Scaler` has to work in either mode, and "refuse a job and hand it to another scale set" has no supported contract (#107).
5. **No runner list API.** Ghost registrations from a host going down, a desktop sleeping or an agent crashing must be cleaned up ourselves with a local ledger + `GetRunner(id)`/`RemoveRunner(id)`. Persistent state (e.g. SQLite) is needed so the ledger survives a controller restart. Example-level in-memory state is not enough (#150).
6. **Interrupting a running job on a machine people use.** When the server side fails the job is unconfirmed. For desktops, the realistic approach is to prevent sleep (a wake lock during jobs) and to block runner starts by detecting use. A job that has already started cannot be preempted **[inferred]**.
7. **Runner-side operations.** The JIT config has to reach the host safely (a sensitive value; the errors section of `S:README.md` also warns it "may contain a token or JIT configuration"), and a per-host agent has to start `run.sh`/`run.cmd`. With `DisableUpdate`, we have to manage the runner binary version. Windows and macOS support is `actions/runner`'s job; the scaleset repo has no per-OS code (it is a pure Go HTTP client, so it compiles anywhere).
8. **Preview risk.** It depends on a private service API (`6.0-preview`), and several contract questions are open with no maintainer response (#107, #133). On the other hand, commits are active and ARC itself is built on this library (`A:cmd/ghalistener/scaler/scaler.go:12-13`), so it is unlikely to be abandoned **[inferred]**.
9. **GHES support.** Irrelevant if the target is github.com, but GHES 3.18 to 3.20 has issues with the multi-label flag and the acquire path (§1 to 3, #75).

## To close by measuring

1. Whether `runs-on` matching is the subset rule, and whether a single-label job matches a multi-label scale set.
2. How jobs are distributed among several scale sets with the same labels.
3. How long after an idle runner's host dies `TotalRegisteredRunners`/`TotalIdleRunners` go down, and how a job ends (time, result) when its runner disappears mid-job.
4. Whether, with a small `maxCapacity`, a job assigned to a runner that does not start is actually cancelled and requeued, and with what delay.
5. Under which conditions the `JobAvailable` mode and the direct `JobAssigned` mode appear.
