# 지금의 설계

코드가 지금 하는 일을 적는다. 무엇을 실물로 확인했는지는 [verified.md](verified.md), 근거 조사는 [research/](research/).

## 풀려는 문제

셀프호스트 러너를 여러 머신에 깔아 쓰면 반복해서 생기는 일이다.

- GitHub 은 비어 있는 러너면 **호스트 부하와 상관없이** 잡을 넣는다. 손잡이는 러너 개수뿐이다.
- 잡 크기가 다르다. lint 한 번과 JVM·DB·브라우저를 띄우는 e2e 한 번이 똑같이 러너 1개다.
- 잡이 남긴 컨테이너·디렉터리가 다음 잡의 자원을 갉아먹는다.
- 사람이 쓰는 머신을 러너로 빌리면 사람도 CI 도 서로 방해한다.

그래서: 잡 크기와 호스트 여유를 보고 배치하고(자리가 없으면 GitHub 큐에서 기다린다), 러너는 잡 하나로 끝내고, 사람이 앉은 머신에는 새 일을 주지 않는다.
Kubernetes 를 요구하지 않는다.

## GitHub 이 주는 손잡이는 scale set 뿐이다

GitHub 공식 `actions/scaleset` 클라이언트 위에 만든다. 그 API 의 성질이 설계를 정한다.

| 사실 | 설계에 주는 뜻 |
|---|---|
| scale set 안에서는 GitHub 이 비어 있는 아무 러너에 잡을 준다. 특정 잡을 특정 러너로 보낼 수 없다 | **클래스(크기·OS)마다 scale set 을 따로 만든다.** 유일한 분류 수단이다 |
| 잡이 배정되면 레포·워크플로·잡 이름이 러너가 생기기 전에 온다 | 무엇이 기다리는지 보고 몇 개를 어디에 띄울지 정한다 |
| 잡의 `runs-on` 라벨이 전부 scale set 의 라벨 안에 있으면 그 scale set 으로 온다. 라벨은 만들 때 정해지고 `self-hosted`·OS·아키텍처도 줄 수 있다 | 기본 라벨은 `[self-hosted, pumasi-<클래스>]`, 클래스의 `labels` 로 바꾼다 |
| 맞는 scale set 이 있으면 같은 라벨의 일반 등록 러너는 idle 이어도 잡을 받지 못한다. scale set 에 러너가 없어도 넘어가지 않는다 | 고정 러너에서 옮길 때 라벨 단위로 한 번에 넘어온다. 되돌리기는 scale set 삭제(`down`) |
| 러너 목록 조회 API 가 없다. 죽은 러너의 등록은 지울 때까지 남는다 | **controller 가 러너 장부(SQLite)를 든다.** 에이전트의 스냅샷으로 맞춘다 |
| 용량을 0 으로 보고하면 대기 잡이 안 보인다 | 용량은 늘 넉넉히 보고하고, **러너를 안 띄우는 것으로** 기다리게 한다 |
| 아무도 안 받은 배정은 5분 뒤 거둬졌다 다시 오고, 세 번 뒤에는 메시지가 끊긴다 | 미룬 수요를 장부에 들고 있다가 자리가 나면 메시지 없이 스스로 띄운다 |
| 잡을 든 러너의 등록 삭제는 GitHub 이 거절한다 | 떠 있는 러너를 지우기 전에 **등록 삭제를 먼저 청한다**(아래 견고함) |
| 러너가 죽어도 GitHub 은 약 10분 뒤에야 잡을 실패로 닫고, 다시 돌리지 않는다 | 러너의 죽음은 에이전트가 직접 보고, 다시 돌리기는 `rerun:` 으로 |

## 구성

```
                 GitHub (scale set API)
                        │  listener (클래스마다 하나)
              ┌─────────▼──────────┐
              │   controller        │  pumasi up
              │  클래스 · 장부 · Decide │  listen :7411
              └───▲───────────▲─────┘
                  │ TLS 연결 하나씩 (호스트가 건다)
          ┌───────┴──┐   ┌────┴──────┐
          │ agent     │   │ agent      │   호스트마다 하나, 그 사용자의 서비스
          │ docker    │   │ native     │   러너 = 컨테이너 또는 프로세스
          └───────────┘   └────────────┘
```

- **controller** — 클래스마다 listener 로 수요를 받고, 호스트들의 스냅샷으로 배치를 정해 "이 호스트에 이 클래스 러너 하나"를 시킨다.
  러너 하나짜리 JIT 설정을 만들어 넘긴다. GitHub 토큰은 controller 에만 있다.
- **agent** — 호스트마다 하나. 러너를 띄우고 지우고, 호스트와 러너의 수치를 답한다. GitHub 자격이 없다.
- 바이너리는 하나다(`pumasi`). 호스트는 자기 OS 의 판을 controller 에서 받는다.

## 클래스와 배치

클래스 = scale set 하나 = 러너의 크기·OS·드라이버. 키는 README 의 표.

배치는 순수 함수 `Decide(snapshot) -> Plan` 이고 결정마다 사유 문자열을 남긴다(대기 사유가 그것이다).

1. **수요**: 클래스별 대기 잡 수 − 이미 뜨고 있거나 노는 그 클래스 러너 수.
2. **자격**: 클래스의 OS·`hosts`·`mounts` 에 맞고, 닫혀 있지 않고(연결 끊김, 격리, 디스크 부족, 사람이 쓰는 중, CPU 압박(PSI) 40% 이상, 호스트가 스스로 닫음),
   `예산 − 쓰는 중 − 다른 클래스의 reserve ≥ 클래스 크기` 인 호스트.
3. **순위**: server 먼저, 그다음 desktop. 같은 종류 안에서는 여유가 큰 쪽.
4. **자리가 없으면 아무것도 안 한다.** 잡은 GitHub 큐에서 기다리고, 10분(`waitAlert`)이 넘으면 사건이 된다.
5. 노는 러너는 두지 않는다. 잡이 오면 띄우고(기동 수 초), 잡을 못 받은 러너는 5분 뒤 지운다.

호스트의 예산이 유일한 상한이다. 클래스별 러너 수 상한은 없다.

## 호스트가 붙는 법

**호스트가 controller 로 연결을 연다.** 길은 이것 하나다. controller 는 `listen` 포트 하나를 열고, 에이전트는 어떤 포트도 열지 않는다.

- **연결 하나.** 호스트가 TLS 로 붙고 그 위에 스트림을 여러 개 얹는다(yamux). controller 는 그 스트림으로 에이전트의 HTTP 계약(`/v1/snapshot`, `/v1/runners`)을 부른다.
- **끊기면 호스트가 다시 건다**, 1초부터 30초까지 늘려 가며. keepalive 10초, 20초 응답이 없으면 양쪽이 버린다. controller 가 내려가 있어도 러너와 잡은 그대로 돈다.
- **초대 줄**이 하는 일: controller 에서 자기 OS 의 바이너리를 받아 `~/.local/bin/pumasi` 에 두고, 붙기 전 확인을 하고, 에이전트를 그 사용자의 서비스로 올린다.
  호스트가 말하는 것은 이름·예산·종류뿐이다(기본: hostname, 머신의 절반, Mac·Windows·화면 세션이 있는 Linux 는 desktop).
- **`hosts: - ssh: X`** 는 같은 일을 controller 가 대신 한다: ssh 로 들어가 초대 줄을 그 호스트의 이름·예산·종류로 실행한다(스크립트는 stdin 으로).
  ssh 는 그때와, 안 붙어 있는 그 호스트를 `down` 이 치울 때(`pumasi leave` 실행)만 쓴다. 그 호스트의 에이전트가 계속 안 붙으면 1분마다 초대 줄을 다시 실행한다.
- **판 맞추기.** 호스트는 붙을 때 자기 판(바이너리 sha256)을 말한다. 다르면 controller 의 판을 받아 sha 를 확인하고 자기 자신을 바꿔 실행한다. 러너는 건드리지 않는다.
  못 바꾸면 이유를 말하며 옛 판으로 붙고 `agent version` 사건이 열린다.

### 서로를 믿는 방법

- controller 는 처음 뜰 때 TLS 키와 인증서를 만든다(상태 디렉터리의 `join.key`·`join.crt`).
- **호스트 → controller**: 초대 줄에 인증서의 공개키 지문이 있다(`--pinnedpubkey`). 에이전트도 그 지문으로만 붙는다. 호스트는 시키는 이미지를 실행하므로 이 확인이 필요하다.
- **controller → 호스트**: 초대 토큰. `pumasi invite` 때마다 새로 만들고 기본은 호스트 하나·1시간이다. 인자가 아니라 환경(`PUMASI_TOKEN`)으로 가서 프로세스 목록에 남지 않는다.
  호스트는 가입할 때 자기 id 와 비밀값을 만들어 보내고 controller 는 그 해시만 적는다(`hosts.json`). 그 뒤 연결은 id 와 비밀값으로 한다.
- **이미 가입한 호스트는 토큰 없이 다시 가입한다**(이름·예산·종류를 바꿀 때). 토큰의 1회 제한은 새 호스트에만 걸린다.
- 설정의 `hosts:` 에 있는 이름은 예약이다. 그 이름으로는 controller 가 그 호스트에게 준 토큰으로만 가입한다.
- `listen` 포트는 가입·바이너리 내려받기·세션만 받는다. 대시보드와 status 는 loopback 에만 열린다.

### OS 마다 다른 것

| | Linux | Mac | Windows |
|---|---|---|---|
| 초대 줄 | sh | sh (같은 줄) | PowerShell |
| 붙기 전 확인 | docker, cgroup v2 | 러너 배포판 받기, 디스크, 로그인 세션 | 같음 |
| 에이전트를 지키는 것 | systemd user unit (linger 일 때) | LaunchAgent `dev.pumasi.agent` | HKCU `Run` 값 — 로그온 때 뜨는 감독 프로세스 |
| 그게 안 되면 | 스스로 다시 뜨는 프로세스(재부팅 뒤에는 안 올라온다 — 가입할 때 말한다) | 같음(화면에 로그인하지 않은 Mac) | — |
| 판 교체 | 같은 pid 로 exec | 같은 pid 로 exec | 파일을 바꿔 두고 끝남 → 감독 프로세스가 새 판을 띄움 |

Mac·Windows 의 에이전트가 **로그인한 사용자의 세션**에서 도는 것은 잡 때문이다: 서명은 그 사용자의 키체인을, 브라우저 시험은 화면을 쓴다. 그래서 Windows 는 서비스(세션 0)가 아니다.
ssh 세션에서 붙인 Windows 호스트는 그 세션이 끝날 때 에이전트도 죽는다 — 그 머신의 창에서 붙인다.

### 떼기

- 호스트에서 `pumasi leave`: 에이전트·러너(컨테이너, native 러너의 scope, 안쪽 docker 용 이미지)·`~/.cache/pumasi`·러너 루트·바이너리·서비스를 지우고 controller 에 알린다.
  러너가 떠 있으면 목록을 보이고 거부한다(`--force`).
- `pumasi down`: 붙어 있는 호스트에 같은 일을 시키고 scale set 을 지운다. 안 붙어 있는 호스트는 `hosts:` 의 것이면 ssh 로 `leave` 를 실행하고, 스스로 붙은 것이면 "거기서 `pumasi leave`" 라고 말한다.
- controller 가 잊은 호스트의 러너는 장부에서 `host … is not configured` 로 닫힌다.

## 러너

러너는 잡 하나로 끝난다. 작업 디렉터리·도구 캐시도 그 러너의 것이다.

| | docker (Linux 기본) | native — Linux | native — Mac | native — Windows |
|---|---|---|---|---|
| 러너 | 컨테이너 | systemd user scope 안의 프로세스 | 프로세스 그룹 + 자손 추적 | 이름 붙은 Job Object |
| 한도 | cgroup: CPU·메모리 | 같음(`CPUQuota`, `MemoryMax`, swap 0) | **없다** | Job Object: 넘는 할당이 실패 |
| 한도를 넘으면 | 그 프로세스만 죽고 OOM 사건 | 같음 | `ran over its class` 사건, 잡은 계속 | 거절 횟수를 세어 같은 사건 |
| 수치 | cpu 초, 메모리 peak, I/O, 압박(PSI) | 같음(io 는 위임된 호스트만) | cpu 초, 메모리 peak(5초 샘플) | cpu 초, 메모리 peak, I/O |
| 남은 자식 정리 | 컨테이너 삭제 | `cgroup.kill` | 그룹에 SIGTERM → SIGKILL, 벗어난 자식도 추적 | Job 종료 |
| 에이전트가 죽어도 | 러너는 돈다. 새 에이전트가 이어받고 수치도 이어진다 | 같음(scope 는 systemd 의 것) | 같음 | 같음(같은 로그온 세션에서) |
| 호스트의 것 | 안 보인다(`mounts` 로 붙인 것만) | 전부 보인다 | 전부 보인다 | 전부 보인다 |

- **러너가 잡을 받았는지는 에이전트가 먼저 안다.** 러너 출력의 `Running job` 줄을 읽어 busy 를 알린다(GitHub 의 메시지는 30초쯤 늦다).
- **JIT 설정은 환경 변수로 넘긴다**(인자는 `ps` 에 보인다). 에이전트의 환경에서 자격처럼 보이는 것(`*TOKEN*`, `*SECRET*`, `PUMASI_*`)은 러너에 넘기지 않는다.
- **러너 판**은 `image` 의 태그다(`ghcr.io/actions/actions-runner:2.338.2`). 컨테이너는 그 이미지를 쓰고, native 는 태그에서 판 번호를 읽어 같은 판의 배포판을
  처음 한 번 받는다(`~/.cache/pumasi/dist`, sha256 을 GitHub release 의 digest 와 대조한 뒤에만 푼다). **태그가 `:latest` 면 native 러너는 뜨지 못한다.** 러너의 자체 업데이트는 꺼져 있어 판은 사람이 올린다.

### 컨테이너 러너에 호스트를 붙이기

- **`mounts`** — `docker run --mount type=bind` 로 같은 경로에 붙인다(`:ro` = 읽기 전용; 없는 경로를 docker 가 root 로 만들지 않는다).
  호스트는 물어 온 경로 중 있는 것을 답하고, 모두 있는 호스트에만 놓인다. 없으면 `no host has /srv/ci-cache`. 컨테이너 안의 사용자(`runner`, uid 1001)가 쓸 수 있게 하는 것은 호스트의 일이다.
  native 클래스에 적으면 붙이지 않고(이미 보인다) 놓을 호스트만 가린다.
- **작업 디렉터리는 첫 쓰기 가능 mount 안에 둔다**(`<mount>/.pumasi-work/<러너>`, 끝나면 지운다). 컨테이너의 overlay 와 mount, 서로 다른 mount 사이에서는
  `link()` 가 `EXDEV` 라 pnpm·uv·ccache 의 hard link 가 조용히 복사로 바뀐다. mount 가 모두 `:ro` 면 컨테이너 안에 둔다.
- **`docker: inside`** — 러너 컨테이너 안에 docker 데몬을 띄운다. 잡의 `docker run`·`services:`·compose 가 거기 생기므로 클래스 한도와 수치 안이고,
  잡이 남긴 컨테이너·볼륨·네트워크·이미지는 러너와 함께 사라진다. 대가는 둘이다. 러너 컨테이너가 **privileged** 다 — 컨테이너 격리를 믿고 낯선 레포를 받는 클래스에는 쓰지 않는다.
  그리고 **이미지를 러너마다 새로 받는다**(안쪽 저장소는 러너와 함께 지우는 볼륨). 러너 사이에 저장소를 남기면 잡마다 10초쯤 아끼지만 앞 잡의 것이 다음 잡에 보이고 디스크 정리 규칙이 따로 든다 — 남기지 않는다.
  기본 러너 이미지에 iptables 가 없어서 호스트마다 처음 한 번 얹은 이미지를 짓는다(`pumasi-inner-docker:<판>`). compose 플러그인은 잡이 받는다. native 클래스에는 못 적는다.

### native 러너

- **Linux**: `systemd-run --user --scope --unit=pumasi-<러너>` 안의 `run.sh`. systemd-run 이 자신을 scope 에 넣고 나서 러너의 셸이 되므로 러너는 무엇을 띄우기 전부터 한도 아래다.
  띄울 수 없는 호스트는 스스로 말한다(systemd user 세션 없음, cpu·memory 미위임, linger 꺼짐) — 그 호스트에는 native 클래스를 놓지 않고 대기 사유에 그 말이 나온다.
  잡이 호스트 docker 로 띄운 컨테이너는 이 cgroup 밖이다.
- **러너 루트는 홈 밖이다**: Linux `/var/tmp/pumasi-<uid>`, Mac `/Users/Shared/pumasi-<uid>`, Windows `C:\pm\<SID 끝자리>`. 잡 로그에 호스트 사용자의 홈 경로가 나오지 않고
  Windows 에서는 경로가 짧다(260자 한계). 그 자리가 이미 남의 것이면 `~/.cache/pumasi/runners` 를 쓴다.
- **배포판은 복제한다**: Mac 은 APFS clone, Linux·Windows 는 읽기 전용 하드 링크(다른 파일시스템이면 복사). 러너들이 파일을 함께 쓰므로 쓰기 권한을 떼어 두고,
  쓰기 가능으로 돌아와 있으면 손댄 것으로 보고 새로 받는다.
- **지우지 못한 것은 치워 두고 알린다.** 잡이 쓰기 권한을 뗀 디렉터리는 권한을 되돌려 지운다. 잡이 컨테이너로 남긴 root 의 파일처럼 그 사용자가 못 지우는 것은
  러너 디렉터리째 `<러너 루트>/.trash/` 로 옮기고 `leftover` 사건이 경로와 이유를 말한다. 지우는 것은 사람이 한다.
- **Mac 의 호스트 여유**: 남이 쓰는 CPU 는 Mach `host_statistics`, 메모리는 `kern.memorystatus_level`. 커널의 memory pressure 가 warning 이상이면 호스트가 스스로 닫는다.
- **Windows**: Job 에 이름을 붙이고 러너의 셸이 Job 핸들을 물려받는다 — 에이전트가 죽어도 Job 이 살고 새 에이전트가 이름으로 다시 연다. 이름은 로그온 세션 안의 것이라 다른 세션의 에이전트는 못 찾는다.
  `shell: bash` 는 WSL 이 깔린 머신에서 WSL 의 bash 로 풀리고, `shell: powershell` 은 실행 정책 `Restricted` 에 막힌다(`shell: powershell -NoProfile -ExecutionPolicy Bypass -File {0}`) — pumasi 가 아니라 Windows 러너의 성질이다.
- **재부팅 판정**은 호스트의 boot id 로 한다(Linux boot_id, Mac `kern.bootsessionuuid`, Windows 커널 시작 시각). **Windows 의 빠른 시작(fast startup)으로 껐다 켠 것은 새 부팅이 아니다** — 그때 끝난 러너는 `rebooted` 가 아니라 `died` 로 나온다.

### 고정 러너에서 옮길 때 달라지는 것

- 워크스페이스에 남겨 둔 것(`clean: false`, `node_modules`)에 기대던 잡은 매번 처음부터 한다.
- `setup-node`·`setup-java` 는 러너마다 다시 받는다.
- 러너 이름이 매번 다르다. 러너 이름으로 캐시 디렉터리를 나누던 워크플로우는 클래스나 잡 이름으로 나눈다.
- GitHub API 로 센 빈 러너 수는 늘 0 이다.
- 순서: 라벨 하나 = `driver: native` + `hosts:` 클래스 하나로 시작(`labels` 에 옛 라벨을 그대로 적으면 워크플로우는 안 바뀐다) → 잡 사이에 넘기는 것이 없는 클래스부터 `hosts:` 를 푼다 → 호스트를 안 쓰는 잡은 컨테이너 클래스로.

## 사람 우선

데스크톱은 누군가의 책상이다. 그 사람이 앉아 있는 동안에는 새 러너를 받지 않는다. 도는 잡은 건드리지 않는다.

- 에이전트는 "마지막 키보드·마우스 입력이 몇 초 전인가"만 말한다. Mac 은 `HIDIdleTime`, Windows 는 `GetLastInputInfo`(사용자 세션 안에서만 읽힌다),
  Linux 는 logind 화면 세션의 `IdleHint`. 알 수 없으면 비우고, 신호를 모르는 호스트는 열려 있다.
- `desktop` 호스트의 마지막 입력이 **15분** 안이면 그 호스트는 닫힌다(STATE 와 대기 사유에 `in use`). server 는 닫지 않는다. 설정은 없다.
- Mac 에이전트는 사람이 있는 동안 러너의 프로세스를 background 대역으로 내린다(`PRIO_DARWIN_BG`). Mac 은 CPU 한도가 없어서 이것이 유일한 수단이다. Linux·Windows 는 한도가 강제되므로 하지 않는다.
- 남이 쓰는 CPU·메모리는 사람 여부와 무관하게 빈 자리 계산에서 빠진다.

## 견고함

원칙 셋.

1. **상태는 다시 만들 수 있다.** 진실은 호스트의 러너(에이전트가 본다)와 GitHub 이다. 장부는 그 둘에서 복원된다. controller 는 언제 죽어도 된다.
2. **도는 잡은 건드리지 않는다.** 예외는 사용자가 명시한 것(`down --force`, `leave --force`, 클래스의 `maxDuration`)뿐이다.
   떠 있는 러너를 지우는 모든 경로(idle timeout, 기동 시간 초과, 장부에 없는 러너, Ctrl-C)는 **GitHub 에 그 러너의 등록 삭제를 먼저 청한다.** GitHub 은 잡을 든 러너의 삭제를 거절하므로,
   방금 잡을 받아 아직 idle 로 보이는 러너도 지워지지 않는다. 거절되면 그 러너를 busy 로 되살리고, GitHub 에 물을 수 없으면 그 회차에는 지우지 않는다.
3. **조용히 실패하지 않는다.** 스스로 고친 것도 못 고친 것도 사건으로 남기고 알린다.

**사건**은 장부의 한 행이고 `key` 가 같으면 같은 사건이다(`host-unreachable/server1`). 열릴 때, 30분마다, 닫힐 때 알린다. 5분 안에 열렸다 닫히기를 세 번 넘기면 flapping 한 번으로 묶는다.
알림 전송이 실패해도 배치는 멈추지 않고 다음 회차에 다시 보낸다.

| 장애 | 감지 | 하는 것 | 알림 |
|---|---|---|---|
| controller 죽음 | 서비스 관리자 | 다시 떠서 잠금 → 호스트를 기다림 → 러너 이어받기. 놓친 메시지는 스냅샷과 GitHub 잡 상태로 메운다 | `controller is back: was down …, adopted N runners, N jobs finished meanwhile` |
| 에이전트 죽음 | 세션 끊김 | 호스트의 서비스가 다시 올린다. 러너는 그대로, 수치는 디스크에 있어 이어진다 | 2분 넘게 안 붙으면 사건 |
| 호스트 연결 끊김 | keepalive·스냅샷 실패 | 새 배치 중단, 러너는 용량에 그대로 계산, 돌아오면 스냅샷으로 맞춤 | `hostLostAfter`(2분) 넘으면 사건(끊긴 이유와 함께), 돌아오면 해제 |
| 호스트 재부팅 | boot id 가 바뀜 | 그 러너를 lost 로 닫고 등록 삭제. 돌던 잡은 인프라 실패 | 사건 + 잃은 잡 목록 |
| 러너가 못 뜸 | `provisionTimeout`(3분), 조기 종료 | 등록 삭제를 청한 뒤 지우고 다시 배치 | 같은 호스트에서 연속 3번이면 격리 |
| 호스트 격리 | 연속 기동 실패 3번 | 새 배치 중단, 5분마다 점검, 통과하면 복귀 | 격리·복귀 각각 |
| 잡 도중 러너가 죽음 | 종료 코드 ≠ 0 인데 잡 완료 없음 | 등록 삭제(GitHub 이 놓을 때까지 재시도). 인프라 실패 | 사건 + 잡·호스트·종료 코드 |
| 메모리 한도 | cgroup `oom_kill`, Job Object 거절 | 없음(코드·클래스 크기의 문제) | `hit the <클래스> … memory limit` |
| 잡이 오래 기다림 | 처음 본 시각 + `waitAlert`(10분) | 재평가는 계속 | 사건 + 사유, 시작되면 해제 |
| 잡이 오래 돎 | 클래스 `maxDuration` | 설정했을 때만 러너 정지 | 넘는 순간 사건 |
| 디스크 부족 | 러너가 놓이는 디스크의 여유 < `diskMinFree`(10%) | 새 배치 중단, 끝난 pumasi 컨테이너·안 쓰는 러너 이미지 정리 | 사건, 회복되면 해제 |
| 지우지 못한 파일 | 러너를 지울 때 | 러너 디렉터리를 `.trash/` 로 | `leftover` 사건 |
| GitHub API 실패 | 호출 오류 | 백오프로 재시도. 401·403 은 멈춤 | 2분 넘으면 사건, 401·403 은 즉시 |
| 장부가 깨짐 | 열기·무결성 검사 | 옆으로 치우고 빈 장부에서 시작, 스냅샷과 GitHub 으로 복원 | 사건(이력은 잃는다) |
| 주인 없는 러너 | 장부에 없는 pumasi 러너 | 등록 삭제가 받아들여진 것만 지움 | 로그 |
| controller 둘 | 상태 디렉터리 잠금 | 두 번째가 바로 끝남 | 오류 한 줄 |

**다시 돌리기** — 실패의 원인이 우리 쪽이면(호스트 재부팅, 러너가 밖에서 죽음) `rerun: 1` 일 때 그 run 의 실패한 잡을 한 번 다시 돌린다(기본 0 = 알리기만).
코드 실패·메모리 한도·`maxDuration` 초과는 다시 돌리지 않는다.

**알림** — `notify.webhook`(JSON POST, Slack·Teams 수신 웹훅이면 그 형식) 또는 `notify.command`(같은 JSON 을 stdin 으로). `level`: `problem`(기본) · `recovered`(스스로 고친 것도) · `all`.

장애 표의 행은 `make chaos`(`scripts/chaos.sh`)가 실제 호스트에서 일으켜 본다.

## 운영

- **상태 디렉터리** `~/.local/state/pumasi/`: 장부(`pumasi.db`), 이벤트 로그, TLS 키, 가입한 호스트, 초대 토큰. 한 디렉터리에 controller 하나.
- **대시보드**: controller 가 loopback 에 내는 한 페이지(정적 파일 셋을 바이너리에 넣었다). 5초마다 `/api/state`·`/api/jobs`·`/api/series` 를 읽는다. 읽기 전용. `/metrics` 는 Prometheus 용.
- **status 의 호스트 열**: `SEEN`(마지막 응답), `RTT`(스냅샷 왕복), `AGENT`(에이전트의 판), `LAST DROP`(마지막으로 끊긴 이유: `keepalive timeout`, `connection reset`, `agent exited`).
- **`diskMinFree`** 기본 10%. 디스크가 늘 차 있는 머신(노트북)을 호스트로 쓰면 이 값에 걸려 러너가 놓이지 않는다 — 사건이 그렇게 말한다. 설정에서 낮출 수 있다.

### controller 를 상시로 띄우기

`up` 은 죽었다 다시 떠도 이어받으므로 서비스 관리자에게 "죽으면 다시 띄워라"만 시킨다. Mac 에서는 로그인한 사용자의 LaunchAgent 로:

```xml
<!-- ~/Library/LaunchAgents/dev.pumasi.controller.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>dev.pumasi.controller</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string><string>-c</string>
		<string>PUMASI_GITHUB_TOKEN=$(gh auth token) exec /path/to/dist/pumasi up -config /path/to/pumasi.yaml</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>10</integer>
	<key>ProcessType</key><string>Interactive</string>
	<key>StandardOutPath</key><string>/path/to/pumasi-up.log</string>
	<key>StandardErrorPath</key><string>/path/to/pumasi-up.log</string>
	<key>EnvironmentVariables</key><dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string></dict>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.pumasi.controller.plist   # 올리기
launchctl bootout gui/$(id -u)/dev.pumasi.controller                                  # 내리기 (러너와 잡은 그대로)
```

- 토큰은 plist 에 적지 않고 뜰 때마다 읽는다. `gh` 에 계정이 여럿이면 `gh auth token -u <계정>` 으로 고정한다.
- `dist/` 는 통째로 둔다(호스트가 거기서 자기 판을 받는다).
- `ProcessType` 은 `Interactive`. 안 적으면 launchd 가 바쁜 머신에서 CPU·IO 를 뒤로 미룰 수 있다.
- 셸에서 `pumasi up &` 로 띄우면 셸이 우선순위를 낮춘다(zsh: nice 5). `up` 은 그럴 때 경고 한 줄을 낸다.
- Linux 에서는 systemd user unit(`Restart=always`)이 같은 일을 한다.

## 하지 않는 것

- NAT 를 뚫는 중계. 호스트가 `listen` 주소에 닿아야 한다.
- 호스트가 클래스나 이미지를 정하는 것.
- 웹에서 호스트를 붙이고 떼는 버튼, 대기열의 우선순위(`reserve` 하나로 한다), 클래스별 러너 수 상한.
- 잡을 다른 호스트로 옮기기, 사람이 돌아왔다고 도는 잡을 멈추기.
- 러너 사이에 도구 캐시·docker 이미지 저장소를 남기기.
- VM 드라이버(Tart, microVM), GitHub 이외의 CI, 클라우드 인스턴스 만들기.
