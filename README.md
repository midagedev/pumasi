# pumasi

셀프호스트 GitHub Actions 러너를 이미 가진 머신들(Linux 서버, 사람이 쓰는 데스크톱의 WSL)에 나눠 띄운다.
호스트마다 빌려줄 상한을 정해 두면, 잡이 올 때 자리가 있는 곳에 일회용 러너를 띄우고 끝나면 지운다.
노트북에서 바이너리 하나를 실행하면 된다. 호스트에는 ssh 와 Docker 만 있으면 되고 설치할 것이 없다.

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
HOST     KIND     CPUS  MEMORY     CPU PRESSURE  RUNNERS  STATE
server1  server   4/4   5.0G/6.0G  11.4%         3        ok
desk1    desktop  6/6   8.0G/9.0G  6.3%          4        ok

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

Ctrl-C 는 러너와 에이전트를 지우고 scale set 은 남긴다(다음 `up` 이 바로 뜬다).
`pumasi down` 은 호스트의 pumasi 컨테이너·에이전트·`~/.cache/pumasi` 와 GitHub 의 scale set 까지 지운다.

## 알아 둘 것

- GitHub 토큰은 노트북의 `PUMASI_GITHUB_TOKEN` 에만 있다. 호스트로는 러너 하나짜리 JIT 설정만 간다.
- 에이전트는 호스트의 loopback 에서만 듣고, 노트북이 `ssh -L` 로 붙는다.
- 장부(SQLite)와 이벤트 로그는 `~/.local/state/pumasi/` 에 쌓인다. 잡마다 cpu 초·메모리 peak·기동 시간이 남는다.
- 지금은 Linux 호스트(WSL2 포함)와 Docker 러너만 된다. 사람 우선·Mac·VM 은 [설계](docs/design.md)의 다음 단계.

## 문서

[문제](docs/problem.md) · [설계](docs/design.md) · [M1 계획](docs/m1-plan.md) · [M1 실측](docs/m1-results.md) · [조사](docs/research/)
