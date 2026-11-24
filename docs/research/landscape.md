# pumasi 랜드스케이프 조사 (2026-11-24)

목적: 이기종 플릿(상시 Linux 서버, 낮에 사람이 쓰는 Linux/Windows 데스크톱, Apple Silicon Mac)에 ephemeral GitHub Actions self-hosted runner 를 배치할 때, 호스트의 여유 CPU/메모리 · job 크기 · 사람 활동(HTCondor 식 yield)을 보고 머신을 고르는 컨트롤러가 이미 있는지 확인한다.

조사 방법: 아래 레포를 `.research/` 에 `git clone --depth 1` 로 받아 README/docs/코드를 직접 읽었다. 클론 경로는 `~/repo-mid/pumasi/.research/<name>`. 스타·마지막 push 는 `gh api repos/<owner>/<repo>` 값(2026-11-24 기준). 직접 읽지 못한 항목은 "미확인"으로 표기했다.

## 1. 비교표

표기: O = 확인됨, X = 없음(코드/문서에서 찾지 못함), △ = 부분/조건부, ? = 미확인.

| 프로젝트 | multi-host | OS (Linux/macOS/Windows) | capacity-aware 배치 | job-size 인지 | 사람 활동 yield | per-job ephemeral 격리 | warm cache | 공식 scale set API | 라이선스 | 활동 (stars / 마지막 push) |
|---|---|---|---|---|---|---|---|---|---|---|
| cloudbase/garm | △ 단일 컨트롤러가 여러 provider 를 호출. provider 가 원격 호스트를 가리킬 수 있으나 "호스트 선택" 개념은 없음 | O / X / O (OSType 은 `linux`·`windows`·`unknown` 뿐) | X | △ 풀/스케일셋의 flavor·label 로 수동 분기 | X | O (provider 별 VM/컨테이너) | △ min-idle 로 warm runner 유지, 이미지 캐시는 provider 몫 | O (자체 구현 `util/github/scalesets`) + 웹훅 풀 병행 | Apache-2.0 | ~500 / 2026-11-23 |
| ysya/runscaler | X (호스트당 프로세스 1개, "one process per host") | O / O(Tart) / X | △ `internal/capacity` 는 고정 슬롯 semaphore, `diskguard` 는 디스크만. CPU·메모리 로드는 안 봄 | X | X | O | O Tart VM warm pool, Docker 캐시 receiver | O (`actions/scaleset` v0.5.0) | SUL 1.0 (n8n 계열, 비OSI. 내부 업무·비상업 용도, 재배포 제한) | ~13 / 2026-11-19 |
| ephpm/ephemerd | X (Windows 호스트가 자기 WSL/Hyper-V VM 에 gRPC dispatch 하는 것이 전부) | O / O / O | X 슬롯 semaphore(`pkg/scheduler/slots.go`) | X | X | O (containerd, Hyper-V 컨테이너, macOS APFS clone VM) | △ OCI 이미지를 artifact 캐시로 사용 | X (go-github 폴링/웹훅) | MIT | ~16 / 2026-11-11 |
| shapehq/tartelet | X (단일 Mac 앱) | X / O / X | X | X | X | O (Tart VM) | X | X | MIT | ~816 / 2026-02-10 |
| khoi/sand | X | X / O / X | X | X | X | O (Tart VM clone) | △ runner 타르볼을 호스트에 캐시 | X (JIT/PAT 등록) | MIT | ~69 / 2026-11-02 |
| efrecon/gh-runner-krunvm | X | O / O / X (krunvm microVM) | X | X | X | O | X | X | MIT | ~30 / 2025-01-24 (사실상 중단) |
| hostinger/fireactions | X (서버 1대 + Firecracker, 풀 단위) | O / X / X (KVM 필요) | X (풀 min 개수 유지) | △ 풀 별 VM 자원 정의 | X | O (Firecracker microVM) | △ 이미지 | X (웹훅 + JIT) | Apache-2.0 | ~208 / 2026-11-17 |
| cirruslabs/orchard (+ Tart) | O (워커 클러스터) | X / O / X | △ VM 이 선언한 CPU/mem 합 vs 워커 총량을 비교(`internal/controller/scheduler/workerinfo.go`의 `ResourcesUsed`). 실측 로드는 아님 | △ VM 별 cpu/memory 지정 | X | O | O Tart 이미지 | X (레포에 GitHub Actions 연동 없음. 별도 상용 제품이 있다는 점은 미확인) | FSL-1.1-ALv2 (2년 후 Apache-2.0 전환) | ~624 / 2026-11-20 |
| Veertu Anka Build Cloud (상용) | O | X / O (Intel·Apple Silicon 혼합) / X | △ 노드 capacity mode 가 VM 개수 또는 CPU·RAM 여유 기준 | △ | ? | O | ? | ? (문서에 Controller+Registry / Controller-less 두 가지 GitHub Actions 연동이 있음. scale set 인지는 미확인) | 상용 | 비공개 |
| eyupio/zoomies | O (컨트롤러 1 + 에이전트 N, 에이전트는 outbound 연결) | O / △ / △ (러너는 Linux 컨테이너뿐. Mac 은 Docker Desktop 의 Linux 컨테이너, Windows 는 WSL2 distro) | O 후보 호스트를 "배치 후 CPU·메모리 여유"로 랭킹(`docs/architecture.md` 4번 항목), 에이전트 heartbeat 의 사용량 측정 반영 | △ 풀(label) 단위로 크기 지정 | X (docs 에서 키보드/사용자 활동 개념을 찾지 못함) | O | △ 레지스트리 미러, persistent cache 문서 있음 | X (`workflow_job` 웹훅 + JIT, 폴링 폴백) | AGPL-3.0 | ~96 / 2026-11-24 (신생, 매우 활발) |
| actions/actions-runner-controller | O (k8s 노드) | O / X / △ | △ k8s 스케줄러가 pod requests 로 배치 | △ 스케일셋별 pod spec | X | O | △ | O (참조 구현) | Apache-2.0 | ~6.7k / 2026-11-23 |
| actions/scaleset (Go 클라이언트) | 해당 없음 (라이브러리) | 해당 없음 | 해당 없음 | 메시지에 `RequestLabels` 포함 | 해당 없음 | 기본 ephemeral | 해당 없음 | 그 자체 | MIT | ~222 / 2026-11-12 (Public Preview) |
| TuroYT/pitlane | X (Docker 호스트 1대) | O / X / X | X | X | X | O + 장수명 DinD 슬롯으로 캐시 | O | O | MIT | ~0 / 2026-11-23 |
| Codemany (상용 앱) | △ 머신별 에이전트 | O / O / X | X (머신별 runner 수 고정) | X | ? | O | ? | ? | 상용 | 비공개 |
| HTCondor (참고용 스케줄러) | O | O / O / O | O (LoadAvg, 슬롯 자원) | O (ClassAd request_cpus/memory) | O (KeyboardIdle/ConsoleIdle) | △ (컨테이너 universe) | X | 해당 없음 | Apache-2.0 | 성숙 |

출처:
- garm: `.research/garm/doc/{providers,external_provider,scale-sets,pools-and-scaling}.md`, `params/params.go:494`, `vendor/github.com/cloudbase/garm-provider-common/params/params.go:31-34`, `workers/scaleset/scaleset_listener.go:196-256`, `workers/scaleset/scaleset.go:1282-1285`. https://github.com/cloudbase/garm
- runscaler: `.research/runscaler/README.md`, `LICENSE`, `go.mod`, `internal/capacity/broker.go`, `internal/diskguard/`. https://github.com/ysya/runscaler
- ephemerd: `.research/ephemerd/README.md`, `pkg/scheduler/slots.go`, `pkg/scheduler/dispatch.go`. https://github.com/ephpm/ephemerd
- tartelet: https://github.com/shapehq/tartelet (README/메타데이터만 확인, 코드 미독). sand: `.research/sand/README.md`. https://github.com/khoi/sand
- fireactions: `.research/fireactions/README.md`. https://github.com/hostinger/fireactions
- krunvm 러너: https://github.com/efrecon/gh-runner-krunvm (메타데이터만, 2025-01 이후 커밋 없음)
- orchard: `.research/orchard/internal/controller/scheduler/{scheduler.go,workerinfo.go}`, `LICENSE`. https://github.com/cirruslabs/orchard
- Anka: https://docs.veertu.com/anka/anka-build-cloud/ (본문에서 capacity mode 와 두 가지 GitHub Actions 연동 언급만 확인)
- zoomies: `.research/zoomies/README.md`, `docs/architecture.md`(104-135행), `docs/hosts-and-pools.md`, `LICENSE`. https://github.com/eyupio/zoomies
- scaleset: `.research/scaleset/README.md`, `types.go`(`JobMessageBase.RequestLabels`). https://github.com/actions/scaleset
- awesome-runners 매트릭스: `.research/awesome-runners/README.md` 36-78행. https://github.com/jonico/awesome-runners
- HTCondor: https://htcondor.readthedocs.io/en/23.0/_sources/admin-manual/policy-configuration.rst.txt, https://research.cs.wisc.edu/htcondor/uwcs/policy.html (직접 본문은 읽지 못하고 검색 요약으로 확인한 부분 있음)

## 2. 항목별 상세

### 2.1 GARM

- provider 모델: 외부 실행 파일을 `GARM_COMMAND` 환경변수로 호출하는 방식이다. 연산은 `CreateInstance/DeleteInstance/GetInstance/ListInstances/RemoveAllInstances/Stop/Start` 7개이고, 부트스트랩 정보는 stdin JSON(`BootstrapInstance`)으로 받는다 (`doc/external_provider.md`). JSON 에 `flavor`, `image`, `labels`, `extra_specs`, `os_type`, `arch`, `pool_id` 가 들어 있어 "크기 클래스"를 provider 쪽에 전달할 수는 있다.
- 한계 1, 호스트 선택 위치가 없다: 풀/스케일셋 하나는 `provider_name` 하나에 묶인다 (`workers/scaleset/controller_watcher.go:84` 에서 `c.providers[scaleSet.ProviderName]` 조회). 즉 "어느 호스트"는 provider 실행 파일 안에서 정해야 한다. GARM 은 호스트 부하 신호를 받지도, 넘기지도 않는다.
- 한계 2, provider 호출은 상태 없는 프로세스 exec 다. 호스트 로드와 사람 활동을 추적하려면 provider 가 결국 별도 데몬과 통신하는 얇은 shim 이 되어, GARM 의 가치(상태·정책)가 shim 바깥에 남는다.
- 한계 3, macOS 가 모델에 없다: 공용 타입 `OSType` 은 `windows`, `linux`, `unknown` 이다. 다만 부트스트랩 `tools` 배열에는 `osx` 항목이 있으므로(`doc/external_provider.md`) 우회는 가능해 보이나, 풀 `--os-type` 은 `linux|windows` 로 문서화되어 있다 (`doc/pools-and-scaling.md:53`).
- 한계 4, yield(실행 중 job 을 비키기)는 provider 인터페이스로 표현할 수 없다. `Stop/Start` 는 인스턴스 단위이고 job 취소·재큐잉 개념이 없다.
- scale set: 지원한다. GARM 은 `actions/scaleset` 패키지를 쓰지 않고 자체 클라이언트(`util/github/scalesets`)를 구현했다. 리스너가 `JobAvailable` 을 받아 `AcquireJobs` 를 호출하고 `TotalAssignedJobs` 로 desired count 를 갱신한다 (`workers/scaleset/scaleset_listener.go:196-256`). 목표 runner 수는 `min(MinIdle + Desired, MaxRunners)` (`scaleset.go:1285`).
- pool 의미: `--min-idle-runners`(기본 0)는 warm runner 수, `--max-runners`(기본 5)는 상한 (`doc/pools-and-scaling.md:56-57`). 스케일셋에서는 GitHub 이 스케줄링을 하고(`doc/scale-sets.md` 표) 이름이 `runs-on` 라벨이다. 라벨은 생성 후 변경 불가.
- 스택이 무겁다: DB, REST API, 인증, 웹UI, 다수 forge/provider 지원. 확장 지점은 "provider 추가"이지 "스케줄러 교체"가 아니다.
- 활동: 매우 활발(2026-11-23 push), 약 500 stars, Apache-2.0.

### 2.2 runscaler

- 가장 가까운 "공식 scale set API + Docker/Tart(macOS)" 구현. `actions/scaleset` v0.5.0 사용, Tart VM warm pool(약 2초 vs 30초), Docker 캐시 receiver.
- 호스트당 프로세스 1개이며 멀티호스트 조정이 없다. `internal/capacity` 는 `NewBroker(limit)` 형태의 고정 슬롯 semaphore 이고 실측 CPU/mem 을 읽지 않는다. `diskguard` 는 디스크 여유만 본다.
- 라이선스가 SUL 1.0 (n8n 커뮤니티 라이선스 계열)이다. 과거 릴리스는 MIT 였으나 현재 HEAD 는 비OSI 이고 "내부 업무·비상업 목적"에 한정된다. 코드를 가져다 쓰거나 포크하는 방식의 확장은 오픈소스 프로젝트 베이스로 부적합하다. 설계 참고만 한다.
- 슬롯 개념이 이미 있으므로, 이것이 "11개 슬롯이 호스트 부하와 무관하게 job 을 받는" 문제의 정확한 원형이다.

### 2.3 ephemerd

- Linux(containerd 내장) / Windows(Hyper-V 격리 컨테이너 + HCS 로 부팅한 Linux VM) / macOS(Virtualization.framework Linux VM + APFS clone macOS VM) 를 단일 바이너리로 덮는 유일한 프로젝트. Windows·macOS 에서 "무엇이 실용적인가"의 선행 사례로 가치가 크다.
- 한 호스트 안에서만 동작한다(`dispatch.go` 의 gRPC 는 호스트→자기 VM). 슬롯은 `sem/linuxSem/macSem` 버퍼 채널 (`slots.go`). 부하 인지 없음.
- scale set 이 아닌 go-github 기반 폴링/웹훅이다 (`go.mod` 에 `actions/scaleset` 없음). 신생(약 16 stars).

### 2.4 Tartelet / sand / krunvm / Fireactions

- 모두 "한 호스트에서 ephemeral VM 을 N개 유지"하는 단일 호스트 도구다. 호스트 선택·부하 인지·사람 yield 모두 없다.
- Tartelet 은 가장 인지도가 높으나(약 816 stars) 2026-02 이후 push 가 없다. sand 는 Tart 기반으로 활발하다. krunvm 러너는 사실상 중단. Fireactions 는 Firecracker(KVM) 전용이라 Mac/Windows 불가.

### 2.5 Orchard / Anka

- Orchard 는 Tart VM 을 Apple Silicon 클러스터에 분산하는 스케줄러다. 스케줄링 루프(`scheduler.go` `schedulingLoopIteration`)는 워커의 오프라인·일시중지 여부, arch/runtime 호환, 엔드포인트 capability 를 보고, VM 에 CPU/메모리를 지정하지 않으면 워커 기본값(기본 4 CPU / 8192 MB)을 채운다(`scheduler.go:362-383`). 워커 사용량은 배정된 VM 의 선언 자원 합계(`workerinfo.go`)다. 즉 선언량 기반 bin-packing 이고 실측 로드(사람이 쓰는 CPU)는 보지 않는다. GitHub 연동은 이 레포에 없고 runner 를 VM 안에 띄우는 일은 사용자 몫이다. macOS 전용이다.
- Anka Build Cloud 는 상용이며 문서상 노드 capacity mode(VM 개수 또는 CPU·RAM 여유)와 GitHub Actions 연동 두 가지(Controller+Registry, Controller-less)가 있다. 내부 스케줄러 코드는 비공개라 부하·사람 yield 여부는 확인하지 못했다.

### 2.5.1 zoomies (가장 근접한 경쟁자)

- 컨트롤러 1 + 에이전트 N 구조. 배치는 순수 함수 `internal/scheduler.Decide` 가 한다: 건강하고, cordon 아니고, 풀이 요구하는 backend·platform·host selector 에 맞고, 자리가 남은 호스트 중에서 "배치 후 CPU/메모리 여유"로 순위를 매긴다 (`docs/architecture.md` 113-126행). 에이전트 heartbeat 의 사용량 측정과 압력 hold 가 스냅샷에 반영된다. 이름 그대로 "capacity-aware 멀티호스트 배치"이다.
- 한계: (a) 러너는 Linux 컨테이너뿐이다. Mac 은 Docker Desktop 의 Linux 컨테이너, Windows 데스크톱은 WSL2 distro 로 취급(`docs/hosts-and-pools.md` 168행 이하). 네이티브 macOS/Windows job(Xcode, .NET Windows 전용 등)은 못 돌린다. (b) 사람 활동 yield 개념을 docs 에서 찾지 못했다(키워드 keyboard/idle/human/workstation 검색 결과 무관한 문맥뿐). (c) 웹훅 + JIT 방식이고 scale set API 가 아니다(`go.mod` 에 `actions/scaleset` 없음, go-github v88). (d) AGPL-3.0 이라 MIT/Apache 프로젝트에 코드를 흡수할 수 없다. (e) 신생(약 96 stars)이며 UI·백업·AI 컨텍스트·MCP 등 범위가 넓어 "얇은 컨트롤러"로 떼어 쓰기 어렵다.

### 2.6 awesome-runners 매트릭스 스캔

매트릭스(36-78행)에서 멀티호스트/이기종/유휴 데스크톱과 관련된 항목은 다음뿐이다.
- `eyupio/zoomies`: "per-pool limits across existing hosts", Linux amd64/arm64.
- `Flinner/gh-runnerctl`: ssh 로 여러 호스트, 고정 크기 풀(autoscale 없음), 2 stars.
- `Codemany`: 사용자 Mac·Linux 에 단일 사용 러너, 상용.
- `cisco-open/forge`: ARC + AWS 래퍼. 클라우드 중심.
- `i2Dev-com/homefield`: Lima VM, Mac 한 대.
"사람이 쓰는 머신 yield"나 "실측 로드 배치"를 명시한 항목은 없었다.

### 2.7 HTCondor 와 Nomad

- HTCondor 의 데스크톱 정책: `START`(시작 조건), `SUSPEND`(중지), `CONTINUE`(재개), `PREEMPT`(퇴출), `KeyboardIdle`/`ConsoleIdle`(입력 유휴 시간), `LoadAvg`와 `NonCondorLoadAvg = LoadAvg - CondorLoadAvg`(사람 쪽 부하만 분리) 를 ClassAd 표현식으로 조합한다. 위스콘신 CS 풀의 예: 1분 load 0.3 미만 + 콘솔 15분 유휴 때 시작, 중지 후 10분 지나면 preempt. `POLICY : Desktop` 템플릿도 있다. 출처: https://htcondor.readthedocs.io/en/23.0/_sources/admin-manual/policy-configuration.rst.txt, https://research.cs.wisc.edu/htcondor/uwcs/policy.html. 이 모델이 pumasi 의 yield 정책에 그대로 대응한다. 다만 정확한 기본식은 직접 읽지 못했으므로 구현 전에 현행 문서로 재확인이 필요하다.
- "HTCondor 잡으로 GitHub runner 를 돌리는" 공개 사례는 검색으로 찾지 못했다 (`--ephemeral`/JIT runner 는 job 1개 실행 후 종료하므로 condor job 으로 감쌀 수는 있으나 사례 미확인). 장점은 정책이 성숙한 점, 단점은 central manager/negotiator/ClassAd 운영 부담, 그리고 scale set 의 "JobAvailable 시점에 JIT 생성" 모델과의 접착을 직접 만들어야 한다는 점이다.
- Nomad 기반 GitHub runner 오토스케일러는 검색에서 찾지 못했다. 검색 결과는 오히려 "Nomad 용 드라이버는 `actions/scaleset` 위에 직접 만들어야 한다"는 방향을 가리킨다.

### 2.8 Windows 데스크톱의 ephemeral 옵션 (실용성)

- Windows 컨테이너(process isolation): Windows 호스트 버전과 컨테이너 이미지 버전이 맞아야 한다. 데스크톱 Windows 11 에서 Windows 컨테이너 이미지 준비가 번거롭다 (일반 지식, 이번 조사에서 별도 출처 미확인).
- Hyper-V 격리 컨테이너: ephemerd 가 Hyper-V 격리 컨테이너 + HCS 로 부팅한 Linux VM 조합을 구현했다 (`.research/ephemerd/README.md` Windows 절). 사람이 쓰는 데스크톱에서 `LocalSystem` 서비스로 돌 수 있다는 점이 강점으로 적혀 있으며, 이 접근이 가장 실증된 경로다.
- Hyper-V VM clone / checkpoint 복원: 기술적으로 가능하나 공개된 per-job 복원 사례는 이번 검색에서 찾지 못했다. 이미지 크기와 부팅 시간이 부담이다 (Windows 러너 부팅 1분 이상이라는 상용 서비스 문서 보고: https://runs-on.com/runners/windows/).
- Windows Sandbox: 인스턴스 1개 제한, 자동화/서비스 구동에 부적합하며 CI 사례도 확인하지 못했다. 비추천.
- 현실적 최소안: Windows 데스크톱은 WSL2/Docker 기반 Linux 러너(zoomies 방식)로 먼저 덮고, 네이티브 Windows job 이 꼭 필요한 경우만 Hyper-V 격리 컨테이너 또는 VM 으로 확장한다.

## 3. 판정

### 3.1 gap 은 실재하는가: 부분적으로 실재한다

이미 있는 것:
- 멀티호스트 + 호스트 여유 기반 배치: zoomies(Linux 컨테이너 한정, AGPL, 웹훅 모델), Orchard(Mac 한정, 선언 자원 기준).
- scale set API 기반 Docker/Tart 오토스케일러: runscaler(단일 호스트, 비OSI 라이선스).
- 3대 OS 단일 호스트 ephemeral 격리: ephemerd.

없는 것(이번 조사에서 하나도 찾지 못함):
1. 사람 활동 yield(키보드/콘솔 유휴, 비-runner load 분리, 실행 중 job 에 대한 정책).
2. 실측 호스트 로드(사람의 사용분 포함)를 반영한 배치와, 그 결과를 scale set 의 보고 capacity 로 되먹임하는 구조.
3. 네이티브 macOS/Windows 호스트와 Linux 서버를 한 컨트롤러가 함께 다루는 구성. zoomies 는 Linux 컨테이너, Orchard 는 Mac 전용, ephemerd 는 호스트별 독립 데몬이다.
4. job 크기 클래스(2코어 lint vs 8코어 e2e + 자체 compose 스택)를 라벨에서 읽어 배치 가중치로 쓰는 구조. 크기를 풀/VM 단위로 수동 선언하는 도구는 있으나 라벨→자원 요구→여유 호스트 선택 파이프라인은 없다. 이 항목은 zoomies 풀 모델과 가장 겹치므로 "존재하지 않는다"고 단정하지는 않는다.

따라서 "capacity-aware 멀티호스트" 자체는 신규가 아니고(zoomies), 차별점은 (1) 사람 yield, (2) 실측 부하 + 네이티브 Mac/Windows, (3) 크기 클래스 입력 세 가지다. 사람 yield 가 가장 강한 공백이고, 이것은 HTCondor 가 20년 전에 풀어 둔 정책 모델을 GitHub Actions 쪽에 이식하는 작업이다.

### 3.2 확장 대상 vs 신규 작성

| 선택지 | 장점 | 결정적 단점 |
|---|---|---|
| GARM 에 placement provider 추가 | Apache-2.0, 활발, scale set 지원, Windows 지원, 운영 기능(DB/REST/UI) 이미 있음 | 풀/스케일셋이 provider 1개에 고정(`controller_watcher.go:84`)이라 호스트 선택은 provider shim 안에서 해야 함. provider 호출이 stateless exec 라 부하/활동 상태는 어차피 별도 데몬 필요. `OSType` 에 macOS 없음. yield(실행 중 job 퇴출)는 인터페이스에 없음 |
| zoomies 포크/기여 | 멀티호스트·headroom 배치·에이전트 구조가 이미 있음 | AGPL-3.0, Linux 컨테이너 전용, 웹훅 모델, 범위가 넓은 신생 프로젝트라 구조 영향 큼 |
| runscaler 포크 | scaleset + Docker/Tart 구현이 정돈됨 | SUL 1.0 비OSI 라이선스. 코드 재사용 불가 |
| ephemerd 확장 | 3대 OS 격리 경험 | 단일 호스트 가정, 폴링/웹훅(scale set 아님), 신생, 슬롯 semaphore 구조 |
| `actions/scaleset` 위에 신규 작성 | MIT, 라이선스 자유. 메시지에 `RequestLabels` 가 있어 job 크기를 배치 전에 안다 (`scaleset/types.go` `JobMessageBase`). 컨트롤러가 폴링 시 "최대 capacity"를 보고하므로(`scaleset/README.md` 2번) 실측 여유를 capacity 로 되먹일 수 있다. 호스트 에이전트에서 네이티브 yield 정책을 직접 구현 가능 | 직접 만들어야 하는 양(에이전트, provider(Docker/Tart/Hyper-V), 상태 저장). `actions/scaleset` 은 Public Preview |

### 3.3 결론: scale set 위에 신규 작성, 단 GARM 과 경쟁하지 않고 어댑터로 병행

권고는 `actions/scaleset`(MIT) 위에 "얇은 컨트롤러 + 호스트 에이전트"를 새로 만드는 것이다. 근거:

1. 필요한 입력이 scale set 쪽에서 이미 나온다. `JobAvailable` 에 `RequestLabels`/`RepositoryName` 이 있어 크기 클래스를 알 수 있고, 리스너는 `AcquireJobs` 로 "이 job 을 내가 맡는다"를 확정하기 전에 후보 호스트의 여유를 확인할 수 있다 (GARM 도 같은 흐름이다. `scaleset_listener.go:196-256`). runner 를 JIT 로 job 도착 후에 만드는 모드(min-idle 0)에서는 배치 결정 시점이 job 정보가 알려진 뒤라서, 기존 문제("GitHub 이 아무 idle 슬롯에나 job 을 준다")의 원인이 구조적으로 제거된다. 대가는 cold start 지연이며, 이는 크기 클래스별 소수의 warm runner 로 완화한다.
2. 슬롯 개수 대신 실측 capacity 보고. runscaler 와 ephemerd 는 고정 슬롯 semaphore 다. 컨트롤러가 폴링마다 보고하는 최대 capacity 를 `호스트 여유로 동적으로 계산한 값`으로 바꾸면 11개 슬롯 문제를 직접 해결한다. 이 값이 사람 활동에 따라 0 으로 내려가는 것이 yield 의 시작점이다.
3. GARM 확장은 위 표의 이유로 핵심 공백(yield, macOS)을 못 덮는다. 다만 GARM 사용자가 많으므로, 나중에 같은 에이전트를 GARM external provider shim 으로 노출하는 어댑터를 따로 제공하는 방안은 열어 둔다 (provider 인터페이스가 stdin JSON + 환경변수로 단순해서 비용이 작다).
4. zoomies 와 runscaler 는 코드 재사용이 불가능하다(AGPL / SUL). 설계 참고만 한다. 특히 zoomies 의 "순수 함수 `Decide(snapshot) -> Plan` + reason string"은 테스트 가능한 스케줄러 구조로 차용 가치가 크다 (아이디어 차용이며 코드 복사 아님).
5. Windows/macOS 격리 방식은 ephemerd(MIT)의 구현을 참고 자료로 삼을 수 있다 (Hyper-V 격리 컨테이너, APFS clone VM). 코드 인용 시 MIT 고지 의무를 지킨다.

### 3.4 위험과 열린 질문

- `actions/scaleset` 은 Public Preview 라 인터페이스가 바뀔 수 있다. GARM 이 자체 클라이언트를 구현한 이유일 수 있으니 이 지점은 추가 확인한다.
- scale set 은 GitHub 이 idle runner 에 job 을 배정하므로, "capacity 0 으로 yield" 는 새 job 을 막을 뿐이다. 이미 실행 중인 job 을 중단하는 정책(HTCondor 의 SUSPEND/PREEMPT 에 해당)은 job 재시도 의미가 GitHub 에는 없어서(runner 가 사라지면 job 이 실패) 신중한 설계가 필요하다. 실용 기본값은 "새 job 시작 차단 + 진행 중 job 은 끝까지 두되 nice/cgroup 으로 우선순위 낮춤"이다.
- 사람 활동 신호의 OS 별 수집(macOS `ioreg HIDIdleTime`, Windows `GetLastInputInfo`, Linux X11/logind idle)은 이번 조사에서 코드로 검증하지 않았다.
- 크기 클래스가 라벨(`runs-on` 이름)로 이미 나뉘어 있어야 하므로, 워크플로 쪽에서 스케일셋 이름을 `lint`/`e2e` 등으로 분리하는 마이그레이션이 필요하다.
- 미확인: Anka·Codemany 의 내부 스케줄링, Orchard 와 GitHub Actions 연동 방식, 상용 제품의 사람 활동 정책.
