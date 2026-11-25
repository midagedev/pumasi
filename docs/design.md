# pumasi 설계 초안 (2026-11-24)

문제는 [problem.md](problem.md), 근거 조사는 [research/scaleset-api.md](research/scaleset-api.md) · [research/landscape.md](research/landscape.md).

## 결론 먼저

- **GitHub 공식 `actions/scaleset`(MIT) 위에 새로 만든다.** GARM 은 scale set 하나가 제공자 하나에 묶이고(`workers/scaleset/controller_watcher.go:84`) macOS 를 모른다. 가장 가까운 zoomies 는 Linux 컨테이너만 돌리고 AGPL 이다. 사람 활동에 비켜 주는 기능은 어디에도 없다.
- 남이 이미 잘하는 것(러너 격리, Tart VM, Hyper-V)은 드라이버로 가져다 쓰고, **배치 결정과 사람 우선 정책**만 우리 몫으로 한다.
- GARM 과는 경쟁하지 않는다. 나중에 GARM 외부 제공자 어댑터를 둘 수 있다.

## 손잡이는 scale set 뿐이다

scaleset API 조사에서 나온 제약이 설계를 정한다.

| 사실 | 근거 | 설계에 주는 뜻 |
|---|---|---|
| 같은 scale set 안에서는 GitHub 이 비어 있는 아무 러너에 잡을 준다. 특정 잡을 특정 러너로 보낼 수 없다 | `README.md:20`, JIT 설정이 잡이 아니라 scale set 에 묶임 `client.go:780` | **잡 크기·OS 마다 scale set 을 따로 만든다.** 이것이 유일한 분류 수단이다 |
| `JobAvailable`·`JobAssigned` 에 레포·워크플로·잡 이름·요청 라벨이 러너가 생기기 전에 실려 온다 | `types.go:47-62` | 무엇이 기다리는지 보고 몇 개를 어디에 띄울지 정할 수 있다 |
| scale set 은 라벨 여러 개를 가질 수 있지만 만든 뒤엔 못 바꾼다 | `client.go:571-583`, 이슈 #140 | 클래스 정의가 바뀌면 이름에 버전을 붙여 새로 만든다 (`linux-l-v2`) |
| 러너 목록 조회 API 가 없다. 죽은 러너는 우리가 지울 때까지 남는다 | scaleset-api.md Q5 | **컨트롤러가 러너 장부를 저장하고** 에이전트 heartbeat 로 주기적으로 맞춘다 |
| `maxCapacity=0` 이면 대기 중인 잡이 안 보인다 | 이슈 #133 | 자리가 없다고 0 을 보고하지 않는다. 용량 상한은 넉넉히 두고, **러너를 안 띄우는 것으로** 기다리게 한다 |
| 아직 `6.0-preview` | scaleset-api.md Q6 | API 를 얇은 내부 인터페이스 뒤에 감춘다 |

## 구성

```
                 GitHub (scale set API)
                        │  listener (클래스별 1개)
              ┌─────────▼──────────┐
              │   controller        │  늘 켜진 서버 1대
              │  - 클래스 정의       │
              │  - 러너 장부 (SQLite)│
              │  - 배치 결정 Decide  │
              └───▲───────────▲─────┘
        outbound  │           │  outbound (에이전트가 먼저 연결)
          ┌───────┴──┐   ┌────┴──────┐   ...
          │ agent     │   │ agent      │
          │ Linux 서버 │   │ Mac 노트북  │
          │ driver:   │   │ driver:    │
          │  docker   │   │  native/tart│
          └───────────┘   └────────────┘
```

- **controller**: 클래스마다 listener 를 돌려 수요(대기 잡 수와 라벨)를 받고, 에이전트들이 보낸 호스트 상태로 배치를 정해 "이 호스트에 이 클래스 러너 하나"를 지시한다. JIT 설정은 controller 가 만들어 넘긴다(에이전트는 GitHub 자격을 갖지 않는다).
- **agent**: 호스트마다 하나. 에이전트가 controller 로 먼저 연결한다(NAT·사내망·Tailscale 어디서든). 보고하는 것과 하는 것은 아래.
- **driver**: 러너를 실제로 띄우는 방법. 호스트 OS·신뢰 수준에 따라 고른다.

## 클래스

클래스 = scale set 하나 = 잡 크기·OS·신뢰 범위 묶음. 예:

| 클래스 | 자원 | 허용 호스트 | 비고 |
|---|---|---|---|
| `linux-s` | 2코어 / 4G | 서버 + 노는 데스크톱 | lint·단위 테스트·진단. 짧다 |
| `linux-l` | 8코어 / 16G, docker 사용 | 서버만 | compose 스택을 띄우는 e2e |
| `macos` | 4코어 / 8G | Mac | Mac 이 꼭 필요한 잡만 |
| `windows` | 4코어 / 8G | Windows | |
| `deploy` | 4코어 / 8G | 신뢰 서버만 | 배포 비밀값을 쓰는 잡은 여기로만 |

워크플로는 `runs-on: pumasi-linux-l` 처럼 클래스를 고른다. 크기가 다른 잡이 같은 슬롯 1개로 계산되던 문제가 여기서 풀린다.

## 에이전트가 보고하는 것

- 사양: 코어·메모리·OS·아키텍처, 쓸 수 있는 드라이버
- 지금 여유: 러너가 아닌 프로세스의 CPU·메모리 사용(사람 몫), 디스크 여유
- **사람 활동**: 마지막 입력 후 경과 시간, 화면 잠금 여부
- 돌고 있는 러너와 그 상태, 캐시 상태(어떤 레포의 캐시가 따뜻한지)
- 신뢰 수준(설정값): `trusted-server` / `desktop`

사람 활동 신호를 OS 별로 어떻게 읽는지는 아직 확인 전이다(열린 질문 3).

## 배치 결정

zoomies 의 방식을 빌린다: `Decide(snapshot) -> Plan` 순수 함수, 결정마다 사유 문자열을 남긴다. 같은 스냅샷이면 같은 결정 → 테스트와 재생이 쉽다.

1. **수요**: 클래스별 대기 잡 수 − 이미 뜨고 있거나 노는 그 클래스 러너 수.
2. **자격**: 클래스 허용 호스트이고, 사람 상태가 `available` 이고, `여유 − 예약분 ≥ 클래스 크기` 인 호스트만.
3. **순위**: 늘 켜진 서버 먼저 → 그 레포 캐시가 따뜻한 호스트 → 여유가 큰 호스트. 데스크톱은 서버가 꽉 찼을 때만.
4. **자리가 없으면 아무것도 안 한다.** 잡은 GitHub 큐에서 기다린다. 이게 "다 받아 놓고 같이 느려지는" 지금 문제의 반대다.
5. 노는 러너는 기본 0 개(min-idle 0). 잡이 오면 띄운다 → 러너 기동 시간만큼 늦어지는 대가가 있다. 클래스별로 min-idle 을 줄 수 있게 한다.

## 사람 우선 (HTCondor owner 정책)

호스트 상태는 셋이다.

| 상태 | 조건 (기본값, 설정 가능) | 새 러너 | 도는 잡 |
|---|---|---|---|
| `available` | 입력 없음 15분 이상 **그리고** 사람 몫 CPU 사용 < 20% | 받음 | — |
| `owner-active` | 입력이 있었거나 사람 몫 부하가 높음 | **안 받음** | 끝날 때까지 둔다 |
| `draining` | 수동 지정 (재부팅·작업 예정) | 안 받음 | 끝날 때까지 둔다 |

- 도는 잡을 중간에 죽이지 않는다. GitHub 에는 HTCondor 의 preempt·이주 같은 게 없어서, 죽이면 실패한 잡만 남는다. 그래서 데스크톱에는 짧은 클래스(`linux-s`)만 허용하는 것이 기본값이다.
- 돌고 있는 러너는 CPU 우선순위를 낮춰서(Linux `nice`/cgroup `cpu.weight`, macOS `taskpolicy`) 사람이 돌아온 뒤 끝날 때까지의 방해를 줄인다.

## 드라이버

| 드라이버 | 격리 | 쓰는 곳 | 주의 |
|---|---|---|---|
| `docker` | 컨테이너 1개 = 러너 1개, 끝나면 삭제 | Linux 서버·데스크톱, WSL2 | e2e 처럼 잡 안에서 docker 가 필요하면 dind 또는 sysbox. 호스트 docker 소켓을 넘기면 격리가 깨진다 |
| `native` | 러너마다 새 작업 디렉터리, 끝나면 삭제 | Mac, Windows | 격리가 약하다. 대신 Mac 동시 VM 제한(아래)에 안 걸린다 |
| `tart` | APFS 복제 macOS VM | Mac | macOS 라이선스가 Mac 한 대당 macOS VM 동시 2개까지만 허용한다 → Mac 한 대당 이 클래스 2개가 상한 |
| `kata` | `docker run --runtime kata` — 컨테이너 사용법 그대로, 격리는 경량 VM | Linux (KVM) | 컨테이너 드라이버에서 런타임만 바꾸는 중간 단계 |
| `microvm` | Firecracker·Cloud Hypervisor, 잡마다 커널 따로 | Linux (KVM) | VM 안에 dockerd 를 띄워 잡 안 docker 문제가 사라진다. e2e(`linux-l`) 후보 |
| `hyperv` | VM 복제 | Windows 서버 | 나중 |

기본 방향: 짧고 많은 잡(`linux-s`)은 컨테이너, compose 스택을 띄우는 잡(`linux-l`)은 `kata` → `microvm` 순으로 비교한다. Mac·Windows 는 VM 이 사실상 유일하다.

호스트의 정리 작업도 드라이버 몫이다. 컨테이너 안에서 virtiofs 너머로 지우지 말고 호스트에서 직접 지운다(실측: 수십 분 → 20초).

## 캐시

러너는 일회용이고 캐시는 호스트에 남는다. 에이전트가 `cache/<레포>/{pnpm-store,gradle,…}` 를 들고, 드라이버가 러너에 마운트한다. 쓰기 경합을 막는 단위(레포별 잠금 또는 읽기 전용 스냅샷 + 쓰기 오버레이)는 M4 에서 정한다. 배치 순위에 캐시가 따뜻한지가 들어간다.

## 장부와 맞추기

- controller 의 SQLite 에 러너 행: `id, class, host, state(requested|starting|idle|busy|done|lost), jit_name, created_at, job(run_id, repo, job_name)`.
- `JobStarted`/`JobCompleted` 의 `runnerName` 으로 어느 호스트가 잡을 받았는지 채운다.
- 에이전트 heartbeat 가 끊긴 호스트의 러너는 `lost` 로 두고 `RemoveRunner` 를 시도한다. `JobStillRunningError` 면 다음 주기에 다시.
- 러너를 띄우다 실패해도 GitHub 에 알릴 방법이 없다(이슈 #124). 장부에서 다른 호스트로 다시 띄운다.

## 메트릭 — VM 이든 컨테이너든 같은 모양으로

원칙: **러너 하나 = 측정 단위 하나.** 격리 방식이 달라도 에이전트가 호스트 쪽 경계에서 같은 지표를 같은 이름으로 낸다. 게스트 안 에이전트 없이도 숫자가 나와야 한다.

| OS | 측정 경계 | 읽는 것 |
|---|---|---|
| Linux | 러너마다 cgroup v2 하나. 컨테이너는 원래 cgroup 이고, `kata`·`microvm` 은 VMM 프로세스를 러너 전용 cgroup 에 넣는다 | `cpu.stat`, `memory.current`·`memory.peak`, `io.stat`, PSI(`cpu`·`memory`·`io.pressure`) |
| macOS | 러너 프로세스 트리 (Tart VM 은 Virtualization 프로세스) | `proc_pid_rusage` — CPU 시간, `phys_footprint`, 디스크 I/O |
| Windows | 러너마다 Job Object | `QueryInformationJobObject` 회계 정보 |

지표는 세 층이다.

| 층 | 예 | 라벨 |
|---|---|---|
| 러너(잡) 자원 | CPU 초, 메모리 peak, I/O 바이트, 압력 대기 시간, 네트워크 바이트 | host, class, driver, runner, repo, workflow, job, run_id |
| 잡 수명 | 큐 대기(`queueTime`→러너 배정), 기동 시간(배치 결정→러너 Listening), 실행 시간, 결과 | class, repo, workflow, job |
| 호스트·결정 | 클래스별 남은 자리, 사람 상태, 호스트 PSI, 배치 결정과 사유, 자리 없어 미룬 수 | host, class |

- 러너↔잡 연결은 `JobStarted` 의 `runnerName` 으로 한다. `JobAssigned` 에 repo·workflow·job 이름이 온다(M0 실측).
- 에이전트는 Prometheus(OpenMetrics) 엔드포인트를 연다. controller 는 잡이 끝날 때 요약 한 줄(peak 메모리, CPU 초, 대기·기동·실행 시간, 결과)을 장부에 남긴다 → 이력.
- 쓰임:
  1. **부하 판단은 load average 대신 PSI.** "load 74" 보다 "CPU 를 기다린 시간 비율"이 배치 판단에 맞다.
  2. **클래스 크기를 이력으로 맞춘다.** 워크플로·잡별 실측 peak 로 "이 잡은 `linux-s` 로 충분 / `linux-l` 이 필요"를 추천한다. 슬롯 1개 = 잡 1개 문제를 숫자로 푼다.
  3. **실패를 환경 탓과 코드 탓으로 가른다.** 잡이 실패한 시각의 호스트 PSI·러너 메모리 peak·사람 상태를 잡 옆에 붙여 보여 준다.

## 단계

| 단계 | 내용 | 끝났다는 기준 |
|---|---|---|
| **M0 검증 스파이크** | scale set 1개, Linux 호스트 1대, `docker` 드라이버로 잡 하나를 끝까지 | 아래 열린 질문 1·2·4 를 실제로 확인해 적는다 |
| M1 (끝, [실측](m1-results.md)) | 여러 Linux 호스트 + 용량 배치 + 장부·맞추기 + 러너 cgroup 메트릭·잡 수명 지표 | 서버 2대에 `linux-s`·`linux-l` 를 섞어 넣어도 클래스 크기 합이 호스트 용량을 넘지 않고, 잡마다 peak 메모리·CPU 초가 장부에 남는다 |
| M2 | 사람 우선 (Linux 데스크톱, Mac) | 입력이 생기면 새 러너가 그 호스트에 안 뜬다 |
| M3 | Mac `native`·`tart`, Windows `native` | 세 OS 에서 잡 하나씩 |
| M4 | 따뜻한 캐시, 잡 안 docker (e2e) | compose 스택 e2e 한 벌이 `linux-l` 에서 통과 |
| 이후 | 결정 로그 대시보드, GARM 어댑터, Gitea | — |

## M0 실측 (2026-11-24)

컨트롤러는 노트북, 러너는 Linux 서버 1대에 `ssh <host> docker run` 으로 띄웠다(PAT 는 서버에 두지 않고 JIT 설정만 stdin 으로 넘김). 원본 이벤트는 스파이크 프로그램(`cmd/pumasi-m0`, M1 뒤 삭제)이 남긴 JSON lines.

| 질문 | 결과 | 설계에 주는 뜻 |
|---|---|---|
| `runs-on` 매칭 | 잡 라벨이 **전부** scale set 라벨 안에 있으면 그 scale set 으로 온다. `pumasi-m0` · `pumasi-extra` · `[pumasi-m0, pumasi-extra]` 모두 통과. **`[self-hosted, pumasi-m0]` 은 scale set 에 메시지조차 오지 않고 대기만 한다** — scale set 에는 `self-hosted` 가 자동으로 붙지 않는다 | 기존 `runs-on: [self-hosted, X]` 워크플로를 옮기려면 워크플로를 고치거나 클래스 라벨에 `self-hosted` 를 넣어야 한다(넣는 쪽은 미확인) |
| github.com 메시지 순서 | `JobAvailable` 없이 `JobAssigned` 가 바로 온다(`runnerRequestId=0`). repo·workflow·job 이름·요청 라벨이 실려 있다 | 수요 = `JobAssigned` 기준. 잡 단위 정보로 배치 판단 가능 |
| 러너가 조용히 죽으면 | 폐기된 러너 버전(2.329.0)은 접속 직후 **종료 코드 0** 으로 끝났다. GitHub 은 약 5분 뒤 그 배정을 끝내고(`JobCompleted`) 다시 배정했다 | 컨테이너·VM 종료를 에이전트가 직접 감시해 즉시 다시 띄운다. 러너 버전은 최신을 따라간다 |
| 잡 도중 호스트 사망 | 러너를 강제 종료한 뒤 **약 9분 43초** 만에 잡이 `failure` 로 끝났다("self-hosted runner lost communication"). 그동안 메시지 없음. 자동 재시도 없음. 끝난 뒤에도 등록 러너 수가 1 로 남음 | 데스크톱에 긴 잡을 보내지 않는 정책이 맞다. 죽은 등록은 장부로 찾아 `RemoveRunner` |
| 러너 없이 대기 | `JobAssigned` → 5분 뒤 `JobCompleted`(빈 결과) + 재배정, 이것이 **3번**(10:46 · 10:51 · 10:56). 그 뒤 **35분간 메시지 없음**, 잡은 계속 `queued`. 46분 뒤 컨트롤러를 다시 시작하자 세션 첫 통계(`totalAssignedJobs=1`)로 러너를 띄웠고 잡을 가져가 성공 | "자리가 없으면 큐에서 기다린다"는 성립한다. 단 **재배정 3번 뒤엔 새 메시지가 없으므로**, 미룬 수요를 장부에 들고 있다가 자리가 나면 컨트롤러가 스스로 러너를 띄워야 한다(메시지를 기다리면 영원히 안 온다). 24시간 큐 한도는 미확인 |
| `self-hosted` 라벨 | 라벨 `[self-hosted, pumasi-m0]` 인 scale set 을 만들 수 있고, `runs-on: [self-hosted, pumasi-m0]` 잡을 받아 성공 | **기존 워크플로를 안 고치고 옮길 수 있다.** 클래스 라벨 = 기존 러너 라벨 그대로 |
| 러너 기동 시간 | 한가한 호스트에서 컨테이너 시작 → 잡 시작 약 8초. 부하가 큰 호스트(load 70+)에서는 **약 2분 40초**(GitHub 연결 조회가 회당 30초 가까이) | 기동 시간은 호스트 상태의 신호다. 잡 수명 지표에 넣고 배치 순위에도 쓴다 |

## 남은 열린 질문

1. OS 별 사람 활동 신호: macOS(HID idle), Linux(logind `IdleHint` / X·Wayland), Windows(`GetLastInputInfo` 는 사용자 세션 안에서만 읽힌다).
2. 잡 안 docker: `kata` 와 `microvm` 중 무엇이 e2e 클래스에 맞는가 (기동 시간, 캐시 마운트, 메모리).
3. 미룬 수요를 오래 들고 있을 때 GitHub 쪽 한도(큐 24시간 등)와, 3번 재배정이 끝난 잡을 통계 말고 다시 확인할 방법.
