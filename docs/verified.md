# 실물로 확인된 것

[design.md](design.md) 의 각 동작을 어디까지 확인했는지 적는다. 수준은 셋이다.

- **실물** — 실제 호스트와 실제 GitHub 잡으로 봤다.
- **유닛** — 테스트로만 봤다(가짜 에이전트·가짜 GitHub, 또는 드라이버 수준).
- **미확인** — 코드는 있으나 돌려 보지 않았다.

실물 호스트: Linux 서버(Docker), WSL2 Ubuntu 데스크톱(systemd 255), Apple silicon 노트북, Windows 11 x64 데스크톱. 2026-11-24 ~ 25.

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
| `{slot}`: 같은 클래스의 동시 러너가 서로 다른 번호를 받고, 풀린 번호를 다음 러너가 다시 쓴다 | 실물(Mac native) / 유닛 | 예산 3cpu 에 1cpu 잡 4개: 동시에 뜬 셋이 1·2·3, 넷째는 먼저 풀린 2 를 다시 받음. 잡의 env 에 번호가 들어가고 번호가 든 디렉터리 셋이 만들어짐(그 아래는 안 만듦). 도는 중에 controller 를 다시 띄워도 `status` 의 번호 그대로. 에이전트 재시작 뒤 번호 유지와 「두 러너가 같은 번호를 갖지 않는다」(동시 시작·삭제 200번)는 유닛. 컨테이너 러너·Linux·Windows 는 유닛뿐 |

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
