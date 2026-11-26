# 실물로 확인된 것

[design.md](design.md) 의 각 동작을 어디까지 확인했는지 적는다. 수준은 셋이다.

- **실물** — 실제 호스트와 실제 GitHub 잡으로 봤다.
- **유닛** — 테스트로만 봤다(가짜 에이전트·가짜 GitHub, 또는 드라이버 수준).
- **미확인** — 코드는 있으나 돌려 보지 않았다.

실물 호스트: Linux 서버(Docker), WSL2 Ubuntu 데스크톱(systemd 255), Apple silicon 노트북, Windows 11 x64 데스크톱. 2026-11-24 ~ 25.
「크기」 절의 실물은 2026-11-26, 노트북과 그 위의 Linux VM(aarch64, 8cpu/32G, Docker, cgroup v2)에서 봤다.

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

## 크기 (사람이 숫자를 고치지 않는 배치)

설정에는 숫자가 없었다(`classes: [{name: grow}, …]`, 호스트는 상한 없이 가입).

| 동작 | 수준 | 본 것 |
|---|---|---|
| 처음 보는 잡은 클래스의 첫 추정으로 놓인다 | 실물 | `takes 2.0cpu/4.0G of the free 6.4cpu/22.7G for hold (1) (the class's first estimate, …)` |
| 워크플로우째로 처음인 잡 여럿이 한꺼번에 오면 나란히 놓지 않는다 | 실물 + 유닛 | VM(빌려줄 수 있는 메모리 22.9G), 빈 장부, 5000MiB 를 쥐는 잡 3개를 동시에. 하나만 놓임: `takes 0.5cpu/4.0G … (the class's first estimate; nothing is measured of this job or its workflow yet); holds 12.0G while it is watched`, 나머지 둘은 `vm1 6.0cpu/10.9G: it watches 1 unmeasured job there and takes the next with 12.0G free or when that one has shown its size` 로 대기. 첫 잡이 3분을 돈 뒤(시작 12:09 → 12:12:30) 둘이 `takes 0.5cpu/6.0G … (no measured run of this job yet; sized like the largest known of its workflow's other jobs, 0.0cpu/5.0G)` 로 놓임. 3배·3분이라는 값이 실제 JVM 빌드에 맞는지는 보지 않았다(규칙의 동작만) |
| 같은 워크플로우의 끝난 표본에서 빌리기, 3배가 안 들어가는 작은 호스트도 하나는 받기, 재 본 잡은 지켜보는 잡 옆에 평소대로 놓이기 | 유닛 | `internal/decide/blind_test.go`, `internal/controller/size_test.go`, `internal/size` |
| 메모리가 커지는 잡을 추정이 따라간다 | 실물(Linux 컨테이너, Mac native) | 300 → 600 → 1200 → 2400MiB 를 쥐는 잡 4번: 추정 4.0G(첫) → 0.6G → 0.9G → 1.8G → 3.2G. Mac: 700 → 1400MiB 에서 1.1G → 1.9G |
| 추정보다 큰 잡이 죽지 않고 끝난다 | 실물(Linux 컨테이너, Mac native) / 드라이버 테스트(native Linux, 실제 systemd) | 추정 576M 에 746M, 960M 에 1476M, 1792M 에 2767M 을 쓴 실행이 모두 success, OOM 0. Mac: 추정 1088M 에 1584M. native Linux 는 VM 에서 드라이버 테스트로: 추정 32M 인 scope 가 300M 을 쥐고 종료 코드 그대로 끝남, `memory.max` = `max` |
| 페이지 캐시는 크기에 안 들어간다 | 실물(Linux 컨테이너) | 300MiB 를 쥐고 1.5G 파일을 쓰고 읽은 잡: cgroup peak 1950M, 장부의 작업 메모리 449M, 다음 추정 0.6G |
| 바깥 부하가 있으면 새 러너를 미루고, 빠지면 놓는다 | 실물(Linux) | VM 에서 pumasi 밖의 프로세스가 4G 를 쥐자 자리가 22.9G → 18.4G: 첫 추정 20G 인 잡이 `no host with 2.0cpu/20.0G free (vm1 6.5cpu/18.6G)` 로 대기, 프로세스를 죽이자 다음 평가에서 `free 6.4cpu/22.7G` 로 시작(대기 기록 16초 뒤) |
| 재 본 잡은 클래스의 숫자보다 잰 크기를 따른다 | 실물 | 첫 추정 20G 인 클래스로 간 잡이, 다른 클래스에서 잰 적 있는 잡이라 3.2G 로 놓였다(열쇠에 클래스가 없다) |
| 클래스 `limit` 에 걸린 잡: 사건, 그리고 다음 추정이 한도 위로 | 실물(Linux 컨테이너) | `limit: {memory: 1G}` 에 1500MiB 잡: 잡 failure, `hit class tight's memory limit (1.0G)` 사건, 추정 0.2G → 1.5G. 한도에 걸린 실행을 한도 이상으로 치기 전의 판에서는 표본이 96M(죽기 전 마지막 샘플)이라 추정이 내려갔다 — 고친 뒤 다시 본 것이 위의 값 |
| 호스트를 지키는 한도(머신의 80%)에 실제로 걸리는 것 | 미확인 | 걸리게 하려면 VM 메모리의 80%(25G)를 쥐어야 해서 하지 않았다. 한도 값의 계산은 유닛, 한도가 걸렸을 때의 동작은 위 `limit` 과 같은 경로 |
| 추정 계산, 러너가 쥔 것 = max(추정, 실측), 자리 계산, 메모리 압박 닫힘 | 유닛 | `internal/size`, `internal/decide` 의 표 테스트 |
| 크기를 이유로 멈추는 러너가 없다 | 유닛 | 추정의 1 ~ 1000배, 압박 0·99, 상태 셋의 조합 전부에서 Stop 0 |
| 숫자 없는 설정이 통한다, `limit` 파싱 | 유닛 + 실물 | 실물은 위 설정 |
| `status` 의 EST·NOW·JOB SIZE, 로그의 `estimate` 줄 | 실물 | `EST 2.0cpu/4.0G (first)  NOW 0.0cpu/0.4G`, `hold (1)  labels.yml  0.4cpu/3.2G  0.3cpu/2.7G  0.3cpu/2.7G  4` |
| 대시보드의 Job sizes 표와 러너의 now/est 칸 | 미확인 | 코드만. 화면을 열어 보지 않았다 |
| Windows 에서의 추정·실측 | 미확인 | 빌드와 vet 만 |
| Linux 메모리 압박(PSI 10%)으로 실제로 닫히는 것 | 미확인 | 유닛뿐 |
| native Linux 러너로 실제 잡 | 미확인(이번) | VM 에 libicu 가 없어 러너가 뜨지 못한다 |
| 러너 라이브러리가 없는 호스트에는 native 클래스를 놓지 않는다 | 실물 | libicu 없는 VM: 붙을 때 `note  driver: native classes will not be placed here: the runner cannot start here, it needs libicu (…)`, native 클래스의 잡은 러너를 띄워 보지 않고 대기하며 `WHY` 에 같은 말. 라이브러리가 다 있는 호스트에서 `ok` 가 나오는 쪽은 유닛(`ldconfig -p` 출력 판독)뿐 |
| 서비스 계정의 `user@<UID>.service` drop-in `BindPaths=` 를 에이전트와 scope 러너가 물려받는다 | 실물 | 11-26, linger 켠 서비스 계정에서 |

## 줄 (잡 우선순위)

| 동작 | 수준 | 본 것 |
|---|---|---|
| high 클래스의 잡이 먼저 온 다른 클래스의 잡보다 먼저 러너를 받는다, low 는 나중 | 실물(Linux VM, 한 번에 러너 하나) | build 3 · nightly 1 · deploy 1 을 이 순서로 쌓고 `{class: deploy} high`, `{job: hold (21)} low`: 시작 순서 build → **deploy** → build → build → nightly |
| 도는 중에 규칙을 바꾸면 바로 적용된다 | 실물 | 잡이 쌓인 뒤 파일에 규칙을 넣자 9초 안에 `priority: 2 rules read again …`, `status` 의 줄이 build·nightly·deploy → deploy·build·nightly |
| 같은 클래스에서 high 잡 앞의 잡이 함께 당겨진다 | 실물 | 먼저 온 nightly 잡보다 build 의 두 잡(뒤의 것이 high)이 먼저 나감. `LINE` 에 `ahead of hold (42) in its class, which is high by rule 3` |
| `pumasi priority`, `status` 의 LINE 칸 | 실물 | |
| 30분 넘게 기다린 잡이 high 보다 앞, low 도 30분 뒤에는 앞(굶지 않음) | 유닛 | 표 테스트. 실물로 30분을 기다려 보지 않았다 |
| 기다리는 높은 잡의 호스트를 낮은 잡에게서 지키기, 같은 등급의 빈틈 채우기 | 유닛 | |
| 도는 잡을 건드리지 않는다 | 유닛 + 실물 | 실물에서 규칙을 바꿀 때 돌던 잡은 끝까지 돌았다 |
| 틀린 규칙 파일은 전의 규칙을 유지 | 유닛 | |
| 대시보드 Waiting 의 Line 칸 | 미확인 | 화면을 열어 보지 않았다 |

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
| 한 호스트에 에이전트가 둘이 된 뒤의 `down` (11-26) | 실물(Linux VM) | 유닛으로 붙인 뒤 사용자 systemd 에 닿지 못하는 ssh 세션에서 다시 붙여 에이전트 둘(유닛 + 재시작 루프)을 만들고 `down`: 디렉터리·바이너리·유닛이 없어지고, 남은 에이전트는 `join.json` 이 없어진 것을 보고 10초 안에 스스로 끝났다 |
| `XDG_RUNTIME_DIR` 없는 ssh 세션에서 붙기 (11-26) | 실물(Linux VM) | 전에는 `systemctl --user` 가 실패해 재시작 루프로 붙었다. 이제 `/run/user/<uid>` 의 사용자 systemd 를 찾아 유닛으로 붙고 `down` 뒤 남는 것 없음 |
| 떠나는 에이전트가 디렉터리를 지운 뒤 아무것도 쓰지 않는다 | 유닛 | `Forget` 뒤의 sample 이 기록 파일을 다시 만들지 않는다 |
| WSL 호스트에서 `down` 뒤 `join.state`·`runners.json` 이 다시 생기던 것 | 미확인 | 그 증상 자체는 Linux VM 에서(유닛·루프 방식 모두) 재현되지 않았다. 위 세 가지(둘째 에이전트, 세션 변수, 떠나는 중의 쓰기)를 막았고 WSL 에서는 다시 보지 않았다 |

## 러너

| 동작 | 수준 | 본 것 |
|---|---|---|
| 떠날 때 러너 이미지: pumasi 가 받은 것만 지운다 | 실물 | VM. 받은 이미지 → `leave` 뒤 없음(이미지 수가 붙기 전과 같음). 붙기 전부터 있던 이미지 → 기록 없음, `leave` 뒤 그대로. 받은 이미지를 남의 컨테이너가 쓰는 중 → `down` 이 `kept: image … stays: … container … is using its referenced image` 라 말하고 남김 |
| 메모리 한도(컨테이너, native Linux, `docker: inside`) | 실물 | 넘은 프로세스만 137 로 죽고 잡은 실패로 보고, OOM 사건, peak = 한도. 한도가 클래스 크기였던 판에서 본 것이고, 지금 판(`limit`)은 컨테이너만 다시 봤다 |
| Windows 메모리 한도 | 실물 | 할당이 거절되고 사건, peak = 한도 |
| Windows CPU 한도의 강제 | 미확인 | |
| Mac: 추정을 넘은 러너 | 실물 | 잡은 성공, 사건 없음(전 판의 `ran over its class` 사건은 없앴다) |
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
| `pumasi service install` — Mac (11-26) | 실물 | LaunchAgent 로 뜸. kill -9 → 2초 뒤 새 pid. 잡이 도는 중 kill -9: `controller is back: was down 9s, adopted 1 runners`, 그 잡 성공, 죽어 있는 동안 건 잡도 세션이 다시 열린 뒤 돌아 성공. `SIGTERM` 뒤에도 다시 뜸(5초). plist·프로세스 환경에 토큰 없음, 토큰 파일 600 |
| `pumasi service install` — Linux (11-26) | 실물(VM) | systemd user unit 으로 뜸. kill -9 → 5초 뒤 새 pid(`started again 1 times`), 77초 뒤 세션이 다시 열림. 유닛·프로세스 환경에 토큰 없음, 토큰 파일 600. 호스트를 붙여 잡을 돌리는 것은 Mac 쪽에서만 봤다 |
| `service remove` | 실물(Mac, Linux VM) | controller 가 끝난 뒤 돌아온다. 유닛/plist·토큰 파일이 없어지고 다시 뜨지 않는다. 그 뒤 `down` 으로 나머지를 지움 |
| 서비스가 도는 동안의 `up`·`down` | 실물 | 거절하고 `service remove` 를 가리킨다 |
| 토큰 파일: 환경 변수 우선, 남이 읽을 수 있으면 거절 | 유닛 | |
| controller 부재를 호스트가 알림 (11-26) | 실물(Linux VM + Mac 호스트, `away: 1m`) | controller 를 kill -9: 첫째 호스트가 1분 3초에, 둘째가 2분 2초에 웹훅으로 한 번씩, 그 뒤 4분까지 더 없음. 다시 띄우자 두 호스트가 해제를 한 번씩. 35초 멈췄다 띄운 경우와 `down` 뒤에는 아무것도 안 감. 웹훅 파일 600 |
| 부재 알림: 둘만 고르기·승계, 한 번만, 에이전트 재시작, 못 전한 부재, 둘째는 두 배 | 유닛 | 호스트 셋 이상에서 셋째가 조용한 것, 말하던 호스트가 끊겼을 때의 승계는 유닛만 |
| 부재 알림: 호스트의 네트워크만 끊긴 경우, 자다 깬 노트북 호스트, 기본값 5분 그대로 | 미확인 | |
| 자격 노출 경고 (11-26) | 실물(Mac) | controller 와 같은 Mac·같은 사용자로 붙인 호스트: `up` 로그와 `status` 의 `WARNINGS` 에 클래스·이유. 같은 controller 에 붙은 Linux VM 호스트는 경고 없음. `jobsMayReadCredentials: true` 로 다시 띄우면 없음. 같은 사용자가 `ps eww` 로 controller 의 토큰 변수를 볼 수 있는 것도 확인 |
| 자격 노출 판정: Linux 의 native·docker 사용자·`docker: inside`·상태 디렉터리 mount, 다른 사용자의 Mac | 유닛 | 표 테스트. Linux 에서 controller 와 호스트를 한 머신에 둔 실물은 보지 않았다 |
| 자격 노출 경고의 대시보드 칸 | 미확인 | 화면을 열어 보지 않았다 |
| GitHub App 인증 (11-26) | 유닛 + 실물 일부 | 유닛: App 의 키로 서명한 JWT 로 설치 토큰을 받고 만료 5분 전에 다시 받는다(가짜 GitHub), PKCS#1·PKCS#8 키, 설정·환경 변수·둘 다 준 경우·600 이 아닌 키. 실물: github.com 에 없는 App(id 1)으로 `up` 을 띄우자 `POST /app/installations/1/access_tokens` 가 `404 Integration not found` 로 끝남 — 서명과 요청까지는 실제 GitHub 에 닿았다. **진짜 App 으로 scale set 을 만들고 잡을 돌리는 것은 미확인**(App 을 만들려면 브라우저에서 계정 주인이 해야 한다) |
| 토큰(PAT) 경로가 App 추가 뒤에도 그대로 | 실물 | `up` → 세션 → `down` |
| 권한 표의 Actions: Read / Read and write 만으로 run 조회·다시 돌리기가 되는지 | 미확인 | classic PAT(`repo`)로만 돌려 봤다. 러너 등록 권한은 GitHub 문서의 표를 옮긴 것 |
| 서비스로 띄운 controller 의 **재부팅** 뒤 | 미확인 | 노트북과 다른 일에 쓰이는 VM 뿐이라 재부팅하지 않았다. linger 가 꺼진 Linux, 화면에 로그인하지 않은 Mac 도 안내 문구만 있고 보지 않았다 |
| 서비스에서 `hosts: - ssh:` 로 대신 붙이기 | 미확인 | 서비스에는 ssh agent 가 없다. 초대 줄로 붙인 호스트로만 봤다 |
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
| 죽은 controller 의 메시지 세션 (11-26) | 세션을 닫지 못하고 죽은(kill -9) 뒤 같은 scale set 에 새 세션을 열면 `409 Conflict`. 죽인 때부터 34초, 79초, 84초 뒤에 열렸다(세 번, 15초마다 물음). 그동안 온 잡은 배정되지 않고 기다렸다가 세션이 열린 뒤 왔다 |
| `JobAvailable` 과 `AcquireJobs` (11-26) | github.com 에서는 오지 않는다. 지금까지의 메시지 250여 개와 손으로 연 세션 모두에서 잡은 `JobAssigned` 로 바로 왔고(`runnerRequestId` 0, `totalAvailableJobs` 늘 0) acquire 할 것이 없었다. **일부만 acquire 해서 고르는 길은 없다** |
| 배정된 잡이 러너에게 나가는 순서 (11-26) | 배정된 순서. 잡 4개가 1 · 2 · 4 · 3 순으로 배정된 뒤 러너를 하나씩 띄우자 1 · 2 · 4 · 3 순으로 받았다. pumasi 가 「이 잡을 위해」 띄운 러너도 그 클래스의 가장 오래된 잡을 받는다(한 scale set 안에서 순서를 고를 수 없다) |
| `GetMessage` 의 `maxCapacity` (11-26) | 그 scale set 에 배정되는 잡 수의 상한이고 도는 잡도 센다. 1 로 두고 잡 3개: 하나만 배정, 그 잡이 끝난 뒤 다음 poll 에서 다음 것이 배정. 100초짜리 잡이 도는 66초 동안 다음 잡은 배정되지 않았다 |
| 상한 때문에 배정되지 않은 잡 (11-26) | `queued` 로 남고 통계에도 안 잡힌다(`totalAvailableJobs` 0). 같은 라벨의 둘째 scale set 을 열자 1초 뒤 그쪽으로 배정됐고, 같은 라벨의 일반 등록 러너를 붙이자 듣기 시작한 3초 뒤 그 러너가 받았다. (상한 없이 배정된 잡은 위 줄대로 고정 러너로 가지 않는다) |
