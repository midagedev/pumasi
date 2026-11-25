# pumasi

셀프호스트 GitHub Actions 러너를 이미 가진 머신들(Linux 서버, 사람이 쓰는 Mac·Windows·Linux 데스크톱)에 나눠 띄운다.
머신마다 빌려줄 CPU·메모리 상한을 정해 두면, 잡이 올 때 자리가 있는 곳에 일회용 러너를 띄우고 끝나면 지운다. 자리가 없으면 잡은 GitHub 큐에서 기다린다.
바이너리 하나를 한 머신(controller)에서 실행하고, 나머지 머신(호스트)은 초대 한 줄로 붙는다.

## 5분 시작

**필요한 것**

- controller 머신: Go(빌드용), 그리고 호스트들이 이 머신의 포트 하나(기본 7411)에 닿을 수 있을 것(같은 망, VPN, tailnet).
- 호스트: Linux 는 그 사용자가 쓸 수 있는 Docker 와 cgroup v2. Mac·Windows 는 아무것도 필요 없다(러너가 그 사용자의 프로세스로 돈다).
- GitHub 토큰: 그 레포에 러너를 등록할 수 있는 권한. classic PAT 면 `repo`(조직 단위로 쓰면 `admin:org`), 레포의 admin 이어야 한다.
  모자라면 `up` 이 `create scale set …: 403` 으로 끝난다. `gh` 를 쓰면 `gh auth refresh -s repo` 뒤 `gh auth token`.
  토큰은 controller 밖으로 나가지 않는다 — 호스트로는 러너 하나짜리 일회용 설정만 간다.

**1. 설정** — `pumasi.yaml` ([예시](pumasi.example.yaml))

```yaml
url: https://github.com/OWNER/REPO
listen: 0.0.0.0:7411                       # 호스트가 붙으러 오는 주소 (기본값)
classes:
  - {name: small, cpus: 1, memory: 1G}     # 워크플로우: runs-on: [self-hosted, pumasi-small]
```

**2. controller 를 띄운다**

```sh
make                                              # dist/pumasi 와, 호스트가 받아 갈 OS별 빌드
PUMASI_GITHUB_TOKEN=$(gh auth token) ./dist/pumasi up
```

```
11:57:40 class small  runs-on: [self-hosted, pumasi-small]
11:57:40 hosts join at [::]:7411  (`pumasi invite` prints the line to run on a host)
11:57:40 dashboard http://127.0.0.1:52673/  (`pumasi open`)
11:57:40 listening for jobs (Ctrl-C to stop)
```

**3. 호스트를 붙인다** — 다른 터미널에서 초대 줄을 받아, 붙일 머신에서 실행한다. `up` 이 떠 있는 동안만 붙는다.

```
$ ./dist/pumasi invite
Run this on the host that should join. It works for one host, until 12:57.

Linux, macOS:

  curl -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.sh | PUMASI_TOKEN=<token> sh -s --

Windows (PowerShell, in a window on that machine's desktop):

  $env:PUMASI_TOKEN='<token>'; & ([scriptblock]::Create((curl.exe -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.ps1 | Out-String)))
```

호스트는 붙기 전에 하나씩 확인하고 막힌 것을 이름으로 말한다:

```
ok    tcp 10.0.0.5:7411 (3ms)
ok    controller certificate matches the invite
ok    token accepted, the name desk1 is free
ok    docker 27.3.1 reachable as this user
ok    cgroup v2 with memory and cpu controllers

joined as desk1 (server): lends 6 cpus and 9.0G, runs as the systemd user unit pumasi.service
```

- 기본은 그 머신의 **절반**을 hostname 으로 빌려준다. 줄 끝에 `--name desk1 --cpus 6 --mem 9G` 로 바꾼다.
- 초대 줄은 호스트 하나, 1시간 동안만 통한다(`pumasi invite --uses 5 --ttl 24h`). `-k` 는 위험하지 않다 — `--pinnedpubkey` 가 이 controller 의 키만 받는다.
- 주소가 여럿인 머신이면 `invite` 가 나머지도 알려 준다. 호스트가 닿는 주소를 쓴다.

**4. 잡을 보낸다** — 워크플로우에 `runs-on: [self-hosted, pumasi-small]`.

```
11:58:52 start small on desk1  free 6.0cpu/9.0G  (small-desk1-1ea93ba5)
11:58:57 ready small-desk1-1ea93ba5  5.1s
11:59:30 job build on small-desk1-1ea93ba5
12:01:02 done build on small-desk1-1ea93ba5  succeeded
```

첫 러너는 느리다: Linux 호스트는 러너 이미지(수백 MB)를, Mac·Windows 는 러너 배포판을 처음 한 번 받는다. 3분(`provisionTimeout`) 안에 못 뜨면 지우고 다시 띄우고,
같은 호스트에서 세 번 연속이면 그 호스트를 잠시 쉬게 한다(5분마다 다시 본다). 회선이 느리면 호스트에서 `docker pull ghcr.io/actions/actions-runner:2.338.2` 를 먼저 해 둔다.

**끄기** — Ctrl-C 는 controller 만 내린다. 도는 잡은 호스트에서 계속 돌고 다음 `up` 이 이어받는다.
`pumasi down` 이 전부 지운다: 호스트의 러너·에이전트·파일, GitHub 의 scale set. 러너가 떠 있으면 목록을 보이고 거부한다(`--force`).
호스트 하나만 떼려면 그 호스트에서 `pumasi leave`.

**대신 붙여 주기** — ssh 로 들어갈 수 있는 호스트는 설정에 적어 두면 `up` 이 거기서 초대 줄을 대신 실행한다. 붙은 뒤는 같다.

```yaml
hosts:
  - {ssh: server1, budget: {cpus: 4, memory: 6G}}
  - {ssh: desk1, desktop: true, budget: {cpus: 6, memory: 9G}}   # WSL 을 쓰는 Windows 면 shell: wsl -e sh -s
```

그 호스트가 `listen` 주소에 닿지 못하면 `up` 이 `server1 cannot reach this controller at …` 로 말한다.

## 클래스

클래스는 러너의 크기와 종류다. 클래스 하나가 GitHub 의 scale set 하나(`pumasi-<이름>`)이고, 잡은 `runs-on` 으로 클래스를 고른다.
모르는 키나 같은 이름의 클래스가 둘이면 `up` 이 거부한다.

| 키 | 뜻 | 기본 |
|---|---|---|
| `name` | `runs-on: [self-hosted, pumasi-<name>]` | 필수 |
| `labels` | scale set 의 라벨을 직접 정한다. 잡의 `runs-on` 라벨이 전부 이 안에 있으면 그 잡을 받는다 — 기존 고정 러너의 라벨을 그대로 적으면 워크플로우를 안 고치고 옮긴다(아래) | `[self-hosted, pumasi-<name>]` |
| `cpus`, `memory` | 러너 하나의 크기. 배치할 때 호스트 예산에서 빼고, Linux·Windows 에서는 한도로 강제한다 | 필수 |
| `os` | `linux` · `macos` · `windows`. 같은 OS 의 호스트에만 놓인다 | `linux` |
| `driver` | `docker`(컨테이너) 또는 `native`(호스트 사용자의 프로세스 — 호스트의 경로·자격·docker 가 그대로 보인다) | linux 는 `docker`, macos·windows 는 `native` 만 |
| `mounts` | 컨테이너 러너에 호스트 경로를 같은 경로로 붙인다(`/srv/cache`, `/srv/keys:ro`). 그 경로가 모두 있는 호스트에만 놓인다 | 없음 |
| `hosts` | 그 호스트들에만 놓는다. 묶인 호스트가 차면 다른 곳이 비어 있어도 기다린다 | 모든 호스트 |
| `reserve` | 이 클래스 러너 N개가 들어갈 자리를 다른 클래스가 못 쓰게 남겨 둔다(배포 잡이 빌드 뒤에 서지 않게) | 0 |
| `maxDuration` | 잡이 이보다 오래 돌면 러너를 멈춘다(`90m`). pumasi 가 도는 잡을 건드리는 유일한 설정이다 | 없음 |
| `docker: inside` | 컨테이너 러너에 자기 docker 데몬을 준다. 잡이 띄운 컨테이너·compose 스택이 클래스 한도와 수치 안에 들어가고 러너와 함께 사라진다. 러너 컨테이너가 **privileged** 가 된다 | 없음 |

```yaml
classes:
  - {name: small,   cpus: 1, memory: 1G}                                  # 컨테이너. 호스트의 것은 안 보인다
  - {name: e2e,     cpus: 4, memory: 8G, docker: inside, maxDuration: 2h} # 잡이 compose 스택을 띄운다
  - {name: cache,   cpus: 2, memory: 3G, mounts: [/srv/ci-cache]}
  - {name: build,   cpus: 4, memory: 8G, driver: native, hosts: [server1]} # 고정 러너처럼 호스트를 그대로 쓴다
  - {name: deploy,  cpus: 2, memory: 4G, reserve: 1}
  - {name: macos,   cpus: 4, memory: 8G, os: macos}
```

- 러너는 **잡 하나로 끝난다.** 작업 디렉터리·도구 캐시(`setup-node` 등)는 남지 않는다. 잡 사이에 남길 것은 `mounts` 나 native 클래스의 호스트 디렉터리, 캐시 액션에 둔다.
- `mounts` 가 있으면 작업 디렉터리를 첫 쓰기 가능 mount 안에 둔다 — pnpm·uv·ccache 가 캐시에서 hard link 로 가져올 수 있게.
- `driver: native`(Linux) 는 systemd user 세션과 cpu·memory 위임, linger 가 필요하다. 없으면 한도 없이 띄우지 않고 대기 사유에 호스트가 이유를 말한다.
- **native 러너는 격리가 약하다**(Mac·Windows 는 늘 native). 잡은 호스트 사용자의 홈·자격·전역 도구를 읽고 바꿀 수 있다. 믿는 레포의 잡에만 쓴다.
- 러너 판은 `image:` 의 태그를 따른다(기본 `ghcr.io/actions/actions-runner:2.338.2`). Mac·Windows 는 그 태그에서 판 번호만 읽으므로 **`:latest` 로 적으면 Mac·Windows 러너가 뜨지 못한다.** 판은 사람이 올린다.

## 기존 고정 러너에서 옮기기

워크플로우를 고치지 않고 옮길 수 있다. 클래스에 고정 러너의 라벨을 그대로 적는다:

```yaml
classes:   # 워크플로우는 runs-on: [self-hosted, Linux, X64, ci-pool] 그대로
  - {name: shared, cpus: 4, memory: 8G, driver: native, labels: [self-hosted, Linux, X64, ci-pool]}
```

- 잡의 `runs-on` 이 그 라벨의 부분집합이면(`[self-hosted, ci-pool]`) 온다. 대소문자는 가리지 않는다. `self-hosted`·OS·아키텍처 라벨도 그냥 적으면 된다.
- **섞여 돌지 않는다.** 그 라벨에 맞는 scale set 이 생기는 순간부터 그 잡은 전부 scale set 으로 오고, 같은 라벨의 고정 러너는 idle 이어도 잡을 받지 못한다.
  `pumasi up` 이 꺼져 있거나 자리가 없으면 잡은 고정 러너로 넘어가지 않고 기다린다. 그러니 옛 러너를 하나씩 멈추며 옮기는 것이 아니라 **라벨 단위로 한 번에 넘어온다.**
- **되돌리기는 `pumasi down`**(scale set 삭제)이다. 그 뒤의 잡은 고정 러너가 바로 받고, 지우는 순간 기다리던 잡은 3 ~ 4분 뒤에 고정 러너로 간다. 넘어오는 동안 고정 러너는 켜 둔 채로 둔다.
- 라벨은 scale set 을 만들 때 정해진다. 바꾸려면 `pumasi down` 뒤 다시 `up` 하거나 클래스 이름을 바꾼다(`up` 이 그렇게 말한다).
- 고정 러너처럼 호스트의 경로·자격·docker 를 쓰는 잡은 `driver: native` 로 시작하고, 잡끼리 호스트 디렉터리로 넘기는 것이 있으면 `hosts:` 로 한 머신에 묶는다.
  넘기는 것이 없는 클래스부터 `hosts:` 를 풀고, 호스트를 안 쓰는 잡은 컨테이너 클래스로 옮긴다. 일회용 러너라서 달라지는 것은 [설계](docs/design.md#고정-러너에서-옮길-때-달라지는-것).

## 호스트 종류와 사람 우선

| | Linux | Mac | Windows |
|---|---|---|---|
| 러너 | docker 컨테이너(또는 native) | 사용자의 프로세스 | 사용자의 프로세스(Job Object) |
| CPU·메모리 한도 | 강제 (넘으면 그 프로세스가 죽고 OOM 사건) | **강제 못 함** — 넘으면 `ran over its class` 사건만, 잡은 계속 | 강제 (넘는 할당이 실패) |
| 에이전트를 지키는 것 | systemd user unit | 로그인한 사용자의 LaunchAgent | 로그온 때 뜨는 프로세스 |
| join 기본 종류 | 화면 세션이 있으면 desktop, 없으면 server | desktop | desktop |

- **server** 는 예산이 차는 대로 먼저 채운다. **desktop** 은 서버가 찬 뒤에 쓰고, 누가 쓰고 있는 동안(마지막 키보드·마우스 입력 15분 안)에는 새 러너를 받지 않는다. 도는 잡은 끝까지 돈다. 설정은 없다.
- 종류는 초대 줄 끝의 `--server` / `--desktop` 으로 바꾼다. 아무도 앉지 않는 Mac mini 는 `--server` 로 붙인다.
- Mac 에서는 사람이 있는 동안 도는 러너를 background 우선순위로 내린다. Mac 은 한도가 없으니 예산을 넉넉히 남겨 잡는다.
- Mac·Windows 의 초대 줄은 그 머신의 화면에 로그인한 세션에서 실행한다. 그래야 잡이 키체인으로 서명하고 창을 띄우며, 재부팅 뒤에도 에이전트가 올라온다.
- 호스트의 디스크 여유가 10% 아래면 그 호스트에 새 러너를 놓지 않는다(`diskMinFree`). 디스크가 늘 차 있는 노트북은 이 값에 걸린다.

```
$ pumasi status
HOST     KIND     OS     CPUS  MEMORY     CPU PRESSURE  RUNNERS  STATE   SEEN    RTT   AGENT         LAST DROP
server1  server   linux  2/4   3.0G/6.0G  0.1%          1        ok      1s ago  8ms   45fd4511f018  -
mac1     desktop  macos  0/4   0.0G/8.0G  -             0        in use  1s ago  1ms   6be4c29a87b3  keepalive timeout

RUNNER                CLASS  HOST     STATE  JOB    AGE
e2e-server1-d8139867  e2e    server1  busy   smoke  23m43s

WAITING  JOBS  WHY
macos    1     no host with 4.0cpu/8.0G free (mac1 in use)
```

## 사건과 알림

pumasi 는 도는 잡을 건드리지 않고(예외: `maxDuration`, `down --force`), 스스로 고친 것도 못 고친 것도 사건으로 남긴다. 열린 사건은 `pumasi status` 맨 위 `PROBLEMS` 에 나온다.

| 일어난 일 | pumasi 가 하는 것 |
|---|---|
| controller 가 죽거나 꺼짐 | 잡은 호스트에서 계속 돈다. 다시 뜨면 이어받고 `controller is back: was down 28s, adopted 7 runners` |
| 호스트 연결 끊김·에이전트 죽음 | 그 호스트에 새 러너를 놓지 않는다. 도는 잡은 그대로. 2분 넘으면 사건(끊긴 이유와 함께), 돌아오면 해제 |
| 호스트 재부팅, 잡 도중 러너가 죽음 | 사건 + 잃은 잡 목록. `rerun: 1` 이면 그 run 의 실패한 잡을 한 번 다시 돌린다 |
| 러너가 못 뜸 | 지우고 다시 배치. 같은 호스트에서 세 번 연속이면 그 호스트 격리, 5분마다 점검 |
| 메모리 한도에 걸림 | `hit the e2e 8.0G memory limit on server1 (peak 8.0G)`. 다시 돌리지 않는다(코드나 클래스 크기의 문제) |
| 잡이 10분 넘게 기다림 | 사건 + 사유(`no host with 2cpu/3G free …`) |
| 지우지 못한 파일(잡이 컨테이너로 남긴 root 의 파일) | 러너는 지우고 그 디렉터리는 치워 둔 뒤 `leftover` 사건이 경로를 말한다. 지우는 것은 사람이 한다 |

```yaml
notify:
  webhook: https://hooks.example/…   # JSON POST {text, key, kind, subject, state, detail}
  command: my-notify --stdin          # 또는 같은 JSON 을 stdin 으로 받는 명령
rerun: 1                              # 우리 쪽 잘못으로 실패한 run 을 한 번 다시 돌린다 (기본 0 = 알리기만)
```

사건은 열릴 때, 30분마다, 해제될 때 한 줄씩 간다. 장애마다의 감지·복구는 [설계](docs/design.md#견고함), 실제로 일으켜 본 결과는 [확인된 것](docs/verified.md).

## 대시보드

`pumasi up` 이 loopback 에 웹 화면을 같이 띄운다(`pumasi open`). 문제 · 호스트 · 러너 · 대기와 사유 · 최근 잡 50개(대기·실행 시간, cpu 초, 메모리 peak, 로그 끝) · 24시간 추이를 한 화면에 보여 준다.
같은 주소의 `/metrics` 는 Prometheus 용이다. 읽기 전용이고 밖으로 열지 않는다.

## controller 를 계속 켜 두기

`up` 은 죽었다 다시 떠도 장부(`~/.local/state/pumasi/`)와 호스트의 상태로 이어받으므로, 서비스 관리자에게 "죽으면 다시 띄워라"만 시키면 된다.
Mac 은 LaunchAgent, Linux 는 systemd user unit — [설계의 운영 절](docs/design.md#운영)에 그대로 쓸 수 있는 예가 있다. 셸에서 `pumasi up &` 로 띄우면 셸이 우선순위를 낮추므로(zsh: nice 5) `up` 이 경고한다.

## 풀지 못하는 것

- **한 run 의 잡들을 같은 머신에 보내기.** scale set 안에서는 어느 러너가 어느 잡을 받을지 정할 수 없다. 호스트에 띄운 것을 뒤 잡이 이어 쓰는 워크플로우는 호스트마다 클래스를 하나씩 두고(`hosts:`) 앞 잡이 run 전체의 클래스를 고르게 한다.
- **native 잡이 호스트 docker 로 띄운 컨테이너의 사용량.** 러너의 한도에도 수치에도 안 잡히고 호스트 전체 압박으로만 보인다. 컨테이너 클래스는 `docker: inside` 로 안에 넣을 수 있다.
- **Mac 의 메모리·CPU 한도.** 강제할 수단이 없다.
- **native 러너와 에이전트는 같은 사용자다.** 잡이 에이전트의 가입 자격을 읽을 수 있다. 배포 자격을 쥔 호스트는 믿는 워크플로우의 클래스만 받게 `hosts:` 로 가른다.
- **NAT 너머의 호스트.** 호스트가 controller 의 `listen` 주소에 직접 닿아야 한다. 중계는 없다.
- **잡을 다른 호스트로 옮기기, 도는 잡을 밀어내기.** GitHub 에 그런 길이 없어서, 죽이면 실패한 잡만 남는다.
- VM 격리(Tart, microVM), GitHub 이외의 CI, 클라우드 인스턴스 만들기는 범위 밖이다.

## 문서

[지금의 설계](docs/design.md) · [실물로 확인된 것](docs/verified.md) · [조사](docs/research/)
