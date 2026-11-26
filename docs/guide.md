# pumasi 안내서

[README](../README.md) 가 줄여 말한 것의 전부다. 왜 그렇게 만들었는지는 [설계](design.md), 실제로 돌려 본 것은 [확인된 것](verified.md).

## 클래스

클래스는 러너의 종류다(OS, 드라이버, 붙일 경로, 라벨). 크기는 적지 않아도 된다 — [잡의 크기는 pumasi 가 잰다](#숫자를-고치지-않는-배치). 클래스 하나가 GitHub 의 scale set 하나(`pumasi-<이름>`)이고, 잡은 `runs-on` 으로 클래스를 고른다.
모르는 키나 같은 이름의 클래스가 둘이면 `up` 이 거부한다.

| 키 | 뜻 | 기본 |
|---|---|---|
| `name` | `runs-on: [self-hosted, pumasi-<name>]` | 필수 |
| `labels` | scale set 의 라벨을 직접 정한다. 잡의 `runs-on` 라벨이 전부 이 안에 있으면 그 잡을 받는다 — 기존 고정 러너의 라벨을 그대로 적으면 워크플로우를 안 고치고 옮긴다(아래) | `[self-hosted, pumasi-<name>]` |
| `overflow: true` | **받을 수 있는 만큼만 받는다.** 넘치는 잡은 같은 라벨의 다른 러너(고정 러너, 다른 클래스)로 흐른다. [아래](#받을-수-있는-만큼만-받기-overflow) | 없음 — 라벨의 잡을 전부 받고 기다리게 한다 |
| `after: <클래스>` | `overflow` 클래스끼리의 순서. 그 클래스에 자리가 있는 동안 이 클래스는 새 잡을 받지 않는다 | 없음 |
| `cpus`, `memory` | **첫 추정.** 한 번도 재 본 적 없는 잡을 놓을 때 잡아 두는 크기다. 한도가 아니다. 한 번 돌고 나면 그 잡은 잰 크기로 놓인다 | 2, 4G |
| `limit: {cpus, memory}` | 이 클래스의 러너를 이 숫자로 묶는다(Linux·Windows). 메모리를 넘으면 넘은 프로세스가 죽는다 | 없음 — 호스트를 지키는 한도만 |
| `os` | `linux` · `macos` · `windows`. 같은 OS 의 호스트에만 놓인다 | `linux` |
| `driver` | `docker`(컨테이너) 또는 `native`(호스트 사용자의 프로세스 — 호스트의 경로·자격·docker 가 그대로 보인다) | linux 는 `docker`, macos·windows 는 `native` 만 |
| `mounts` | 컨테이너 러너에 호스트 경로를 같은 경로로 붙인다(`/srv/cache`, `/srv/keys:ro`). 그 경로가 모두 있는 호스트에만 놓인다 | 없음 |
| `hosts` | 그 호스트들에만 놓는다. 묶인 호스트가 차면 다른 곳이 비어 있어도 기다린다 | 모든 호스트 |
| `env` | 러너(와 그 잡)의 환경에 더할 변수. 고정 러너가 서비스 unit 이나 `.env` 로 주던 것(`JAVA_HOME`, `DOCKER_HOST`, `LANG`). 값 안의 `{runner}` 는 그 러너의 디렉터리(러너와 함께 지워진다)로, `{slot}` 은 그 호스트에서 지금 떠 있는 같은 클래스 러너들 사이에서 겹치지 않는 번호(1부터, 비어 있는 가장 작은 수)로 바뀐다 | 없음 |
| `path` | native 러너의 `PATH` 앞에 붙일 디렉터리(고정 러너의 `.path`). 컨테이너 클래스에는 못 적는다 | 없음 |
| `reserve` | 이 클래스 러너 N개가 들어갈 자리를 다른 클래스가 못 쓰게 남겨 둔다(배포 잡이 빌드 뒤에 서지 않게) | 0 |
| `maxDuration` | 잡이 이보다 오래 돌면 러너를 멈춘다(`90m`). pumasi 가 도는 잡을 건드리는 유일한 설정이다 | 없음 |
| `docker: inside` | 컨테이너 러너에 자기 docker 데몬을 준다. 잡이 띄운 컨테이너·compose 스택이 클래스 한도와 수치 안에 들어가고 러너와 함께 사라진다. 러너 컨테이너가 **privileged** 가 된다 | 없음 |

```yaml
classes:
  - {name: small}                                              # 컨테이너. 호스트의 것은 안 보인다
  - {name: e2e,     docker: inside, maxDuration: 2h}           # 잡이 compose 스택을 띄운다
  - {name: cache,   mounts: [/srv/ci-cache]}
  - {name: build,   driver: native, hosts: [server1]}          # 고정 러너처럼 호스트를 그대로 쓴다
  - {name: deploy,  reserve: 1}
  - {name: macos,   os: macos}
  - {name: heavy,   cpus: 8, memory: 24G}                      # 처음 보는 잡도 크게 잡고 시작한다
  - {name: fenced,  limit: {memory: 8G}}                       # 8G 를 넘는 잡은 죽는다
```

- 러너는 **잡 하나로 끝난다.** 작업 디렉터리·도구 캐시(`setup-node` 등)는 남지 않는다. 잡 사이에 남길 것은 `mounts` 나 native 클래스의 호스트 디렉터리, 캐시 액션에 둔다.
- `mounts` 가 있으면 작업 디렉터리를 첫 쓰기 가능 mount 안에 둔다 — pnpm·uv·ccache 가 캐시에서 hard link 로 가져올 수 있게.
- `driver: native`(Linux) 는 systemd user 세션과 cpu·memory 위임, linger, 그리고 러너가 뜰 때 읽는 라이브러리(libicu·libssl·libkrb5·zlib·libstdc++ — 러너의 `installdependencies.sh` 가 까는 것)가 필요하다. 없으면 띄우지 않고 대기 사유에 호스트가 이유를 말한다. 붙을 때의 확인도 `note` 한 줄로 말한다.
- native 클래스를 로그인 사용자가 아닌 서비스 계정으로 돌릴 때 러너에게만 보여야 하는 경로는, 그 계정의 systemd 사용자 관리자(`user@<UID>.service`)에 drop-in 으로 `BindPaths=` 를 주면 된다. 에이전트(user unit)와 러너(scope)가 그 마운트를 물려받으므로, 고정 러너의 unit 이 `BindPaths` 로 주던 경로를 호스트 전체를 바꾸지 않고 옮길 수 있다. 그 계정에 linger 가 켜져 있어야 한다.
- **native 러너는 격리가 약하다**(Mac·Windows 는 늘 native). 잡은 호스트 사용자의 홈·자격·전역 도구를 읽고 바꿀 수 있다. 믿는 레포의 잡에만 쓴다.
- 러너 판은 `image:` 의 태그를 따른다(기본 `ghcr.io/actions/actions-runner:2.338.2`). Mac·Windows 는 그 태그에서 판 번호만 읽으므로 **`:latest` 로 적으면 Mac·Windows 러너가 뜨지 못한다.** 판은 사람이 올린다.

## 숫자를 고치지 않는 배치

잡이 얼마나 쓰는지는 사람이 적지 않는다. pumasi 가 재고, 다음에는 잰 대로 놓는다.

- **잡의 크기.** 레포 · 워크플로우 파일 · 잡 이름이 같으면 같은 잡이다(브랜치는 가리지 않는다. 매트릭스 잡은 이름에 값이 들어 있어 따로다).
  끝난 실행 가운데 최근 10번에서 가장 컸던 것에 20% 를 얹은 것이 다음 실행의 크기다. 메모리는 페이지 캐시를 뺀 값(Linux: cgroup 의 anon + shmem + kernel)으로,
  CPU 는 잡이 도는 동안의 평균 코어 수로 잰다. 커진 잡은 바로 다음 실행부터 크게 놓이고, 작아진 잡은 큰 실행이 10번 밖으로 밀려나야 작아진다.
- **처음 보는 잡**은 클래스의 `cpus`·`memory`(없으면 2cpu/4G)로 놓는다. 같은 워크플로우의 다른 잡을 재 본 적이 있으면 그중 가장 큰 크기를 빌린다(첫 추정보다 작게는 안 빌린다).
  워크플로우째로 처음이면 그 크기는 짐작이라, 그런 잡은 **한 호스트에 한꺼번에 나란히 놓지 않는다**: 잡이 3분을 돌 때까지 메모리를 첫 추정의 3배로 쥐어 두고, 그동안 그 호스트는 다음 「모르는 잡」을 3배가 비어 있을 때만 받는다. 3분이 지나면 그 잡이 실제로 쥔 양이 나머지 잡들의 크기가 된다. 대기 사유에 `it watches 1 unmeasured job there …` 로 보인다.
- **호스트의 자리.** 머신 전체에서 러너들이 쥔 것, pumasi 밖의 프로세스가 지금 쓰는 것, 남겨 두는 메모리(머신의 10%, 1 ~ 4G)를 뺀 것이 새 러너가 쓸 수 있는 자리다.
  러너가 쥔 것은 추정과 지금 쓰는 양 중 큰 쪽이다. 다른 일로 바쁜 호스트에는 덜 놓이고, 한가해지면 다시 놓인다. `budget` 은 그 위에 얹는 상한일 뿐이다.
  메모리 압박(Linux PSI 10% 이상, Mac 의 memory pressure)이 있으면 그 호스트에 새 러너를 놓지 않는다.
- **추정을 넘어도 잡을 죽이지 않는다.** 추정은 배치가 세는 숫자일 뿐 한도가 아니다. 넘은 잡은 끝까지 돌고, 그동안 그 호스트에 새 러너가 덜 놓이고, 다음 실행은 큰 크기로 놓인다.
- **호스트를 지키는 한도**는 따로 있다(Linux·Windows): 러너 하나가 머신 메모리의 80%(`budget` 의 memory 가 더 작으면 그 값)를 넘으면 넘은 프로세스가 죽는다.
  잡의 크기와는 상관없는 값이고, 한 잡이 폭주해 그 머신의 다른 잡을 전부 죽이는 것을 막는다. CPU 는 기본으로 묶지 않는다(바쁘면 느려질 뿐이다).
  그 한도에 걸린 잡은 `out-of-memory` 사건이 되고, 다음 실행은 한도의 1.5배를 쥐고 그만큼 비어 있는 호스트에 놓인다(없으면 사유와 함께 기다린다).
  더 좁게 묶고 싶을 때만 클래스에 `limit:` 를 적는다.

보이는 곳: `pumasi status` 의 `EST`(배치가 센 크기)·`NOW`(지금 쓰는 양)와 맨 아래 `JOB SIZE` 표(다음 실행의 크기 · 지난 실행 · 최근 최대 · 실행 수), 대시보드의 Job sizes.
추정이 4분의 1 넘게 움직이면 `up` 의 로그에 한 줄이 남는다:

```
start grow on vm1  takes 0.4cpu/1.8G of the free 6.4cpu/22.8G for build (learned from 3 runs, the largest 0.3cpu/1.4G)
estimate build: 0.4cpu/1.8G -> 0.4cpu/3.2G after a run that held 2.7G (learned from 4 runs, the largest 0.3cpu/2.7G)
```

**전 판에서 올리면 달라지는 것.** `cpus`·`memory` 는 한도였고 필수였다. 이제 첫 추정이고 없어도 된다 — 그 숫자로 러너를 묶던 것을 그대로 두려면 `limit: {cpus: …, memory: …}` 로 옮겨 적는다.
호스트의 `budget` 과 초대 줄의 `--cpus`·`--mem` 은 필수에서 상한으로 바뀌었고, 초대 줄의 기본은 "머신의 절반"에서 "비어 있는 만큼"이 되었다(이미 붙은 호스트는 다시 붙일 때까지 전의 값을 상한으로 쥐고 있다).
`up` 은 뜰 때 클래스마다 지금의 뜻을 한 줄로 말한다.

## 잡 우선순위

자리가 모자랄 때 누구에게 먼저 줄지를 규칙으로 정한다. 자리가 넉넉하면 아무 일도 하지 않는다. **도는 잡은 건드리지 않는다**(밀어내지 않는다).

```yaml
priority:                                   # 위에서부터, 처음 맞는 규칙이 그 잡의 등급이다
  - {match: {workflow: "deploy-*.yml"}, level: high}
  - {match: {event: schedule}, level: low}
  - {match: {class: e2e, branch: "release/*"}, level: high}
# priorityHeadStart: 30m                    # 기본값
```

- `match` 의 키: `class`, `event`(push · pull_request · schedule · workflow_dispatch …)는 그대로 비교하고, `workflow`(파일 이름) · `job`(GitHub 이 보여 주는 잡 이름) · `repo` · `ref` · `branch` 는 glob 이다. 적은 키가 모두 맞아야 한다.
- `level` 은 `high` · `normal` · `low`. **high 는 30분(`priorityHeadStart`) 먼저 온 것으로, low 는 30분 늦게 온 것으로 친다.** 줄은 그렇게 친 시각 순이다.
  그래서 굶는 잡이 없다: 30분 넘게 기다린 normal 잡은 방금 온 high 잡보다 앞이고, low 잡도 30분이 지나면 새로 온 normal 잡보다 앞이다.
- **바꾸면 바로 적용된다.** 돌고 있는 `up` 이 설정 파일이 바뀐 것을 보고 `priority` 와 `priorityHeadStart` 만 다시 읽는다(다음 평가, 10초 안. 로그에 `priority: 3 rules read again …`). 파일이 틀리면 전의 규칙을 그대로 쓰고 경고한다. 다른 키는 `up` 을 다시 띄워야 바뀐다.
- `pumasi priority` 가 규칙과 지금 기다리는 잡의 줄을 보여 준다. `pumasi status` 의 `WAITING` 은 줄 순서대로 나오고 `LINE` 칸이 그 자리에 선 이유를 말한다.
- 높은 잡이 자리가 없어 기다리는 동안, 그 잡이 갈 수 있는 호스트에는 그보다 낮은 잡을 새로 놓지 않는다(같은 등급끼리는 전처럼 작은 잡이 빈틈을 채운다).

**클래스 안에서는 순서를 바꾸지 못한다.** GitHub 은 scale set(클래스) 안의 잡을 온 순서대로 러너에게 주고, 어느 러너가 어느 잡을 받을지는 정할 수 없다([잰 것](verified.md#github-쪽에서-잰-것)).
그래서 규칙이 실제로 바꾸는 것은 **어느 클래스가 러너를 먼저 받느냐**다. 같은 클래스에서 high 잡보다 앞에 선 잡들은 그 잡과 함께 당겨진다 — 그 잡이 차례를 받으려면 앞의 잡이 먼저 나가야 하기 때문이다:

```
WAITING  JOBS  LINE                                                                   WHY
build    2     1: ahead of deploy in its class, which is high by rule 1 (job: deploy)  no host with 2.0cpu/4.0G free (…)
nightly  1     2                                                                       no host with 2.0cpu/4.0G free (…)
```

배포처럼 꼭 먼저 가야 하는 잡은 자기 클래스(`runs-on`)를 따로 주는 것이 가장 분명하다. 자리를 아예 남겨 두려면 `reserve`.

## 기존 고정 러너에서 옮기기

워크플로우를 고치지 않고 옮길 수 있다. 클래스에 고정 러너의 라벨을 그대로 적는다:

```yaml
classes:   # 워크플로우는 runs-on: [self-hosted, Linux, X64, ci-pool] 그대로
  - name: shared
    cpus: 4
    memory: 8G
    driver: native
    labels: [self-hosted, Linux, X64, ci-pool]
    env: {JAVA_HOME: /usr/lib/jvm/java-21, DOCKER_HOST: unix:///run/user/1000/docker.sock}
    path: [/opt/tools/bin]
```

- 잡의 `runs-on` 이 그 라벨의 부분집합이면(`[self-hosted, ci-pool]`) 온다. 대소문자는 가리지 않는다. `self-hosted`·OS·아키텍처 라벨도 그냥 적으면 된다.
- **기본은 섞여 돌지 않는다.** 그 라벨에 맞는 scale set 이 생기는 순간부터 그 잡은 전부 scale set 으로 오고, 같은 라벨의 고정 러너는 idle 이어도 잡을 받지 못한다.
  `pumasi up` 이 꺼져 있거나 자리가 없으면 잡은 고정 러너로 넘어가지 않고 기다린다. 그러니 옛 러너를 하나씩 멈추며 옮기는 것이 아니라 **라벨 단위로 한 번에 넘어온다.**
  고정 러너를 켜 둔 채 감당하는 만큼만 가져오려면 클래스에 `overflow: true` 를 적는다([아래](#받을-수-있는-만큼만-받기-overflow)).
- **되돌리기는 `pumasi down`**(scale set 삭제)이다. 그 뒤의 잡은 고정 러너가 바로 받고, 지우는 순간 기다리던 잡은 3 ~ 4분 뒤에 고정 러너로 간다. 넘어오는 동안 고정 러너는 켜 둔 채로 둔다.
- **라벨이 넓으면 남의 잡도 온다.** 부분집합이면 오므로 `[self-hosted, Linux, X64, ci-pool]` 인 클래스는 `runs-on: self-hosted` 나 `[self-hosted, Linux]` 만 적은 잡도 전부 가져간다.
  `up` 이 시작할 때 그 조합을 한 줄로 경고하고, 그런 잡이 실제로 오면 잡 이름과 함께 한 번 더 말한다. 그런 워크플로우가 있으면 먼저 라벨을 좁힌다.
- 고정 러너가 주던 환경은 클래스의 `env`·`path` 로 준다. native 러너의 잡은 **에이전트 서비스의 환경**을 물려받는다 — 초대 줄을 실행한 셸의 환경이 아니다
  (Mac 은 그때의 `PATH` 만 가져가고, Linux 는 systemd user 의 기본 `PATH` 다). 러너마다 따로 두던 것은 `{runner}` 로: `env: {HOME: "{runner}", GRADLE_USER_HOME: "{runner}/gradle"}`.
- **잡 사이에 남아야 하는 러너별 디렉터리**(홈, gradle home, 캐시)는 `{slot}` 으로: `env: {HOME: "/srv/ci-home/{slot}", GRADLE_USER_HOME: "/srv/ci-home/{slot}/.gradle"}`.
  `{runner}` 는 러너와 함께 지워지는 자리, `{slot}` 은 남는 자리다. 번호는 러너가 사라지면 풀려 다음 러너가 다시 쓰므로 한 자리를 한 번에 한 러너만 쓴다.
  native 러너면 번호가 든 디렉터리(`/srv/ci-home/3`)가 없을 때 호스트 사용자로 만들고(못 만들면 러너를 띄우지 않고 이유를 말한다), 지우지는 않는다.
  컨테이너 러너에서는 `env` 값만 바뀐다 — 경로는 컨테이너 안의 것이다. `pumasi status` 의 SLOT 칸에 번호가 보인다.
- 라벨은 scale set 을 만들 때 정해진다. 바꾸려면 `pumasi down` 뒤 다시 `up` 하거나 클래스 이름을 바꾼다(`up` 이 그렇게 말한다).
- 고정 러너처럼 호스트의 경로·자격·docker 를 쓰는 잡은 `driver: native` 로 시작하고, 잡끼리 호스트 디렉터리로 넘기는 것이 있으면 `hosts:` 로 한 머신에 묶는다.
  넘기는 것이 없는 클래스부터 `hosts:` 를 풀고, 호스트를 안 쓰는 잡은 컨테이너 클래스로 옮긴다. 일회용 러너라서 달라지는 것은 [설계](design.md#고정-러너에서-옮길-때-달라지는-것).

## 받을 수 있는 만큼만 받기 (`overflow`)

기본으로 클래스는 자기 라벨의 잡을 **전부** 받고, 자리가 없으면 기다리게 한다. `overflow: true` 인 클래스는 **지금 놓을 수 있는 만큼만** 받는다:
GitHub 에 메시지를 물을 때마다 「이 scale set 이 쥘 수 있는 잡 수」(쥐고 있는 잡 + 지금 더 놓을 수 있는 수)를 같이 보내고, GitHub 은 그 수를 넘는 잡을 이 클래스에 배정하지 않는다.
넘치는 잡은 같은 라벨을 듣는 다른 러너가 있으면 그리로 가고(재 본 것: 1 ~ 3초), 없으면 GitHub 에서 기다리다가 자리가 나면 온다(재 본 것: 앞 잡이 끝나고 6초 뒤).
「더 놓을 수 있는 수」는 그 클래스가 최근에 돌린 잡들 가운데 가장 큰 크기로 센다. 자리가 바뀌면 묻던 것을 거두고 새 수로 다시 묻는다.

```yaml
classes:
  # (1) 점진 전환: 고정 러너는 그대로 켜 두고, pumasi 가 감당하는 만큼만 가져온다. 호스트를 늘리는 대로 더 가져온다.
  - {name: shared, driver: native, labels: [self-hosted, Linux, X64, ci-pool], overflow: true}

  # (2) 한 라벨을 두 클래스가 나눠 받기: 워크플로우는 runs-on: [self-hosted, build] 하나.
  #     linux 에 자리가 있는 동안은 linux 가 받고, 차면 mac 이 받는다.
  - {name: linux, labels: [self-hosted, build], overflow: true}
  - {name: mac,   labels: [self-hosted, build], os: macos, overflow: true, after: linux}
```

쓰임 셋:

1. **점진 전환.** 고정 러너를 끄지 않고 라벨의 잡 일부만 가져온다. 호스트가 하나도 안 붙어 있으면 0개를 받고 잡은 전부 고정 러너로 간다.
2. **한 라벨을 두 클래스가.** OS 나 호스트 묶음이 다른 두 클래스가 같은 `runs-on` 을 나눠 받는다. `after:` 가 없으면 어느 쪽이 받을지는 GitHub 이 고른다
   (재 본 것: 둘 다 자리가 있을 때 하나씩 보낸 잡 6개가 모두 한쪽으로 갔다. 기준은 모른다). `after:` 를 적으면 앞 클래스가 찬 뒤에만 뒤 클래스가 받는다(재 본 것: 앞 클래스가 찬 0.3초 뒤 다음 잡이 뒤 클래스로).
3. **고정 러너를 넘침 받이로.** pumasi 가 꽉 찼을 때만 고정 러너(또는 GitHub 호스트 러너가 아닌 다른 자체 러너)가 받는다.

잃는 것:

- **안 받은 잡은 pumasi 에 없다.** 배정되지 않은 잡은 GitHub 의 통계에도 실려 오지 않는다. 대기 수·대기 사건·[우선순위](#잡-우선순위)는 이 클래스가 **받은 잡**에만 있다.
  `pumasi status` 는 그런 클래스를 `OVERFLOW` 표에 따로 보인다(`TAKES` 지금 쥘 수 있는 수, `HOLDS` 쥐고 있는 수).
- **받을 러너가 달리 없으면 잡은 GitHub 에서 보이지 않게 기다린다.** 같은 라벨의 다른 러너가 없는 클래스에 `overflow` 를 켜면 얻는 것 없이 대기만 안 보이게 된다.
- **`pumasi up` 이 꺼져 있으면 바로 흐르지 않는다.** 수를 말해 줄 controller 가 없는 동안 잡은 고정 러너가 idle 이어도 `queued` 로 기다렸고, 5분이 지나서야 고정 러너로 갔다(한 번 재 본 것). controller 를 [서비스로](#controller-를-계속-켜-두기) 켜 두고, 길게 내릴 때는 `pumasi down`(scale set 삭제)을 쓴다.
- **수는 물을 때의 것이다.** 묻고 난 뒤 다른 클래스가 자리를 가져가면 받은 잡이 잠깐 여기서 기다릴 수 있고, 처음 보는 잡이 생각보다 커도 그렇다.

## 호스트 종류와 사람 우선

| | Linux | Mac | Windows |
|---|---|---|---|
| 러너 | docker 컨테이너(또는 native) | 사용자의 프로세스 | 사용자의 프로세스(Job Object) |
| 호스트를 지키는 메모리 한도, 클래스의 `limit` | 강제 (넘으면 그 프로세스가 죽고 OOM 사건) | **강제 못 함** — 배치가 재서 피할 뿐이다 | 강제 (넘는 할당이 실패) |
| 에이전트를 지키는 것 | systemd user unit | 로그인한 사용자의 LaunchAgent | 로그온 때 뜨는 프로세스 |
| join 기본 종류 | 화면 세션이 있으면 desktop, 없으면 server | desktop | desktop |

- **server** 는 자리가 있는 대로 먼저 채운다. **desktop** 은 서버가 찬 뒤에 쓰고, 누가 쓰고 있는 동안(마지막 키보드·마우스 입력 15분 안)에는 새 러너를 받지 않는다. 도는 잡은 끝까지 돈다. 설정은 없다.
- 종류는 초대 줄 끝의 `--server` / `--desktop` 으로 바꾼다. 아무도 앉지 않는 Mac mini 는 `--server` 로 붙인다.
- Mac 에서는 사람이 있는 동안 도는 러너를 background 우선순위로 내린다. Mac 은 한도를 걸 수단이 없으니, 다른 것과 같이 쓰는 Mac 은 `--cpus`·`--mem` 으로 상한을 둔다.
- Mac·Windows 의 초대 줄은 그 머신의 화면에 로그인한 세션에서 실행한다. 그래야 잡이 키체인으로 서명하고 창을 띄우며, 재부팅 뒤에도 에이전트가 올라온다.
- 호스트의 디스크 여유가 10% 아래면 그 호스트에 새 러너를 놓지 않는다(`diskMinFree`). 디스크가 늘 차 있는 노트북은 이 값에 걸린다.

```
$ pumasi status
HOST     KIND     OS     HELD         FREE          CAP        CPU PRESSURE  RUNNERS  STATE   SEEN    RTT  AGENT         LAST DROP
server1  server   linux  0.4cpu/3.7G  6.1cpu/18.2G  auto       0.6%          1        ok      1s ago  8ms  45fd4511f018  -
mac1     desktop  macos  0.0cpu/0.0G  9.8cpu/10.0G  any/10.0G  -             0        in use  1s ago  1ms  6be4c29a87b3  keepalive timeout

RUNNER                CLASS  HOST     SLOT  STATE  JOB    AGE     EST          NOW
e2e-server1-d8139867  e2e    server1  1     busy   smoke  23m43s  0.4cpu/3.7G  0.2cpu/3.1G

WAITING  JOBS  LINE  WHY
macos    1     1     no host with 2.0cpu/4.0G free (mac1 in use)
```

## 사건과 알림

pumasi 는 도는 잡을 건드리지 않고(예외: `maxDuration`, `down --force`), 스스로 고친 것도 못 고친 것도 사건으로 남긴다. 열린 사건은 `pumasi status` 맨 위 `PROBLEMS` 에 나온다.

| 일어난 일 | pumasi 가 하는 것 |
|---|---|
| controller 가 죽거나 꺼짐 | 잡은 호스트에서 계속 돈다. 다시 뜨면 이어받고 `controller is back: was down 28s, adopted 7 runners`. 5분 넘게 안 돌아오면 **호스트가 대신 알린다**(아래) |
| 호스트 연결 끊김·에이전트 죽음 | 그 호스트에 새 러너를 놓지 않는다. 도는 잡은 그대로. 2분 넘으면 사건(끊긴 이유와 함께), 돌아오면 해제 |
| 호스트 재부팅, 잡 도중 러너가 죽음 | 사건 + 잃은 잡 목록. `rerun: 1` 이면 그 run 의 실패한 잡을 한 번 다시 돌린다 |
| 러너가 못 뜸 | 지우고 다시 배치. 같은 호스트에서 세 번 연속이면 그 호스트 격리, 5분마다 점검 |
| 메모리 한도에 걸림 | `build hit the memory limit that protects the host (25.6G) on server1`. 다시 돌리지 않는다. 그 잡의 다음 실행은 더 큰 크기로 놓인다. 클래스의 `limit` 에 걸린 것이면 `hit class fenced's memory limit (8.0G)` |
| 잡이 추정보다 많이 씀 | 사건이 아니다. 잡은 끝까지 돌고 다음 실행의 크기가 커진다(로그의 `estimate …` 한 줄) |
| 잡이 10분 넘게 기다림 | 사건 + 사유(`no host with 2cpu/3G free …`) |
| 지우지 못한 파일(잡이 컨테이너로 남긴 root 의 파일) | 러너는 지우고 그 디렉터리는 치워 둔 뒤 `leftover` 사건이 경로를 말한다. 지우는 것은 사람이 한다 |

```yaml
notify:
  webhook: https://hooks.example/…   # JSON POST {text, key, kind, subject, state, detail}
  command: my-notify --stdin          # 또는 같은 JSON 을 stdin 으로 받는 명령
  # away: 5m                          # controller 에 이만큼 닿지 못한 호스트가 webhook 에 알린다 (0 = 호스트에 주지 않음)
rerun: 1                              # 우리 쪽 잘못으로 실패한 run 을 한 번 다시 돌린다 (기본 0 = 알리기만)
```

**controller 가 없어진 것은 호스트가 알린다.** 죽은 controller 는 말할 수 없으므로, `notify.webhook` 이 있으면 controller 가 붙어 있는 호스트 가운데 둘을 골라 웹훅 주소를 준다.
그 호스트는 controller 에 5분(`notify.away`) 닿지 못하면 한 번, 둘째 호스트는 10분 뒤에 한 번 말하고, 다시 닿으면 말했던 호스트만 한 번 더 말한다:

```
[pumasi] server1 has not reached the pumasi controller ci-box at 10.0.0.5:7433 for 5m2s (connection refused): new jobs of build, deploy wait until it is back, running jobs go on
[pumasi] server1 reaches the pumasi controller at 10.0.0.5:7433 again; it was out of reach for 12m40s
```

- 호스트가 몇이든 알림은 많아야 둘이다. 하나만 두면 그 호스트가 controller 와 같이 내려갔을 때 아무도 말하지 않는다.
- `up` 을 일부러 멈춘 것도 5분이 지나면 알린다(그동안 새 잡은 기다린다). `down` 은 호스트를 떼므로 알리지 않는다.
- 웹훅 주소는 그 두 호스트의 `~/.cache/pumasi/away.json`(600)에 있다. native 러너의 잡은 그 사용자라 읽을 수 있다. 주고 싶지 않으면 `notify: {away: 0}`.
- `notify.command` 는 controller 머신의 명령이라 호스트에 가지 않는다. 명령만 적혀 있으면 `up` 이 controller 부재를 알릴 길이 없다고 한 줄 말한다.
- 호스트 자신의 네트워크가 끊긴 경우에는 웹훅에도 닿지 못한다. 닿게 되면 그때 말하고, 그 전에 controller 가 돌아오면 아무 말도 하지 않는다.

사건은 열릴 때, 30분마다, 해제될 때 한 줄씩 간다. 장애마다의 감지·복구는 [설계](design.md#견고함), 실제로 일으켜 본 결과는 [확인된 것](verified.md).

## 대시보드

`pumasi up` 이 loopback 에 웹 화면을 같이 띄운다(`pumasi open`). 문제 · 호스트 · 러너 · 대기와 사유 · 최근 잡 50개(대기·실행 시간, cpu 초, 메모리 peak, 로그 끝) · 24시간 추이를 한 화면에 보여 준다.
같은 주소의 `/metrics` 는 Prometheus 용이다. 읽기 전용이고 밖으로 열지 않는다.

## controller 를 계속 켜 두기

```sh
PUMASI_GITHUB_TOKEN=$(gh auth token) ./dist/pumasi service install    # -config pumasi.yaml
./dist/pumasi service status
./dist/pumasi service remove
```

`service install` 은 `pumasi up` 을 그 OS 의 서비스 관리자에게 맡긴다: **죽으면 다시 띄우고, 재부팅 뒤에도 올라온다.** `up` 은 다시 떠도 장부와 호스트의 상태로 이어받으므로 잡은 그 사이에도 돈다.

| | Linux | macOS |
|---|---|---|
| 무엇으로 | systemd user unit `pumasi-controller.service` (`Restart=always`) | LaunchAgent `dev.pumasi.controller` (`KeepAlive`) |
| 죽은 뒤 | 5초 뒤 다시 | 5초 안에 다시 |
| 재부팅 뒤 | linger 가 켜져 있으면 부팅 때. 꺼져 있으면 `install` 이 `loginctl enable-linger <사용자>` 를 하라고 말한다 | 그 사용자가 화면에 로그인할 때(자동 로그인이면 부팅 때) |
| 로그 | `journalctl --user -u pumasi-controller` | `<상태 디렉터리>/up.log` |

- **토큰**은 `install` 할 때 환경 변수에서 한 번 읽어 `<상태 디렉터리>/github-token`(모드 600)에 둔다. 유닛·plist·프로세스 인자·서비스의 환경에는 없다. 남이 읽을 수 있는 모드면 `up` 이 쓰지 않고 그렇게 말한다. `service remove` 가 지운다.
  토큰을 바꾸려면 새 토큰으로 `service install` 을 다시 한다.
- controller 가 **죽었다 다시 뜬 뒤 30 ~ 90초**는 새 잡을 못 받는다: 죽은 controller 의 메시지 세션을 GitHub 이 그만큼 붙들고 있다([잰 것](verified.md#github-쪽에서-잰-것)). 그동안 온 잡은 기다렸다가 돈다. 멈추라고 해서 멈춘 경우(`SIGTERM`, 판 교체)는 세션을 닫고 나가므로 바로 받는다.
- 서비스가 도는 동안 손으로 띄운 `pumasi up`·`pumasi down` 은 거절된다(한 상태 디렉터리에 controller 하나). `down` 은 `service remove` 다음에 한다.
- `dist/` 는 통째로 둔다(호스트가 거기서 자기 판을 받는다). 서비스에는 ssh agent 가 없으니 `hosts: - ssh:` 는 agent 없이 쓸 수 있는 키가 있어야 한다 — 아니면 `pumasi invite` 로 붙인다.
- Windows 에는 이 명령이 없다. 셸에서 `pumasi up &` 로 띄우면 셸이 우선순위를 낮추므로(zsh: nice 5) `up` 이 경고한다.

## GitHub 자격

pumasi 가 GitHub 에 하는 일은 셋이다: scale set 과 러너 등록을 만들고 지운다, 기다리는 잡의 run 이 끝났는지 본다, (`rerun` 을 켰으면) 실패한 잡을 다시 돌린다. 그만큼의 권한만 주면 된다.

| | 레포 단위 (`url:` 이 레포) | 조직 단위 (`url:` 이 조직) |
|---|---|---|
| GitHub App | Repository → **Administration: Read and write**, Metadata: Read-only | Organization → **Self-hosted runners: Read and write**, Repository → Metadata: Read-only |
| fine-grained PAT | Repository → **Administration: Read and write** | Organization → **Self-hosted runners: Read and write**, Repository → Administration: Read |
| classic PAT | `repo` | `admin:org` |
| 덧붙일 것 | Repository → **Actions: Read-only** (run 이 끝났는지 보기). `rerun: 1` 이상이면 **Actions: Read and write** | 같음 |

- 러너 등록 권한의 표는 GitHub 문서 「Authenticating ARC to the GitHub API」와 같다(같은 scale set API 다). Actions 권한이 없으면 pumasi 는 돌지만, 취소된 run 의 잡을 기다리는 줄에서 바로 못 지우고(`run status` 경고) 다시 돌리기가 실패한다. classic PAT 의 `repo` 에는 다 들어 있다.
- **토큰**: `PUMASI_GITHUB_TOKEN`. 서비스로 띄우면 `<상태 디렉터리>/github-token`(600).
- **GitHub App**: 사람의 토큰 대신 App 의 설치로 인증한다 — 한 시간짜리 설치 토큰을 pumasi 가 그때그때 받아 쓴다.

  ```yaml
  githubApp:
    id: Iv23liXXXXXXXXXXXXXX            # App 의 Client ID (App ID 숫자도 된다)
    installationId: 12345678
    privateKeyFile: ~/.config/pumasi/app.pem   # 경로만 적는다. 설정 파일 기준 상대 경로도 된다
  ```

  환경 변수로도 줄 수 있다: `PUMASI_GITHUB_APP_ID`, `PUMASI_GITHUB_APP_INSTALLATION_ID`, `PUMASI_GITHUB_APP_PRIVATE_KEY_FILE`(서비스에는 설정 파일로만).
  키는 **파일 경로로만** 받는다. 내용은 메모리에만 있고 인자·로그·상태 디렉터리에 남지 않는다. 남이 읽을 수 있는 모드(600 이 아닌)인 키 파일은 쓰지 않고 그렇게 말한다. App 과 `PUMASI_GITHUB_TOKEN` 을 같이 주면 고르지 않고 끝난다.
- App 만들기(가장 짧은 길): GitHub → Settings → Developer settings → GitHub Apps → New GitHub App. 이름·홈페이지 URL 만 채우고 **Webhook 의 Active 를 끈다**. 위 표의 권한을 준다 → Create.
  만든 화면에서 Client ID 를 적고 「Generate a private key」로 `.pem` 을 받아 `chmod 600` 한다. 「Install App」으로 그 레포(조직)에 설치하면 주소가 `…/settings/installations/<installationId>` 다.

## 보안 모델

**자격은 controller 에만 있다. 호스트에는 러너 하나가 잡 하나를 받을 일회용 등록만 간다.**

| 무엇 | 어디에 | 누가 읽나 |
|---|---|---|
| GitHub 토큰 또는 App 의 개인 키 | controller 프로세스(환경 변수 / 키 파일을 읽은 메모리), 서비스로 띄웠으면 `<상태 디렉터리>/github-token`(600), App 이면 설정이 가리키는 키 파일(600) | controller 를 띄운 사용자 |
| controller 의 TLS 키 `join.key`, 초대 토큰, 가입한 호스트의 비밀값 해시 | 상태 디렉터리(700, 파일 600) | 〃 |
| 호스트의 가입 자격(`join.json`: controller 주소·지문, 호스트 id·비밀값) | 그 호스트의 `~/.cache/pumasi/`(700) | 에이전트의 사용자 |
| 알림 웹훅 주소 | controller 설정, 그리고 [부재를 알릴 호스트 둘](#사건과-알림)의 `away.json`(600) | 〃 |
| 러너의 일회용 등록(JIT 설정) | 그 러너의 환경. 잡 하나를 받으면 끝난다 | 그 러너와 그 잡 |

잡이 볼 수 있는 것은 러너를 무엇으로 띄우느냐에 달렸다.

- **컨테이너 러너**(Linux 기본): 자기 컨테이너 안. 호스트의 파일은 클래스가 `mounts` 로 준 것만 본다. `docker: inside` 는 그 컨테이너를 privileged 로 띄우므로 호스트의 경계로 치지 않는다.
- **native 러너**(Mac·Windows, Linux 의 `driver: native`): 에이전트 사용자의 프로세스다. 그 사용자의 파일을 다 본다 — 호스트의 가입 자격, 같은 호스트의 다른 러너 디렉터리, 그 사용자의 ssh 키·키체인, 그리고 Linux 에서는 docker(= 그 머신의 모든 파일).
- 어느 쪽이든 잡은 **GitHub 토큰에 닿지 못한다 — controller 와 다른 머신에서 도는 한.** 호스트의 가입 자격으로 할 수 있는 것은 그 호스트 행세(그 호스트에 놓일 러너를 받는 것)까지다. 다른 호스트에 무엇을 시키거나 controller 의 것을 읽는 길은 없다: 호스트는 controller 로 걸기만 하고, 그 포트는 가입·바이너리 받기·세션만 받는다.

**controller 를 둘 곳**: 잡이 controller 의 사용자로 돌지 않는 머신. 지켜지지 않는 경우를 `up` 이 찾아 말한다:

```
warning: jobs of class mac on desk1 can read this controller's GitHub token and keys: desk1 is this machine, and its runners are processes of the user who runs this controller. …
```

- 걸리는 경우: controller 와 같은 머신의 호스트에 ① native 러너가 controller 의 사용자로 돈다 ② (Linux) native 러너가 다른 사용자로 돌지만 그 사용자가 docker 를 쓴다 ③ `docker: inside` 클래스가 놓인다 ④ 클래스의 `mounts` 가 상태 디렉터리를 덮는다. 같은 머신의 보통 컨테이너 러너, 다른 사용자의 Mac·Windows 러너는 걸리지 않는다.
- **막지 않고 경고한다.** 자기 노트북 한 대에서 controller 와 호스트를 같이 돌리는 것이 pumasi 를 처음 써 보는 길이고, 워크플로우가 전부 자기 것이면 문제가 없다. 그 러너에 닿는 워크플로우를 믿을 수 있는지(fork 의 pull request, 외부 기여자)는 pumasi 가 알 수 없다.
  경고는 `up` 로그, `pumasi status` 의 `WARNINGS`, 대시보드에 그 상태가 이어지는 동안 계속 보인다. 판단했으면 `jobsMayReadCredentials: true` 로 끈다.
- 피하는 법: controller 를 잡이 안 도는 머신에 두거나, 그 머신의 호스트에는 `hosts:` 로 믿는 클래스만 보낸다. 자격은 [필요한 권한](#github-자격)만 가진 것을 쓴다 — 사람의 classic PAT 보다 그 레포에만 설치한 GitHub App 이 새도 잃는 것이 적다.
- 판정은 「같은 부팅의 같은 머신인가」(부팅 id 의 해시)와 사용자 id 로 한다. 같은 물리 머신 위의 VM·WSL 은 다른 머신으로 본다 — 호스트 OS 의 파일을 공유 폴더로 물려 둔 VM 은 이 경고가 잡지 못한다.
