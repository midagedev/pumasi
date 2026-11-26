# pumasi

**Kubernetes 없이, 이미 가진 머신들에, GitHub 의 공식 경로(runner scale set)로 일회용 Actions 러너를 띄운다.**
잡의 크기는 pumasi 가 재서 놓는다 — 사람이 CPU·메모리 숫자를 적거나 고치지 않는다.

- **가진 머신 그대로.** Linux 서버, 사람이 쓰는 Mac·Windows·Linux 데스크톱. 바이너리 하나를 한 머신(controller)에서 실행하고, 나머지(호스트)는 초대 한 줄로 붙는다. 호스트에 열 포트는 없다.
- **잡 하나에 러너 하나.** 잡이 오면 자리가 있는 머신에 러너를 띄우고 끝나면 지운다. 자리가 없으면 잡은 이유와 함께 기다린다.
- **재서 놓는다.** 처음 보는 잡은 짐작으로, 한 번 돈 잡은 잰 크기로 놓는다. 머신이 다른 일로 바쁘면 덜 놓고, 사람이 쓰는 데스크톱은 쓰는 동안 새 잡을 받지 않는다.
- **도는 잡은 죽이지 않는다.** 크기를 넘어도, controller 가 죽어도, 연결이 끊겨도 잡은 호스트에서 끝까지 돈다. 스스로 고친 것도 못 고친 것도 사건으로 남기고 알린다.

## 5분 시작

**필요한 것**

- controller 머신: Go(빌드용), 그리고 호스트들이 이 머신의 포트 하나(기본 7411)에 닿을 수 있을 것(같은 망, VPN, tailnet).
- 호스트: Linux 는 그 사용자가 쓸 수 있는 Docker 와 cgroup v2. Mac·Windows 는 아무것도 필요 없다(러너가 그 사용자의 프로세스로 돈다).
- GitHub 자격: 그 레포에 러너를 등록할 수 있는 토큰(classic PAT 면 `repo`, 조직 단위면 `admin:org`. 레포의 admin 이어야 한다) 또는 GitHub App. 필요한 권한의 전부는 [GitHub 자격](docs/guide.md#github-자격).
  모자라면 `up` 이 `create scale set …: 403` 으로 끝난다. `gh` 를 쓰면 `gh auth refresh -s repo` 뒤 `gh auth token`.
  토큰은 controller 밖으로 나가지 않는다 — 호스트로는 러너 하나짜리 일회용 설정만 간다.

**1. 설정** — `pumasi.yaml` ([예시](pumasi.example.yaml))

```yaml
url: https://github.com/OWNER/REPO
listen: 0.0.0.0:7411                       # 호스트가 붙으러 오는 주소 (기본값)
classes:
  - {name: small}                          # 워크플로우: runs-on: [self-hosted, pumasi-small]
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

joined as desk1 (server): lends what it has free, runs as the systemd user unit pumasi.service
```

- 기본은 hostname 으로 붙고, **그때그때 재서 비어 있는 만큼** 빌려준다. 줄 끝의 `--name desk1` 로 이름을, `--cpus 6 --mem 9G` 로 빌려주는 양의 상한을 정한다.
- 초대 줄은 호스트 하나, 1시간 동안만 통한다(`pumasi invite --uses 5 --ttl 24h`). `-k` 는 위험하지 않다 — `--pinnedpubkey` 가 이 controller 의 키만 받는다.
- 주소가 여럿인 머신이면 `invite` 가 나머지도 알려 준다. 호스트가 닿는 주소를 쓴다.

**4. 잡을 보낸다** — 워크플로우에 `runs-on: [self-hosted, pumasi-small]`.

```
11:58:52 start small on desk1  takes 2.0cpu/4.0G of the free 6.0cpu/9.0G for build (the class's first estimate; …)  (small-desk1-1ea93ba5)
11:58:57 ready small-desk1-1ea93ba5  5.1s
11:59:30 job build on small-desk1-1ea93ba5
12:01:02 done build on small-desk1-1ea93ba5  succeeded
```

첫 러너는 느리다: Linux 호스트는 러너 이미지(수백 MB)를, Mac·Windows 는 러너 배포판을 처음 한 번 받는다. 3분(`provisionTimeout`) 안에 못 뜨면 지우고 다시 띄우고,
같은 호스트에서 세 번 연속이면 그 호스트를 잠시 쉬게 한다(5분마다 다시 본다). 회선이 느리면 호스트에서 `docker pull ghcr.io/actions/actions-runner:2.338.2` 를 먼저 해 둔다.

**끄기** — Ctrl-C 는 controller 만 내린다. 도는 잡은 호스트에서 계속 돌고 다음 `up` 이 이어받는다.
`pumasi down` 이 전부 지운다: 호스트의 러너·에이전트·파일, GitHub 의 scale set. 러너가 떠 있으면 목록을 보이고 거부한다(`--force`).
러너 이미지는 pumasi 가 받은 것만 지운다(원래 있던 것, 다른 컨테이너가 쓰는 것은 남긴다).
호스트 하나만 떼려면 그 호스트에서 `pumasi leave`.

**대신 붙여 주기** — ssh 로 들어갈 수 있는 호스트는 설정에 적어 두면 `up` 이 거기서 초대 줄을 대신 실행한다. 붙은 뒤는 같다.

```yaml
hosts:
  - {ssh: server1}
  - {ssh: desk1, desktop: true, budget: {cpus: 6, memory: 9G}}   # budget 은 상한이다(없으면 비어 있는 만큼). WSL 을 쓰는 Windows 면 shell: wsl -e sh -s
```

그 호스트가 `listen` 주소에 닿지 못하면 `up` 이 `server1 cannot reach this controller at …` 로 말한다.

## 클래스

클래스는 러너의 종류고 GitHub 의 scale set 하나다. 잡은 `runs-on: [self-hosted, pumasi-<이름>]` 으로 고른다. 숫자 없이 이름만 적어도 된다.

```yaml
classes:
  - {name: small}                                     # 컨테이너 러너. 호스트의 것은 안 보인다
  - {name: e2e,    docker: inside, maxDuration: 2h}   # 잡이 자기 docker 로 compose 스택을 띄운다
  - {name: cache,  mounts: [/srv/ci-cache]}           # 호스트 경로를 붙인다(그 경로가 있는 호스트에만)
  - {name: build,  driver: native, hosts: [server1]}  # 호스트 사용자의 프로세스로, 그 호스트에서만
  - {name: macos,  os: macos}                         # Mac·Windows 는 늘 그 사용자의 프로세스
  - {name: deploy, reserve: 1}                        # 다른 클래스가 못 쓰는 자리 하나
  - {name: fenced, limit: {memory: 8G}}               # 묶고 싶을 때만: 8G 를 넘는 잡은 죽는다
```

키 전부(`labels` · `overflow` · `env` · `path` · `cpus`/`memory` …)와 주의할 점은 [안내서: 클래스](docs/guide.md#클래스).
native 러너는 격리가 약하다 — 잡이 그 사용자의 홈·자격을 읽을 수 있으니 믿는 레포의 잡에만 쓴다.

## 크기는 재서 놓는다

- 레포 · 워크플로우 파일 · 잡 이름이 같으면 같은 잡이다. 끝난 실행 최근 10번의 최대에 20% 를 얹은 것이 다음 실행의 크기다(메모리는 페이지 캐시를 뺀 값).
- 처음 보는 잡은 클래스의 첫 추정(기본 2cpu/4G)이나 같은 워크플로우의 다른 잡 크기로 놓고, 아무것도 모르는 잡은 한 호스트에 한꺼번에 나란히 놓지 않는다.
- 호스트의 자리는 그때그때 잰다: 머신에서 러너들이 쥔 것, 다른 프로세스가 쓰는 것, 남겨 두는 메모리를 뺀 것. 메모리 압박이 있으면 새 러너를 놓지 않는다.
- **추정은 한도가 아니다.** 넘은 잡은 끝까지 돌고 다음에는 크게 놓인다. 강제하는 것은 호스트를 지키는 한도(러너 하나가 머신 메모리의 80%)와, 적었을 때만 클래스의 `limit` 이다.

```
$ pumasi status
RUNNER                CLASS  HOST     SLOT  STATE  JOB    AGE     EST          NOW
e2e-server1-d8139867  e2e    server1  1     busy   smoke  23m43s  0.4cpu/3.7G  0.2cpu/3.1G

WAITING  JOBS  LINE  WHY
macos    1     1     no host with 2.0cpu/4.0G free (mac1 in use)

JOB SIZE  WORKFLOW  NEXT RUN     LAST RUN     LARGEST      RUNS
smoke     e2e.yml   0.4cpu/3.7G  0.2cpu/3.0G  0.3cpu/3.1G  10
```

자세히: [안내서: 숫자를 고치지 않는 배치](docs/guide.md#숫자를-고치지-않는-배치).

## 더 하는 것

| | 한 줄로 | 안내서 |
|---|---|---|
| 잡 우선순위 | 자리가 모자랄 때의 줄을 규칙으로(`priority:`). 도는 잡은 밀어내지 않고, 굶는 잡이 없다 | [잡 우선순위](docs/guide.md#잡-우선순위) |
| 고정 러너에서 옮기기 | 클래스에 고정 러너의 `labels` 를 그대로 적으면 워크플로우를 안 고치고 넘어온다. 환경은 `env`·`path`·`{slot}` | [옮기기](docs/guide.md#기존-고정-러너에서-옮기기) |
| 조금씩 옮기기 | `overflow: true` 인 클래스는 받을 수 있는 만큼만 받고, 넘치는 잡은 같은 라벨의 고정 러너·다른 클래스로 흐른다 | [overflow](docs/guide.md#받을-수-있는-만큼만-받기-overflow) |
| 사람 우선 | 데스크톱은 서버가 찬 뒤에 쓰고, 누가 쓰는 동안(입력 15분 안)에는 새 러너를 받지 않는다 | [호스트 종류](docs/guide.md#호스트-종류와-사람-우선) |
| 사건과 알림 | 러너가 죽음·호스트 끊김·메모리 한도·오래 기다림을 웹훅이나 명령으로. controller 가 없어진 것은 호스트가 알린다. `rerun: 1` 이면 우리 쪽 잘못으로 실패한 run 을 다시 돌린다 | [사건과 알림](docs/guide.md#사건과-알림) |
| 대시보드 | `pumasi open` — 문제·호스트·러너·대기 사유·최근 잡·추이. `/metrics` 는 Prometheus | [대시보드](docs/guide.md#대시보드) |
| 계속 켜 두기 | `pumasi service install` — 죽으면 다시 띄우고 재부팅 뒤에도 올라온다(Linux·macOS) | [서비스](docs/guide.md#controller-를-계속-켜-두기) |
| GitHub 자격 | 토큰 또는 GitHub App. 레포 단위면 Administration: Read and write 면 된다 | [GitHub 자격](docs/guide.md#github-자격) |

## 보안 한 줄

**GitHub 자격은 controller 에만 있다.** 호스트에는 러너 하나가 잡 하나를 받을 일회용 등록만 간다. 컨테이너 러너의 잡은 자기 컨테이너만 보고, native 러너의 잡은 그 호스트 사용자가 보는 것을 다 본다.
잡이 controller 의 사용자로 도는 구성(자기 노트북 한 대에 둘 다)은 `up` 이 찾아 경고한다. 전부: [안내서: 보안 모델](docs/guide.md#보안-모델).

## 풀지 못하는 것

- **한 run 의 잡들을 같은 머신에 보내기.** scale set 안에서는 어느 러너가 어느 잡을 받을지 정할 수 없다. 호스트에 띄운 것을 뒤 잡이 이어 쓰는 워크플로우는 호스트마다 클래스를 하나씩 두고(`hosts:`) 앞 잡이 run 전체의 클래스를 고르게 한다.
- **native 잡이 호스트 docker 로 띄운 컨테이너의 사용량.** 러너의 한도에도 수치에도 안 잡히고 호스트 전체 압박으로만 보인다. 컨테이너 클래스는 `docker: inside` 로 안에 넣을 수 있다.
- **Mac 의 메모리·CPU 한도.** 강제할 수단이 없다. 폭주하는 잡에서 Mac 을 지키는 것은 배치가 재서 새 러너를 안 놓는 것뿐이다.
- **호스트 전체의 메모리가 바닥나는 것.** 러너 하나는 한도로 막지만, 여러 러너가 함께 커져 머신이 바닥나면 커널이 고르는 프로세스가 죽는다. 남겨 두는 메모리와 압박 감지가 그 전에 새 러너를 멈출 뿐이다.
- **처음 보는 큰 잡.** 첫 실행은 클래스의 첫 추정으로 놓인다. 워크플로우째로 처음인 잡은 3배를 쥐어 두고 지켜보지만([처음 보는 잡](docs/guide.md#숫자를-고치지-않는-배치)), 첫 추정의 3배보다 크거나 3분 뒤에야 자라는 잡이 몰리면 그 한 번은 호스트가 빡빡해질 수 있다. 큰 잡이 올 클래스는 `cpus`·`memory` 를 크게 적어 둔다.
- **native 러너와 에이전트는 같은 사용자다.** 잡이 에이전트의 가입 자격을 읽을 수 있다. 배포 자격을 쥔 호스트는 믿는 워크플로우의 클래스만 받게 `hosts:` 로 가른다.
- **NAT 너머의 호스트.** 호스트가 controller 의 `listen` 주소에 직접 닿아야 한다. 중계는 없다.
- **잡을 다른 호스트로 옮기기, 도는 잡을 밀어내기.** GitHub 에 그런 길이 없어서, 죽이면 실패한 잡만 남는다.
- VM 격리(Tart, microVM), GitHub 이외의 CI, 클라우드 인스턴스 만들기는 범위 밖이다.

## 문서

[안내서](docs/guide.md) · [지금의 설계](docs/design.md) · [실물로 확인된 것](docs/verified.md) · [조사](docs/research/)
