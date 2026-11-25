# pumasi

셀프호스트 GitHub Actions 러너를 이미 가진 머신들(Linux 서버, 사람이 쓰는 데스크톱의 WSL)에 나눠 띄운다.
호스트마다 빌려줄 상한을 정해 두면, 잡이 올 때 자리가 있는 곳에 일회용 러너를 띄우고 끝나면 지운다.
노트북에서 바이너리 하나를 실행하면 된다. 호스트는 두 가지로 붙는다: 호스트에서 초대 한 줄을 실행하거나(join),
노트북이 ssh 로 들어간다. 어느 쪽이든 호스트에 필요한 것은 Docker 뿐이다.

## 빠른 시작

```yaml
# pumasi.yaml  (pumasi.example.yaml 을 복사)
url: https://github.com/OWNER/REPO
classes:
  - {name: linux-s, cpus: 1, memory: 1G}
  - {name: linux-l, cpus: 2, memory: 3G}
hosts:
  - ssh: server1
    budget: {cpus: 4, memory: 6G}
  - ssh: desk1
    desktop: true
    shell: wsl -e sh -s        # WSL 을 쓰는 Windows 호스트만
    budget: {cpus: 6, memory: 9G}
```

```sh
make                                              # dist/pumasi 와 호스트용 linux 빌드
PUMASI_GITHUB_TOKEN=$(gh auth token) ./dist/pumasi up
```

```yaml
# 워크플로
jobs:
  build:
    runs-on: [self-hosted, pumasi-linux-s]
```

`pumasi up` 은 호스트마다 에이전트를 올리고(ssh, 처음 한 번만 업로드) 잡을 기다린다. 한 줄에 사건 하나:

```
11:57:40 class linux-s  runs-on: [pumasi-linux-s, self-hosted]
11:57:40 class linux-l  runs-on: [pumasi-linux-l, self-hosted]
11:57:42 host desk1  agent uploaded (12.0M) and started  budget 6cpu/9.0G
11:57:43 host server1  agent uploaded (12.0M) and started  budget 4cpu/6.0G
11:57:43 listening for jobs (Ctrl-C to stop)
11:57:52 start linux-l on server1  free 4.0cpu/6.0G  (linux-l-server1-1ea93ba5)
11:57:52 start linux-s on desk1  free 6.0cpu/9.0G  (linux-s-desk1-c440fccb)
11:57:52 defer linux-l x2  no host with 2.0cpu/3.0G free (desk1 1.0cpu/4.0G, server1 0.0cpu/0.0G)
11:58:03 ready linux-l-server1-1ea93ba5  6.4s
11:58:03 busy linux-l-server1-1ea93ba5
11:58:39 job large (2) on linux-l-server1-1ea93ba5
11:58:39 done large (2) on linux-l-server1-1ea93ba5  succeeded
11:58:39 stop linux-l-server1-1ea93ba5  job completed
```

다른 터미널에서 `pumasi status` (`--json` 도 있다):

```
HOST     KIND     OS     CPUS  MEMORY     CPU PRESSURE  RUNNERS  STATE
server1  server   linux  4/4   5.0G/6.0G  11.4%         3        ok
desk1    desktop  linux  6/6   8.0G/9.0G  6.3%          4        ok

RUNNER                    CLASS    HOST     STATE  JOB        AGE
linux-l-server1-f60e92c8  linux-l  server1  busy   large (1)  1m41s
linux-s-server1-6676a512  linux-s  server1  busy   small (2)  1m41s
linux-s-server1-939b3c9d  linux-s  server1  busy   small (4)  1m41s
linux-s-desk1-6342a4d5    linux-s  desk1    busy   small (5)  1m41s
linux-l-desk1-2f0de860    linux-l  desk1    busy   large (3)  1m41s
linux-l-desk1-6fef9bd5    linux-l  desk1    busy   large (4)  1m41s
linux-s-desk1-355c28e5    linux-s  desk1    busy   small (3)  1m40s

WAITING  JOBS  WHY
linux-l  1     no host with 2.0cpu/3.0G free (desk1 0.0cpu/1.0G, server1 0.0cpu/1.0G)
linux-s  2     no host with 1.0cpu/1.0G free (desk1 0.0cpu/1.0G, server1 0.0cpu/1.0G)
```

**도는 잡은 죽이지 않는다.** Ctrl-C(또는 노트북이 꺼져도)는 잡이 없는 러너만 지우고, 잡을 돌리는 러너와 에이전트는 호스트에 그대로 둔다.
다시 `pumasi up` 하면 그 러너들을 이어받고, 그사이 끝난 잡의 결과와 cpu·메모리도 장부에 들어온다. 터널이 끊겨도 잡을 가진 러너는 지우지 않는다.

```
12:10:32 stopped  7 jobs still running on the hosts; `pumasi up` picks them up again (0 idle runners removed)
```

`pumasi down` 만 전부 지운다: 호스트의 pumasi 컨테이너·에이전트·`~/.cache/pumasi`, GitHub 의 scale set. 러너가 떠 있으면 목록을 보이고 거부한다(`--force` 로 강행).

## 호스트가 스스로 붙기 (join)

노트북이 호스트에 ssh 로 닿지 못하거나 ssh 설정을 호스트마다 맞추기 싫으면 방향을 뒤집는다. 호스트가 노트북으로 연결을 연다([설계](docs/join.md)).

```yaml
# pumasi.yaml — hosts: 는 없어도 되고, 섞어 써도 된다
url: https://github.com/OWNER/REPO
listen: 0.0.0.0:7411
classes:
  - {name: linux-s, cpus: 1, memory: 1G}
```

```
$ pumasi up            # 다른 터미널에서
$ pumasi invite
Run this on the host that should join.

Linux, macOS:

  curl -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.sh | sh -s -- <token>

Windows (PowerShell, in a window on that machine's desktop):

  & ([scriptblock]::Create((curl.exe -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.ps1 | Out-String))) <token>
```

그 한 줄을 호스트에서 실행하면 호스트가 붙기 전에 하나씩 확인하고, 막힌 것을 이름으로 말한다:

```
ok    resolve 10.0.0.5
ok    tcp 10.0.0.5:7411 (3ms)
ok    controller certificate matches the invite
ok    token accepted, the name desk1 is free
ok    docker 27.3.1 reachable as this user
ok    cgroup v2 with memory and cpu controllers

joined as desk1 (server): lends 6 cpus and 9.0G, runs as the systemd user unit pumasi.service
```

- 기본은 그 머신의 절반을 hostname 으로 빌려준다. 토큰 뒤에 `--name desk1 --cpus 6 --mem 9G --desktop` 로 바꾼다.
- 호스트는 초대 줄의 공개키 지문으로만 controller 를 믿고, controller 는 토큰으로 호스트를 받는다. `pumasi invite --rotate` 는 토큰을 바꾸되 이미 붙은 호스트는 그대로 둔다.
- 연결이 끊기면 호스트가 1~30초 간격으로 다시 건다. 노트북이 꺼져 있어도 도는 잡은 그대로 돌고, 다시 `up` 하면 이어받는다.
- controller 의 판이 바뀌면 호스트가 새 바이너리를 받아 스스로 바꿔 실행한다. 러너는 건드리지 않는다.
- 호스트에서 `pumasi doctor` 는 같은 확인에 에이전트·연결 상태를 더해 보여 주고, `pumasi leave` 는 그 호스트에서 pumasi 를 전부 지운다. `pumasi down` 은 붙어 있는 호스트에 같은 일을 시킨다.
- Mac 과 Windows 도 같은 줄로 붙는다. 거기서는 docker·cgroup 대신 러너 배포판을 받을 수 있는지·디스크·로그인 세션을 확인하고,
  에이전트는 로그인한 사용자의 세션에서 돈다(Mac: LaunchAgent, Windows: 로그온 때 시작). 그래야 잡이 키체인으로 서명하고 창을 띄운다.
- `pumasi status` 와 대시보드의 Hosts 표에 붙은 방식(`VIA`)·마지막 응답(`SEEN`)·왕복 시간(`RTT`)·에이전트 판(`AGENT`)·마지막으로 끊긴 이유(`LAST DROP`)가 나오고, 호스트 유실 알림에도 이유가 붙는다: `desk1 unreachable for 30s (keepalive timeout) — …`.

## Mac 과 Windows 호스트

그 OS 여야 하는 잡(서명, 네이티브 빌드)은 클래스에 `os` 를 적는다. 그 클래스는 같은 OS 의 호스트에만 놓인다.

```yaml
classes:
  - {name: linux-s, cpus: 1, memory: 1G}
  - {name: macos, cpus: 4, memory: 8G, os: macos}     # runs-on: [self-hosted, pumasi-macos]
hosts:
  - {ssh: mac1, desktop: true, budget: {cpus: 8, memory: 16G}}
```

Mac·Windows 호스트에서는 러너가 컨테이너가 아니라 **그 OS 의 프로세스**로 돈다(Docker 가 필요 없다). 컨테이너와 다른 점:

- **격리가 약하다.** 러너는 에이전트를 띄운 사용자로 돈다. 잡은 그 사용자의 홈·키체인·전역 도구를 읽고 바꿀 수 있고, 남긴 것은 남는다.
  러너 디렉터리(`~/.cache/pumasi/runners/<이름>`)와 잡이 남긴 프로세스만 pumasi 가 지운다. **믿는 레포의 잡에만 쓴다.**
- **Mac 은 메모리·CPU 를 강제로 막지 못한다.** 클래스 크기는 배치할 때의 약속일 뿐이다. 넘은 러너는 `ran over its class` 사건으로 알리고 잡은 계속 돈다. 그래서 Mac 호스트의 예산은 넉넉히 남겨 잡는다.
- **잴 수 있는 것만 남긴다.** Mac 은 cpu 초와 메모리 peak 를 재고 디스크 I/O·압박(PSI)은 비워 둔다.
- 러너 배포판은 `image` 의 태그(`:2.338.2`)와 같은 판을 호스트가 처음 한 번 내려받는다(`~/.cache/pumasi/dist`).
- Windows 는 Job Object 로 메모리·CPU 를 강제한다(넘는 할당은 실패한다). 드라이버는 Windows 11 실물에서 확인했다. join 은 Mac 까지 실물로 확인했고 Windows 쪽은 코드와 크로스 빌드까지라, Windows 의 GitHub 잡은 아직이다.

자세한 것은 [Mac 과 Windows 호스트](docs/platforms.md).

## 기존 고정 러너에서 옮기기

오래 떠 있는 셀프호스트 러너의 워크플로우는 호스트의 것을 쓴다: 공용 캐시 디렉터리, 앞 잡이 남긴 산출물,
호스트 사용자의 자격(`~/.npmrc`, ssh 키), 호스트의 docker. 워크플로우를 고치지 않고 옮기려고 클래스에 세 가지를 적을 수 있다.

```yaml
classes:
  - {name: linux-s, cpus: 1, memory: 1G}                                   # 컨테이너. 호스트의 것은 안 보인다
  - {name: cache,   cpus: 2, memory: 3G, mounts: [/srv/ci-cache, "/srv/keys:ro"]}
  - {name: build,   cpus: 4, memory: 8G, driver: native}                   # 호스트 사용자의 프로세스
  - {name: release, cpus: 4, memory: 8G, driver: native, hosts: [server1]} # 그 호스트에만
```

- **`driver: native`** — 러너를 컨테이너가 아니라 에이전트를 띄운 사용자의 프로세스로 띄운다. 호스트의 경로·자격·docker 가
  고정 러너와 똑같이 보인다. Linux 에서는 러너를 systemd user scope(cgroup)에 넣으므로 CPU·메모리 한도, 수치, OOM 사건,
  잡이 남긴 프로세스 정리는 컨테이너 러너와 같다. 그 사용자에게 systemd user 세션(`loginctl enable-linger <사용자>`)과
  cpu·memory 위임이 있어야 한다 — 없으면 한도 없이 띄우지 않고, 대기 사유에 호스트가 말한 이유가 나온다.
  안 적으면 linux 는 `docker`, macos·windows 는 `native` 다.
- **`mounts`** — 컨테이너 러너에 호스트 경로를 같은 경로로 붙인다(`:ro` = 읽기 전용). 그 경로가 모두 있는 호스트에만 놓이고,
  어디에도 없으면 `no host has /srv/ci-cache` 로 기다린다. 컨테이너 안의 사용자(`runner`, uid 1001)가 쓸 수 있게 하는 것은 호스트의 일이다.
  native 클래스에 적으면 붙이지는 않고(이미 보인다) 놓을 호스트만 가린다.
- **`hosts`** — 클래스를 그 호스트에만 놓는다. 앞 잡이 호스트 디렉터리에 쓴 것을 다음 잡이 읽으려면 둘이 같은 머신에 떨어져야 하는데,
  GitHub 은 그것을 정하게 해 주지 않는다. 묶인 호스트가 차면 다른 호스트가 비어 있어도 기다린다.

옮기는 순서:

1. 고정 러너의 라벨 하나를 클래스 하나로 만든다. 우선 `driver: native`, 그 라벨이 머신 한 대였으면 `hosts:` 로 묶는다.
   워크플로우는 `runs-on` 만 바뀐다.
2. 잡 사이에 넘기는 것이 없는 클래스부터 `hosts:` 를 풀어 여러 머신으로 넓힌다. 캐시는 머신마다 생긴다.
3. 호스트의 것을 안 쓰는 잡은 컨테이너 클래스로 옮긴다. 캐시 디렉터리만 필요하면 `mounts`.

native 러너의 격리는 Mac·Windows 와 같이 약하다(위 절). 잡이 호스트 docker 로 띄운 컨테이너는 러너의 cgroup 밖이라
한도에도 수치에도 안 잡힌다. 자세한 것은 [호스트의 디렉터리를 쓰는 잡](docs/host-paths.md).

## 대시보드와 알림

`pumasi up` 이 loopback 에 대시보드를 같이 띄운다(`pumasi open`). 문제·호스트·러너·대기·최근 잡 50개·24시간 추이를 한 화면에 보여 주고, 같은 주소의 `/metrics` 는 Prometheus 용이다. 따로 설치하거나 빌드할 것은 없다.

문제는 사건으로 장부에 남고 `pumasi status` 맨 위 `PROBLEMS` 에 나온다. `notify:` 를 적으면 열릴 때·30분마다·해제될 때 한 줄씩 보낸다([견고함](docs/robustness.md)):

```yaml
notify:
  webhook: https://hooks.example/…   # JSON POST {text, key, kind, subject, state, detail}
  command: my-notify --stdin          # 또는 같은 JSON 을 stdin 으로 받는 명령
```

```
[pumasi] desk1 unreachable for 30s — 5 jobs still running there, no new runners placed
[pumasi] runner linux-s-desk1-78e45f33 died while running small (2) on desk1 (exit code 137); GitHub will fail that job
[pumasi] controller is back: was down 28s, adopted 7 runners, 0 jobs finished meanwhile
```

## 알아 둘 것

- GitHub 토큰은 노트북의 `PUMASI_GITHUB_TOKEN` 에만 있다. 호스트로는 러너 하나짜리 JIT 설정만 간다.
- ssh 호스트의 에이전트는 loopback 에서만 듣고 노트북이 `ssh -L` 로 붙는다. join 호스트의 에이전트는 어디서도 듣지 않고 노트북의 `listen` 포트로 TLS 연결 하나를 연다. 대시보드와 status 는 어느 쪽이든 loopback 에만 열린다.
- 장부(SQLite)와 이벤트 로그는 `~/.local/state/pumasi/` 에 쌓인다. 잡마다 cpu 초·메모리 peak·기동 시간이 남는다.
- Linux 호스트(WSL2 포함)는 Docker 러너, Mac 호스트는 프로세스 러너다. 사람 우선·VM 은 [설계](docs/design.md)의 다음 단계.

## 문서

[문제](docs/problem.md) · [설계](docs/design.md) · [join](docs/join.md) · [견고함](docs/robustness.md) · [Mac·Windows](docs/platforms.md) · [호스트 경로](docs/host-paths.md) · [대시보드](docs/dashboard.md) · [M1 계획](docs/m1-plan.md) · [M1 실측](docs/m1-results.md) · [조사](docs/research/)
