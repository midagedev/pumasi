# 실물로 확인된 것

[design.md](design.md) 의 각 동작을 어디까지 확인했는지 적는다. 수준은 셋이다.

- **실물** — 실제 호스트와 실제 GitHub 잡으로 봤다.
- **유닛** — 테스트로만 봤다(가짜 에이전트·가짜 GitHub, 또는 드라이버 수준).
- **미확인** — 코드는 있으나 돌려 보지 않았다.

실물 호스트: Linux 서버(Docker), WSL2 Ubuntu 데스크톱(systemd 255), Apple silicon 노트북, Windows 11 x64 데스크톱. 2026-11-24 ~ 26.

## 배치와 장부

| 동작 | 수준 | 본 것 |
|---|---|---|
| 몰아 넣어도 러너 크기의 합이 호스트 예산을 넘지 않는다 | 실물 | 호스트 2대, 잡 39개: 스냅샷 622개에서 합의 최댓값이 예산과 같거나 아래 |
| 미룬 잡이 자리가 나면 새 메시지 없이 시작된다 | 실물 | 예산을 줄여 미루고 되돌리자 다음 평가에서 배치(처음 본 뒤 17분) |
| 잡마다 cpu 초·메모리 peak·기동 시간·결과가 장부에 남는다 | 실물 | `/metrics` 와 대조: peak 일치, cpu 0.6초 이내 |
| 러너 기동(배치 → Listening) | 실물 | 컨테이너 4.3 ~ 11초(붐비는 서버는 2 ~ 5배 흔들린다), native Linux 약 3초, Mac 3.4 ~ 5.4초, Windows 약 3초, `docker: inside` 5.0 ~ 5.6초 |
| busy 를 GitHub 메시지보다 먼저 안다 | 실물 | 잡 10개 모두 30초쯤 먼저 |
| `hosts`: 묶인 호스트에만, 차면 기다린다 | 실물(한 대) / 유닛 | 호스트가 둘일 때 "빈 다른 호스트로 가지 않는다"는 유닛뿐 |
| `reserve` | 실물 | 예산 6cpu: 다른 클래스 잡 7개 중 4개만 뜨고 3개는 `reserved for …` 로 대기, 예약 클래스 잡은 바로 뜸 |
| `mounts`: 경로 있는 호스트에만, 같은 경로로 읽고 쓰기, 없으면 대기 사유 | 실물 | 작업 디렉터리가 mount 안일 때 hard link 가 된다(링크 수 2) |
| 설정의 모르는 키·같은 이름의 클래스 거부 | 유닛 | |
| 대시보드 24시간 추이에 스스로 붙은 호스트 포함 | 유닛 | |

## 호스트가 붙는 법

| 동작 | 수준 | 본 것 |
|---|---|---|
| 초대 줄로 붙기 | 실물 | Linux·Mac·Windows(ssh 세션에서) |
| `hosts: - ssh:` 를 controller 가 대신 붙이기 | 실물(Linux, WSL) | `shell: wsl -e sh -s` 인 호스트도 붙었고(11-26), native 러너가 user scope 로 잡 하나를 돌렸다 |
| 닿지 못하는 `listen` 주소 | 실물 | `up` 이 `cannot reach this controller at …` 로 끝남 |
| 모르는 토큰, 한 번 쓴 토큰, 틀린 지문 | 실물 | `FAIL token` / `FAIL token` / curl 이 pinned key 불일치로 끝남. 실패 뒤 호스트에 남는 것 없음 |
| 토큰이 프로세스 인자·파일에 남지 않는다 | 실물 | Linux·Mac |
| `--uses`, `--ttl` | 유닛 | |
| 설정의 호스트 이름 예약, 이름에 묶인 토큰, Host 헤더 확인 | 유닛 | |
| docker 가 없는 호스트 | 실물 | `FAIL docker: not installed` (길을 줄이기 전의 판에서) |
| join 기본 종류(Mac desktop) | 실물 | Linux 의 화면 세션 판정은 유닛 |
| 판이 다른 controller → 호스트가 스스로 바꾼다 | 실물(Linux·Mac) / 미확인(Windows) | 같은 pid 로 새 판, 도는 러너 그대로 |
| 에이전트를 죽임 | 실물 | Linux 2초, Mac 1초 안, Windows 5초 안에 다시 뜨고 러너를 이어받음 |
| `leave`, `down` 뒤 남는 것 없음 | 실물 | Linux(컨테이너, native scope), Mac, Windows. 안 붙어 있는 ssh 호스트를 `down` 이 ssh 로 치우는 것 포함 |
| 러너가 떠 있으면 `leave`·`down` 거부 | 실물 | |

## 러너

| 동작 | 수준 | 본 것 |
|---|---|---|
| 메모리 한도(컨테이너, native Linux, `docker: inside`) | 실물 | 넘은 프로세스만 137 로 죽고 잡은 실패로 보고, OOM 사건, peak = 한도 |
| Windows 메모리 한도 | 실물 | 할당이 거절되고 사건, peak = 한도 |
| Windows CPU 한도의 강제 | 미확인 | |
| Mac: 클래스를 넘은 러너 | 실물 | `ran over its class` 사건 한 번, 잡은 성공 |
| 잡이 남긴 자식 정리 | 실물 | Linux native(`setsid` 포함), Mac(`setsid` 포함), Windows |
| native Linux: 호스트의 경로·docker 사용, 잡 사이에 파일 넘기기 | 실물 | |
| native Linux: cgroup 위임이 없으면 띄우지 않고 이유를 말한다 | 유닛 | 드라이버 테스트 |
| `docker: inside`: `docker run`·`services:`·compose 스택 | 실물 | 끝난 뒤 호스트 docker 의 컨테이너·볼륨·네트워크가 잡 전과 같다 |
| 러너 루트(홈 밖) | 실물(Linux·Windows) / 유닛(Mac) | Mac 은 디렉터리 만들고 clone 하는 테스트까지 |
| 함께 쓰는 배포판을 읽기 전용으로, 손댄 것은 새로 받기 | 실물 | Linux·Windows |
| 지우지 못한 파일 → `.trash/` 와 `leftover` 사건 | 실물 | 잡이 컨테이너로 남긴 root 의 디렉터리 |
| 클래스 `env`·`path`·`{runner}` | 실물(Mac native) / 유닛 | 잡에서 찍은 값이 설정대로, `path` 는 에이전트의 `PATH` 앞에. 초대 줄을 실행한 셸의 변수는 잡에 없다. 컨테이너와 Linux native 는 유닛뿐 |
| 넓은 라벨 경고 | 실물 | `runs-on: [self-hosted]` 만 적은 잡이 오자 잡 이름과 함께 경고 한 줄 |
| `image` 가 `:latest` 일 때 native 러너 | 미확인 | 코드로는 판 번호를 못 읽어 뜨지 않는다 |
| Mac 수치(cpu 초, 메모리 peak) | 실물 | `ps`·`top` 과 일치 |
| Windows: 책상에 로그온한 세션(입력 감지, 로그온 때 시작, 창) | 미확인 | ssh 세션에서만 붙여 봤다 |
| Windows fast startup 뒤의 판정(`died`) | 미확인 | boot id 값이 실행마다 같은 것까지만 |

## 등록 러너 (`slots:`)

Apple silicon 노트북 한 대(예산 4cpu/6G, server 로 붙임)와 개인 레포, 러너 2.338.2. 2026-11-26. Linux 는 아래 표의 표시대로.

| 동작 | 수준 | 본 것 |
|---|---|---|
| 등록 토큰 하나로 `pumasi register` → 호스트가 빈 슬롯마다 등록 | 실물 | 클래스 2개·슬롯 3개: 28초 뒤 GitHub 에 3개가 online, 라벨은 클래스의 것. 토큰은 stdin 으로 |
| `up` 이 GitHub 토큰 없이 뜨고 잡이 돈다 | 실물 | 환경에 토큰 없이 띄움. 잡 71개 모두 `success` |
| 잡마다 깨끗한 사본, 끝나면 작업 디렉터리째 지운다 | 실물 | 잡마다 러너 디렉터리 이름이 다르고(`RUNNER_WORKSPACE` 가 그 안), 끝난 뒤 러너 루트에 남는 것이 없다 |
| 듣는 수가 예산을 따른다 | 실물 | 2cpu×2 슬롯과 4cpu×1 슬롯: 2cpu 잡 하나가 돌면 4cpu 슬롯이 `off — no room: 2.0cpu free …`, 끝나면 다시 듣는다 |
| controller 를 죽여도 새 잡이 돌고 끝난다 | 실물 | 죽인 뒤 보낸 잡 4개가 두 바퀴로 `success`(잡이 끝난 같은 초에 다음 사본이 듣기 시작). 다시 띄우자 그 잡들이 장부에 들어옴 |
| controller 가 새 판으로 바뀌어 에이전트가 갈아타도 듣던 러너가 남는다 | 실물 | 갈아탄 뒤에도 같은 사본이 듣고 있었다 |
| 두 클래스가 한 예산을 두고 다툴 때 잡이 죽지 않는다 | 실물 | 2cpu×2 슬롯과 4cpu×1 슬롯, 예산 4cpu. 양쪽에 잡을 쌓아 두고 세 번: ① 10개 — 죽은 잡 없음(아래 「굶는다」와 세션 충돌을 여기서 봄) ② 12개 — 죽은 잡 없음, 그러나 **세 러너가 동시에 잡을 받아 8cpu 가 두 번**(각 30초, `over-budget` 사건이 열리고 닫힘) ③ 차례로 듣게 고친 뒤 12개 — 죽은 잡 없음, 예산 넘김 0, 세션 충돌 0. 놀고 있는 러너를 멈춘 것은 합쳐 수십 번(첫 40분에 22번) |
| 놀고 있는 러너를 멈춘 뒤 그 등록이 다시 듣는다 | 실물 | 듣는 중에 멈춘 것은 매번 다음 사본이 바로 들었다. **뜨는 중(세션을 여는 도중)에 멈춘 것은 한 번 세션이 GitHub 에 남아** 2분 넘게(13분 안에 풀림) `A session for this runner already exists` 로 못 들었다 → 뜨는 중인 러너는 듣기 시작한 뒤에 멈추도록 고침 |
| 어느 길도 잡을 든(받는 중인) 슬롯을 멈추지 않는다 | 유닛 | `TestNoPathStopsASlotThatHoldsAJob`: 클래스·예산·닫힘·내놓기 조합 4천여 가지 |
| `pumasi adopt`: 등록된 고정 러너를 넘겨받아 잡을 돌린다 | 실물 | 평소대로 등록해 잡을 돌리던 러너. 듣고 있는 동안은 거부(pid 와 함께), 멈춘 뒤 넘겨받음. 잡이 같은 이름·id 의 러너로 돌았고 클래스 `env`·`path`(옛 `.path` 앞에)가 들어감. 원본의 `.runner`·`.credentials` 해시 그대로 |
| 되돌리기: `pumasi release` 뒤 옛 러너를 다시 띄운다 | 실물 | 내놓은 직후 옛 디렉터리의 `run.sh` 가 바로 듣고 잡을 받음 |
| 호스트가 스스로 닫는다(메모리 압박) | 실물 | 노트북의 memory pressure 가 warning 이 되자 놀고 있던 러너 3개를 멈추고 `off — memory pressure (warning)`, 풀리자 다시 들음. 호스트 로그에 사유가 남는다 |
| `leave`·`down`: 자격을 남기거나 등록까지 지운다 | 실물 | Mac 에서 토큰 없이 `leave`: 자격 셋이 `~/pumasi-runners/<이름>/` 으로 가고 무엇이 GitHub 에 남았는지 러너마다 말함, 캐시·러너 루트·서비스·바이너리 없음. Linux 에서 등록 토큰과 함께 `down`: 등록 2개가 GitHub 에서 사라지고 호스트에 남은 것 없음. `leave` 가 남긴 파일을 `adopt` 로 되받는 것은 미확인 |
| 서버 먼저(desktop 의 클래스를 닫음), mount 없는 호스트, 격리 | 유닛 | `TestSlotPlans` |
| 슬롯 저장소: 원본을 안 건드림, 사본의 설정(`_work`, `disableUpdate`, 큰 id 보존, BOM) | 유닛 | |
| Linux 호스트의 슬롯 | 실물(한 대) | WSL2 Ubuntu 를 2cpu/2G 로 붙임(초대 줄로 한 번, `hosts:` 의 ssh 로 한 번). 등록 2개, 잡 6개 `success`(3개는 controller 를 죽인 뒤), 잡마다 `/var/tmp/pumasi-<uid>/` 아래 새 디렉터리와 systemd scope, 클래스 `env` 가 들어감. 놀고 있는 러너를 멈추는 길(클래스 둘의 다툼)과 adopt 는 Linux 에서 안 돌려 봄 |
| `maxDuration`, 잡 도중 러너가 죽음(`runner-died`), `failing` 뒤 다시 해 보기, `over-budget` 사건 | 미확인 | 코드만 있다. `failing` 과 사건 열림은 위 세션 충돌 때 실물로 한 번 봄 |
| Windows 호스트 | 없음 | `slots:` 와 `os: windows` 를 같이 적으면 `up` 이 거부한다 |
| 브로커를 거치지 않는 러너(GHES 등 `useV2Flow` 가 꺼진 등록) | 미확인 | 놀고 있어도 멈추지 않게 해 두었다. 그런 서버로 돌려 보지 않았다 |

**큰 클래스가 굶는다.** 2cpu 클래스와 4cpu 클래스가 4cpu 예산을 나눌 때 양쪽에 잡이 계속 쌓여 있으면, 2cpu 잡이 끝난 자리를 2cpu 잡이 다시 채워 4cpu 슬롯은 자리를 얻지 못했다
(2cpu 잡 6개가 다 끝날 때까지 4cpu 잡 4개가 기다림). 차례로 듣게 한 뒤에도 2cpu 잡 둘이 엇갈려 끝나는 동안은 4cpu 자리가 한 번에 비지 않아 같았다(2cpu 잡 8개 뒤에 4cpu 잡 4개).
큰 클래스에 `reserve: 1` 을 주면 그 자리를 작은 클래스가 못 쓴다 — 유닛으로만 봤다.

**차례의 값.** 같은 자리를 보는 클래스는 한꺼번에 듣지 않는다(잡을 가장 오래 못 받은 클래스가 먼저, 나머지는 그 러너가 8초 동안 잡 없이 들은 뒤). 그래서 방금 잡을 끝낸 클래스의 다음 잡은
다른 클래스에 잡이 없어도 15 ~ 25초 늦게 시작한다(4cpu 잡 사이 간격 약 24 ~ 38초, 러너 기동 포함).

## 사람 우선

| 동작 | 수준 | 본 것 |
|---|---|---|
| Mac: 입력 시각 읽기, 러너를 background 로 내리고 되돌리기 | 실물 | `ps` 의 PRI 31 ↔ 4, 그 뒤에 띄운 자식도 |
| 15분 규칙(desktop 만 닫힌다) | 유닛 | |
| Linux 화면 세션, Windows 입력 시각 | 유닛 / 미확인 | 실물 Linux 데스크톱 없음 |

## 견고함

`make chaos` 의 13행은 초대 줄로 붙은 Linux 서버에서 지금의 길(호스트가 건다)로 모두 통과했다. `controller_kill` 행은 처음에 알림 누락으로 실패해 고친 뒤 그 행만 다시 돌렸다.

| 동작 | 수준 | 본 것 |
|---|---|---|
| **busy 러너는 어떤 경로로도 지워지지 않는다**(`maxDuration` 제외) | 유닛 + 실물 | 경로별 표 테스트. 실제 GitHub 이 busy 러너의 등록 삭제를 거절하는 것은 실물 |
| 등록 삭제를 못 물으면 지우지 않는다, 늦게 뜬 러너, 호스트가 떠난 순간의 launch | 유닛 | |
| controller 를 죽임(kill -9) | 실물 | 다시 떠서 `adopted 2 runners`, 잡 성공 |
| controller 둘 | 실물 | 두 번째가 첫째의 pid 를 말하고 끝남 |
| 에이전트 죽음·재시작 | 실물 | 러너 그대로, cpu 초가 이어진다 |
| 호스트 연결 끊김(에이전트를 60초 멈춤) | 실물 | `unreachable for … (keepalive timeout)` 사건이 열리고 닫힘, 잡 성공. 실제 네트워크 차단은 해 보지 않았다 |
| 호스트 재부팅 | 실물(흉내) | boot id 를 바꾸고 러너를 죽여서. 사건 + 잃은 잡, `rerun` 으로 다시 돌아 성공. 진짜 재부팅은 미확인 |
| 잡 도중 러너가 죽음 | 실물 | 사건, GitHub 이 약 10분 뒤 닫은 다음 다시 돌려 성공 |
| 러너가 못 뜸 → 격리 | 실물 | 연속 3번 뒤 격리 사건 |
| 메모리 한도 사건, 다시 돌리지 않음 | 실물 | |
| `maxDuration` | 실물 | 넘는 순간 사건, 러너 정지, 다시 돌리지 않음 |
| 디스크 부족 | 실물 | 임계를 올려서: 사건, 새 배치 중단 |
| 장부가 깨짐 | 실물 | 옆으로 치우고 빈 장부로 시작 |
| 틀린 GitHub 토큰 | 실물 | `up` 이 그 이름으로 끝남 |
| **45분 잡** | 실물 | Linux(컨테이너)와 Mac 각각: 도는 동안 controller kill -9, 판 교체 재시작, 에이전트 60초 멈춤, controller 머신 load 120 — 잡 성공 |
| controller 를 Mac LaunchAgent 로 | 실물 | nice 0, kill -9 뒤 launchd 가 다시 띄워 이어받음. 부하 걸린 Mac 에서 수 시간은 미확인 |
| 남의 부하로 스래싱하는 호스트(load 200, swap 가득) | 실물 | 에이전트가 10초 넘게 못 돌아 연결이 끊겼다 붙음. 사건이 이유와 함께, 잡 성공 |
| 잡이 오래 기다림(`waitAlert`), GitHub API 실패, 주인 없는 러너 | 유닛 | |

## GitHub 쪽에서 잰 것

| | 값 |
|---|---|
| `runs-on` 매칭 | 잡의 라벨이 전부 scale set 라벨 안에 있어야 온다. `self-hosted` 는 scale set 에 넣어야 한다 |
| 시험용 라벨 하나만 단 scale set(`labels: [trial]`, `runs-on: [trial]`) | 온다. 기존 라벨을 건드리지 않고 남의 워크플로우를 브랜치에서 `runs-on` 만 바꿔 돌려 볼 수 있다(조직 레포의 gradle 잡·하드웨어 WebGPU 브라우저 e2e 를 Mac 호스트에서 이렇게 확인, 11-26) |
| 고정 러너의 라벨을 단 scale set(`labels: [self-hosted, macOS, ARM64, ci-pool]`) | 만들어진다(기본 라벨도 거절 없음). 전체 라벨·부분집합·대소문자가 다른 `runs-on` 모두 scale set 으로 온다 |
| 같은 라벨의 일반 등록 러너가 idle 로 같이 있을 때 | 잡 14개 모두 scale set 으로 갔다(고정 러너 0개). scale set 과 고정 러너 중 어느 쪽을 먼저 만들었는지와 무관 |
| scale set 은 있는데 `up` 이 꺼져 있을 때 | 고정 러너가 idle 이어도 잡은 `queued` 로 기다린다(90초 관찰) |
| scale set 을 지운 뒤 | 새 잡은 고정 러너가 바로 받는다. 지울 때 기다리던 잡은 약 3분 뒤 고정 러너로 갔다 |
| 러너 없이 대기 | 5분 간격으로 세 번 다시 배정된 뒤 메시지가 끊긴다. 잡은 계속 `queued` |
| 잡 도중 러너가 사라짐 | 약 9분 40초 뒤 잡이 `failure`. 그동안 등록은 지울 수 없다. 자동 재시도 없음 |
| 폐기된 러너 판 | 접속 직후 종료 코드 0 으로 끝난다 |
| `JobStarted` 메시지 | 러너가 잡을 잡은 뒤 30 ~ 50초 늦다 |
| 일반 등록 러너의 `run.sh --once` | 잡 하나를 받고 종료 코드 0 으로 끝난다. 등록은 남는다(폐기 예정 경고만 나온다) |
| 등록의 자격 파일 셋(`.runner`·`.credentials`·`.credentials_rsaparams`)을 새 러너 디렉터리에 복사 | 그대로 듣고 잡을 받는다. 같은 등록으로 둘이 들으면 둘째가 `A session for this runner already exists` 를 내며 다시 시도한다 |
| 러너가 잡을 받는 순서 | 진단 로그에 `Acknowledging runner request` → 약 2초 뒤 `Running job:` |
| 놀고 있는 리스너에 SIGINT | 1초 안에 끝나고 세션을 지운다. `Acknowledging` 순간에 보내도 잡은 큐에 남아, 2초 뒤에 듣기 시작한 다른 러너가 받았다 |
| `Running job:` 뒤에 SIGINT | 잡이 죽는다. `in_progress` 로 남다가 약 10분 뒤 `failure`. 그동안 그 등록은 지울 수 없다 |
| `.runner` 에만 `"disableUpdate": true` 를 적은 사본 | 2.337.0 러너가 잡을 받을 때 스스로 올리지 않았다(안 적으면 128MB 를 받아 약 17초 걸려 올린 뒤 받는다) |
| 등록 토큰으로 `config.sh remove` | 된다(삭제 토큰이 따로 필요하지 않다) |
| 새 등록의 첫 세션 | 설정 갱신 메시지를 받아 세션을 한 번 다시 연다. 한 번은 첫 요청이 100초 동안 답 없이 걸려 그동안 그 러너가 잡을 못 받았다(3개 중 1개, 다시 안 나옴) |
