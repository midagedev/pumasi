# actions/scaleset API 조사

조사일: 2026-11-24. 대상: `github.com/actions/scaleset` (HEAD `716908e`, 2026-11-07), `actions/actions-runner-controller` (HEAD `b27dac7`, 2026-11-19, `cmd/ghalistener`). 로컬 클론: `.research/scaleset`, `.research/arc` (gitignore 대상).

표기: `S:` = scaleset 레포, `A:` = ARC 레포, `#N` = actions/scaleset 이슈. **[추론]** = 코드에 직접 근거가 없고 코드 + 이슈에서 끌어낸 판단. **[서버 동작]** = 클라이언트 코드로는 확인 불가, GitHub 서비스 쪽 동작.

## 요약

| 질문 | 답 |
| --- | --- |
| 러너 배정 전에 job 정보가 오는가 | 오늘 확인된 모든 경로에서 온다. `JobAvailable`, `JobAssigned` 모두 repo, workflow ref, job 이름, run id, `requestLabels`를 담는다 (`S:types.go:47-62`). |
| job 단위 배치가 가능한가 | 러너를 어디에 띄울지는 job 메타데이터로 정할 수 있다. 그러나 그 러너가 그 job을 받는다는 보장은 없다. 보장 없는 휴리스틱 수준이다. |
| 특정 job을 특정 러너로 보낼 수 있나 | 못 한다. JIT config는 scale set에 묶이고 job에는 묶이지 않는다 (`S:client.go:780`). 유일한 레버는 scale set 분할이다. |
| scale set 라벨 | 여러 개 가능 (github.com). 다만 생성 후 변경은 안 된다 (#140). |
| 한 프로세스가 여러 scale set을 다루나 | 가능하다. `Client` 하나에 scale set마다 `MessageSessionClient` + `Listener` 하나씩. |
| 러너 목록 API | 없다. `GetRunner`, `GetRunnerByName`, `RemoveRunner`뿐이다. |

---

## 1. End-to-end 흐름

### 1.1 인증

| 항목 | 내용 | 출처 |
| --- | --- | --- |
| 방식 3종 | GitHub App(PEM 키), GitHub App + 커스텀 `JWTProvider`(KMS/HSM), PAT | `S:client.go:115-268`, `S:README.md:149-151` |
| App 필수값 | `ClientID`, `InstallationID`, `PrivateKey` 셋 다 필수 | `S:client.go:117-141` |
| PAT과 App 동시 지정 | 에러 | `S:client.go:155-166` |
| scope 결정 | `GitHubConfigURL` 경로 파싱. 1단 = org, `enterprises/<x>` = enterprise, 2단 = repo | `S:config.go:32-73` |
| 호스팅 판정 | `github.com`, `www.github.com`, `github.localhost`, `*.ghe.com`이면 `api.<host>`, 그 외는 GHES(`/api/v3`). env `GITHUB_ACTIONS_FORCE_GHES`로 강제 가능 | `S:config.go:75-109` |

토큰 교환 3단계:

1. PAT 또는 App installation token(`POST /app/installations/{id}/access_tokens`)을 얻는다 (`S:client.go` `fetchAccessToken`, 964행 부근).
2. `POST /{orgs|enterprises|repos}/.../actions/runners/registration-token`으로 등록 토큰을 얻는다 (`createRegistrationTokenPath`, `S:client.go:1078-1097`).
3. `POST /actions/runner-registration`에 `Authorization: RemoteAuth <regtoken>`을 보내 Actions 서비스 URL과 admin JWT를 받는다 (`S:client.go:1008-1045`). 이후 호출은 `_apis/runtime/runnerscalesets`와 `_apis/distributedtask/pools/0/agents` 아래에서 `api-version=6.0-preview`로 나간다 (`S:client.go:23-24,394-395`). admin 토큰은 만료 전에 자동 갱신한다 (`S:client.go:1122`).

필요한 권한 목록은 코드에 없고 GitHub 문서로 연결된다 (`S:README.md` 인증 절의 링크, https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api).

### 1.2 scale set 생성과 삭제

- `CreateRunnerScaleSet`: `POST .../runnerscalesets` (`S:client.go:585-620`). 이름은 runner group 안에서 유일해야 한다 (`S:client.go:584`). `Labels`가 비면 `Name`이 유일한 라벨이 된다 (`S:client.go:571-583`).
- runner group ID가 필요하다. 예제는 `default`면 `1`, 아니면 `GetRunnerGroupByName`으로 조회한다 (`S:examples/dockerscaleset/main.go:67-75`).
- `RunnerSetting{DisableUpdate: true}`를 예제가 쓴다 (`S:examples/dockerscaleset/main.go:~82`). 러너 자동 업데이트가 꺼지므로 러너 버전은 우리가 관리해야 한다 **[추론]**.
- `DeleteRunnerScaleSet`은 204를 기대한다 (`S:client.go:657`). 예제는 종료 시 scale set을 삭제한다. 비정상 종료로 남은 scale set의 정리는 이슈 #78에서 UI 부재가 지적됐다.

### 1.3 메시지 세션과 listener 루프

- `Client.MessageSessionClient(ctx, scaleSetID, owner)`가 `POST .../{id}/sessions`로 세션을 만든다 (`S:client.go:750-778`, `S:session_client.go:44-72`). 응답에 `MessageQueueURL`, `MessageQueueAccessToken`, 초기 `Statistics`가 담긴다 (`S:types.go:124-131`).
- `listener.Listener.Run` (`S:listener/listener.go:126-182`):
  1. 초기 세션 통계로 합성 메시지(`MessageID = -1`)를 `Scale`에 넘긴다 (138-143행).
  2. 루프: `GetMessage(lastMessageID, maxCapacity)` 후 `Scale(msg)`, 성공하면 `DeleteMessage`(ack) (160-180행).
  3. `Scale`이 에러를 내면 ack 없이 종료한다. 메시지는 재전달되므로 `Scale`은 멱등이어야 한다 (`S:listener/listener.go:114-120`).
  4. `Scale`은 동시 호출되지 않는다 (같은 위치 주석). 호출자는 `Scaler` 인터페이스 하나만 구현하면 된다 (121-123행).
- `GetMessage`는 long poll이다. 서버 대기 약 50초, 클라이언트 한도 `MessagePollTimeout = 2분` (`S:README.md:91-103`, `S:http_client.go:73`). 202면 `(nil, nil)`을 반환한다 (`S:session_client.go:171-172`). `maxCapacity`는 `X-ScaleSetMaxCapacity` 헤더로 전달된다 (`S:client.go:39`, `S:session_client.go:163`). `listener.SetMaxRunners`로 실행 중에 바꿀 수 있다 (`S:listener/listener.go:72`).
- 세션 토큰이 만료되면 `PATCH .../sessions/{id}`로 갱신하고 1회 재시도한다 (`S:session_client.go:81-113,115-141`).

### 1.4 desired runner count

- 서버가 모든 메시지에 `Statistics`를 실어 보낸다. `TotalAssignedJobs`가 스케일 신호다. 대기 중 job과 실행 중 job을 모두 포함한다 (`S:README.md:38-46`, `S:types.go:133-141`).
- README는 개별 job 메시지를 세지 말라고 명시한다. 응답당 메시지가 최대 50개로 잘리기 때문이다 (`S:README.md:44-47`).
- 공식 계산식은 `min(maxRunners, minRunners + TotalAssignedJobs)`다 (`A:cmd/ghalistener/scaler/scaler.go:339`, `S:examples/dockerscaleset/scaler.go:70`).
- "보고"는 별도 API가 아니다. `maxCapacity` 헤더가 보고이고, 서버는 그 값을 넘겨 job을 배정하지 않는 것으로 보인다 **[추론]** (#133의 관찰: `maxCapacity=0`이면 큐에 job이 있어도 `TotalAssignedJobs`가 0이다).

### 1.5 러너 시작 (JIT)

- `GenerateJitRunnerConfig(ctx, {Name, WorkFolder}, scaleSetID)`는 `POST .../{id}/generatejitconfig`를 호출하고 `{Runner{ID,Name}, EncodedJITConfig}`를 반환한다 (`S:client.go:780-813`, `S:types.go:93-96,158-161`). 이 호출이 러너 등록을 서버에 만든다. 같은 이름이 있으면 `RunnerExistsError`가 난다 (`S:errors.go:305-306`).
- 러너 쪽에 필요한 것은 공식 `actions/runner` 바이너리와 env `ACTIONS_RUNNER_INPUT_JITCONFIG=<EncodedJITConfig>`로 `run.sh`를 실행하는 것뿐이다 (`S:examples/dockerscaleset/scaler.go:138-149`, `A:controllers/actions.github.com/constants.go:13`). Windows는 `run.cmd`일 것이나 이 레포에는 근거가 없다 **[서버 동작/러너 쪽, 미확인]**.
- JIT 러너는 ephemeral이다. job 하나를 실행하고 등록이 사라진다 (`S:README.md:22`).

### 1.6 러너 제거

- 정상: job 종료 후 러너가 스스로 빠지고 `JobCompleted`가 온다. 예제는 그때 컨테이너를 지운다 (`S:examples/dockerscaleset/scaler.go:113-122`). 스케일 다운 로직은 일부러 없다. `JobCompleted`가 제거를 처리한다는 설계다 (같은 파일 94-98행).
- 강제: `RemoveRunner(runnerID)`는 `DELETE .../agents/{id}`로 204를 기대한다 (`S:client.go:881-905`). job 실행 중이면 `JobStillRunningError`를 반환한다 (`S:errors.go:309-310`). ARC는 `RunnerNotFound`를 성공으로, `JobStillRunning`을 재시도로 처리한다 (`A:controllers/actions.github.com/runner_unregistration.go:311-325`).

---

## 2. 메시지 타입과 필드

타입은 4종이다: `JobAvailable`, `JobAssigned`, `JobStarted`, `JobCompleted` (`S:types.go:14-19`). 별도의 statistics 메시지는 없다. `Statistics`는 `RunnerScaleSetMessage`의 필드다 (`S:types.go:105-112`). 최상위 `messageType`은 `RunnerScaleSetJobMessages`여야 하고, body는 JSON 문자열 안의 배열로 오며 알 수 없는 타입은 조용히 무시된다 (`S:client.go:677-748`).

공통 필드 `JobMessageBase` (`S:types.go:47-62`): `runnerRequestId`, `repositoryName`, `ownerName`, `jobId`, `jobWorkflowRef`, `jobDisplayName`, `workflowRunId`, `eventName`, `requestLabels[]`, `queueTime`, `scaleSetAssignTime`, `runnerAssignTime`, `finishTime`.

| 타입 | 공통 필드 외 추가 | 러너 배정 전 job 정보 |
| --- | --- | --- |
| `JobAvailable` | `acquireJobUrl` | 있음. 이 메시지를 받으면 `AcquireJobs([runnerRequestId])`를 호출해야 한다. 안 하면 job이 미배정으로 남는다 (`S:listener/listener.go:109-112`). |
| `JobAssigned` | 없음 | 있음. 러너 정보는 없다. |
| `JobStarted` | `runnerId`, `runnerName` | 러너 지정 후. `runnerName`으로 우리 쪽 러너를 찾아 busy 표시. |
| `JobCompleted` | `result`, `runnerId`, `runnerName` | `result`에 `canceled` 등이 들어간다. |

**결론: placement 결정에 필요한 job 정보(repo, workflow, job 이름, `requestLabels`)는 러너 배정 이전 메시지에 이미 있다.** 따라서 job 단위 입력 신호는 있다. 단 다음 주의가 있다.

1. 서버 모드에 따라 `JobAvailable`이 오지 않을 수 있다. github.com 저장소 scope 실험에서 `JobAvailable` 없이 `JobAssigned`가 직접 왔고 `runnerRequestId`가 0이었다 (#107, 메인테이너 답변 없음). GHES에서는 acquire가 필요했다는 보고가 있다 (#75, #90으로 복원). 두 경로를 모두 처리해야 한다.
2. `JobAssigned`가 오는 시점은 이미 scale set에 배정된 뒤다. `maxCapacity > 0`이면 서버가 알아서 배정한다 (#133). 우리가 "받을지 말지"를 고를 수 있는 것은 `JobAvailable` 모드뿐이고, 그 모드에서 acquire하지 않은 job의 복구 계약은 문서화돼 있지 않다 (#107 댓글).
3. `maxCapacity=0`이면 큐에 대기 중인 job이 보이지 않는다 (#133, 미해결). 용량이 가득 찬 상태에서 "대기 수요"를 관찰하는 공식 방법이 없다.
4. 메시지당 최대 50개로 잘린다 (`S:README.md:46`). job 메시지는 모두 볼 수 있다고 가정할 수 없다. 통계는 항상 정확하다.
5. 필드 추가 요청(`repositoryId`, `jobWorkflowPath` 등)은 #114에 열려 있다. 지금은 `jobWorkflowRef` 문자열을 파싱해야 한다 (`A:cmd/ghalistener/metrics/workflow_ref_parser.go`).

---

## 3. 라벨과 매칭

- `RunnerScaleSet.Labels []Label`로 여러 개를 지정할 수 있다 (`S:types.go:86`). `Label{Type, Name}`이고 `Type`이 비면 `System`이 들어간다 (`S:client.go:563-569`). `Labels`를 안 주면 `Name`이 유일한 라벨이다 (`S:client.go:571-583`).
- README는 "scale set 이름이 곧 `runs-on` 라벨"이라 하고, 여러 라벨은 github.com에서 지원, GHES는 3.18+에서 feature flag `DistributedTask.AllowRunnerScaleSetCustomLabels`가 켜져 있어야 하며 꺼져 있으면 추가 라벨이 조용히 버려진다고 한다 (`S:README.md:15,189-203`).
- 예제 CLI는 `--labels`로 다중 라벨을 받는다 (`S:examples/dockerscaleset/config.go:130-141`).
- 매칭 규칙: README는 "라벨과 runner group 정책으로, 일반 self-hosted 러너와 동일하게" 매칭된다고만 쓴다 (`S:README.md:17`). 일반 self-hosted의 의미(`runs-on`의 모든 라벨이 러너 라벨의 부분집합)가 그대로 적용된다고 보이나 코드로는 확인할 수 없다 **[서버 동작, 실측 필요]**. #140의 재현 코드에서 `runs-on: [self-hosted, new-label]`이 다중 라벨 scale set 대상이 된다는 전제가 쓰였다.
- **라벨은 생성 후 변경할 수 없다고 봐야 한다.** `UpdateRunnerScaleSet`이 200을 돌려주지만 라벨이 안 바뀐다 (#140, 열림). 라벨을 바꾸려면 삭제 후 재생성이 필요하다.
- **한 프로세스가 여러 scale set을 다룰 수 있다.** `Client`는 scale set 비종속이다 (`GenerateJitRunnerConfig`와 `MessageSessionClient` 모두 `scaleSetID`를 인자로 받는다, `S:client.go:750,780`). `Listener`는 `Config.ScaleSetID` 하나에 묶인다 (`S:listener/listener.go:17-19`). 따라서 scale set마다 `MessageSessionClient` + `Listener`를 하나씩 만들어 goroutine으로 돌리면 된다. HTTP 클라이언트는 동시 사용을 전제로 설계돼 있다 (`S:README.md` HTTP Transport 절, "SDK calls `Do` concurrently"). ARC는 반대로 scale set당 listener 프로세스 하나를 쓴다 (`A:cmd/ghalistener/main.go:100`).
- 세션은 `ownerName`(예제는 hostname)을 갖는다 (`S:examples/dockerscaleset/main.go`, `S:types.go:126`). 같은 scale set에 세션 2개를 열 때의 의미는 코드에 없고 #107에는 서로 다른 scale set 간 관찰만 있다. **[미확인]**

---

## 4. 배정 의미론

- 러너는 scale set에 속한다. job은 scale set에 배정되고, scale set 안의 아무 idle 러너가 받는다 (`S:README.md:20` "GitHub assigns a pending job to any idle runner in the scale set"). `GenerateJitRunnerConfig`의 인자는 이름과 작업 폴더뿐이고 job 지정 수단이 없다 (`S:types.go:93-96`).
- job과 특정 agent의 고정(stickiness)을 요구한 이슈 #107은 열려 있고 메인테이너 답이 없다. 같은 라벨의 scale set 여러 개에 job이 어떻게 분배되는지도 문서화돼 있지 않다 (#107).
- 그러므로 **지원되는 레버는 scale set 분할뿐이다.** 분할 축은 아래와 같다.
  - OS/아키텍처 클래스(`runs-on` 라벨로 구분)
  - 크기 클래스(`runs-on` 라벨로 구분)
  - scope(repo, org, enterprise)와 runner group
- 한 scale set 안에서의 설계 **[추론]**:
  - 러너를 미리 띄우지 않고(min=0) `TotalAssignedJobs`가 오른 만큼만 JIT로 띄운다. 이때 대기 중인 job 메타데이터로 호스트를 고르면 "이 job을 위해 띄운 러너"가 된다. 그러나 같은 scale set의 다른 job이 먼저 가져갈 수 있다. 같은 scale set 안의 job은 같은 크기 클래스이므로 호스트 적합성 오류는 클래스 안에서만 생긴다.
  - job이 시작되면 `JobStarted.runnerName`으로 실제 매칭을 알 수 있다. 러너 이름에 호스트를 넣어두면 사후 계정과 검증에 쓸 수 있다.
  - "job 하나당 scale set 하나"는 이슈 #107의 PoC 방식이나 같은 라벨 scale set 간 분배가 임의여서 보장이 안 된다.
- 배정 후 러너를 제때 못 받으면: job 배정이 취소(`JobCompleted`의 `result: "canceled"`)되고 재큐잉되며 최대 3회 지연이 늘며 반복된다 (`S:README.md:114-118`).

---

## 5. 실패 의미론

| 상황 | 코드/문서 근거 | 비고 |
| --- | --- | --- |
| 러너가 job 수신 전 사라짐(idle 중 호스트 다운) | 클라이언트가 알 방법이 없다. 서버에 등록이 남고 통계의 `TotalRegisteredRunners`, `TotalIdleRunners`에 반영된다 (`S:types.go:138-140`). | 우리가 직접 heartbeat로 감지하고 `RemoveRunner(id)`로 등록을 지워야 한다. ARC도 pod 실패 시 컨트롤러가 `RemoveRunner`를 호출한다 (`A:controllers/.../ephemeralrunner_controller.go:535-560`, `runner_unregistration.go`). |
| job 실행 중 호스트 다운(데스크톱 sleep) | scaleset 코드에 처리가 없다. ARC는 job이 있는 러너의 pod가 실패하면 EphemeralRunner를 삭제하고 등록 제거를 큐에 넣는다 (`A:ephemeralrunner_controller.go:535-558`). `RemoveRunner`는 `JobStillRunningError`를 돌려줄 수 있어 재시도한다. | job 자체의 운명(러너 통신 두절 후 서버가 job을 실패 처리하는 시점)은 서버 동작이다. 이 레포에서 확인되지 않는다 **[서버 동작]**. 재큐잉은 job 배정 단계의 재시도(최대 3회)만 문서화돼 있다. |
| 러너 프로비저닝 실패 | 서버에 실패를 알리는 API가 없다. 서버 타임아웃에 의존한다 (#124, 열림). | |
| 리스너 재시작 | 미ack 메시지는 재전달된다 (`S:README.md:103-113`). 재시작 전에 띄운 러너는 로컬 상태에서 사라진다. 예제는 모르는 러너 이름에 panic한다 (#150, 열림). ARC는 "찾지 못함"을 정상으로 무시한다. | 로컬 상태 DB와 서버 통계를 대조하는 복구 루프가 필요하다. |
| 같은 이름 중복 | `RunnerExistsError`. ARC는 `GetRunnerByName` 후 기존 등록을 지우고 재생성한다 (`A:ephemeralrunner_controller.go:860-880`). | |
| 죽은 HTTP/2 연결 | 최대 약 25분 정체했던 버그가 수정됐다 (#131 닫힘, `S:http_client_health_test.go`, 커밋 88a9274). | |

**조회·정리 API 전체 목록**: `GetRunner(id)`, `GetRunnerByName(name)`, `RemoveRunner(id)`, `GetRunnerScaleSet`, `GetRunnerScaleSetByID`, `ListRunnerScaleSets(runnerGroupID)` (`S:client.go:431-905`). 러너 목록 API는 없다. 러너 상태 조정(reconcile) 기능 요청 #89는 닫혔고 구현은 없다. 조정은 `Statistics`(합계)와 로컬 장부를 비교하는 수밖에 없다. 러너 개수 단위로만 비교할 수 있고, 어느 러너가 사라졌는지는 `GetRunner(id)`를 러너별로 호출해서 확인해야 한다 **[추론]**.

---

## 6. 한도와 안정성

| 항목 | 내용 | 출처 |
| --- | --- | --- |
| 상태 | Public Preview. "API는 안정적이나 인터페이스와 예제는 바뀔 수 있다" | `S:README.md:3` |
| 라이선스 | MIT | `S:LICENSE`, GitHub API `license: MIT` |
| Go 버전 | `go 1.27.3` (scaleset, ARC 동일) | `S:go.mod:3`, `A:go.mod:3` |
| 의존성 | `docker/docker`, `cobra`가 같은 `go.mod`에 있다 (예제용). 라이브러리 소비자에게 전이 의존성으로 보일 수 있다. | `S:go.mod` |
| 레포 나이 | 생성 2025-10-29, 릴리스 v0.1.0(2026-02-09) ~ v0.5.0(2026-10-23) | GitHub API (`gh api repos/actions/scaleset`) |
| 활동 | 최근 커밋: 2026-11-07(#145), 11-06(#142, #144), 10-29(#143), 10-23 다수. 마지막 push 2026-11-12. 별 222개, 열린 이슈 17개(PR 포함 여부는 미확인) | GitHub API |
| API 버전 | `api-version=6.0-preview` 고정 (비공개 Actions 서비스 API) | `S:client.go:395`, `S:session_client.go:160` |
| 재시도 | 기본: 전송 오류, 5xx(501 제외), 429를 최대 4회, 지수 백오프(1초 ~ 30초) | `S:README.md:259`, `S:http_client.go:254-256` |
| rate limit | 429와 `Retry-After`, `X-RateLimit-*`를 가진 403을 `*RateLimitError`로 분류. `OnRateLimit` 훅으로 정책 변경 가능. 한도 수치 자체는 코드에 없다 | `S:README.md:257-323`, `S:errors.go` |
| 폴링 | 서버 대기 약 50초, 클라이언트 한도 2분 | `S:README.md:91-103`, `S:http_client.go:73` |

rate limit 위험이 있는 호출은 `generatejitconfig`(러너 하나당 1회)와 등록 토큰 교환(REST, `api.github.com`)이다. 호스트 수십 대 규모에서 문제가 될 가능성은 낮다 **[추론]**. 수치는 측정하지 않았다.

관련 열린 이슈 (비 k8s 사용에 의미 있는 것):

- #107 job 고정/배정 계약과 `JobAvailable` 대 `JobAssigned` 경로 (답 없음)
- #133 `maxCapacity=0`일 때 큐 수요 관찰 불가
- #124 프로비저닝 실패 신호 없음
- #140 라벨 업데이트 무반응
- #122 long poll이 `JobStarted`에 반응하지 않음
- #150 예제가 모르는 러너 메시지에 panic
- #75 (닫힘) GHES에서 메시지 큐가 비어 있는 문제, acquire 복원으로 해결

---

## 7. 다중 호스트, 다중 OS 배치 컨트롤러의 차단/복잡 요소

1. **job을 러너에 고정할 수 없다.** job 단위 배치는 "러너를 어디에 띄울까" 수준에서 끝나고, 어떤 러너가 어떤 job을 받을지는 GitHub가 정한다 (§4). 크기 클래스, OS 클래스를 scale set으로 나누는 것이 유일하게 보장된 레버다. scale set 수가 늘면 listener와 세션도 그만큼 늘어난다.
2. **라벨 불변.** 라벨을 바꾸려면 scale set 재생성이 필요하다 (#140). 크기 클래스 체계를 먼저 확정해야 한다.
3. **`maxCapacity=0`일 때 수요가 보이지 않는다** (#133). 사람이 쓰는 머신이 바빠서 용량을 0으로 보고하면 해당 scale set의 대기 job을 볼 수 없다. 해결책: 호스트 합산 용량을 보고하되 실제 기동 시점에 배치하고, 기동 불가 시 job이 배정 취소 후 재큐잉(최대 3회)되는 경로에 의존하거나, 최소 용량 1을 유지한다. 둘 다 서버 동작에 기대는 설계라 실측이 필요하다.
4. **`JobAvailable`/`JobAssigned` 두 경로.** github.com은 직접 `JobAssigned`(`runnerRequestId=0`)가 올 수 있고 GHES는 acquire가 필요하다 (§2). 어느 모드에서도 동작하는 `Scaler`를 만들어야 하고, "job을 거절하고 다른 scale set에 넘기기"는 지원 계약이 없다 (#107).
5. **러너 목록 API 부재.** 호스트 다운, 데스크톱 sleep, 에이전트 크래시로 생긴 유령 등록은 로컬 장부 + `GetRunner(id)`/`RemoveRunner(id)`로 직접 정리해야 한다. 컨트롤러가 재시작해도 장부가 남도록 영속 상태(예: SQLite)가 필요하다. 예제 수준 인메모리 상태는 부족하다 (#150).
6. **사람이 쓰는 머신 중 job 실행 중 중단.** 서버 쪽 job 실패 처리 시점은 미확인이다. 데스크톱은 sleep 방지(job 중 wake lock)와 사용 감지로 러너 시작을 막는 것이 현실적이다. 이미 시작된 job은 선점할 수 없다 **[추론]**.
7. **러너 쪽 운영.** JIT config를 호스트로 안전하게 전달하고(민감 값, `S:README.md` 에러 절에서도 "토큰 또는 JIT 설정을 담을 수 있음" 경고), 호스트별 에이전트가 `run.sh`/`run.cmd`를 띄워야 한다. 러너 바이너리 버전은 `DisableUpdate`를 쓰면 우리가 관리해야 한다. Windows, macOS 지원은 `actions/runner` 쪽 일이고 scaleset 레포에는 OS별 코드가 없다 (순수 Go HTTP 클라이언트라 어디서나 컴파일된다).
8. **preview 위험.** 비공개 서비스 API(`6.0-preview`)에 의존하고, 메인테이너 응답이 없는 계약 질문이 여럿 열려 있다 (#107, #133). 반면 커밋은 활발하고 ARC 자체가 이 라이브러리 위에 올라가 있어(`A:cmd/ghalistener/scaler/scaler.go:12-13`) 중단 가능성은 낮다 **[추론]**.
9. **GHES 지원.** 대상이 github.com이면 무관하나, GHES 3.18~3.20은 다중 라벨 플래그와 acquire 경로 이슈가 있다 (§1~3, #75).

## 실측으로 닫아야 할 항목

1. `runs-on` 매칭이 부분집합 규칙인지, 다중 라벨 scale set에서 단일 라벨 job이 매칭되는지.
2. 같은 라벨 scale set 여러 개 사이의 분배 방식.
3. idle 러너 호스트가 죽었을 때 `TotalRegisteredRunners`/`TotalIdleRunners`가 얼마 뒤에 줄어드는지, job 실행 중 사라졌을 때 job이 어떻게 종료되는지(시간, 결과).
4. 작은 `maxCapacity`로 job 배정 후 러너가 안 뜨면 취소, 재큐잉이 실제로 일어나는지와 지연 시간.
5. `JobAvailable` 모드와 직접 `JobAssigned` 모드가 어떤 조건에서 나타나는지.
