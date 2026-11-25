# 호스트의 디렉터리를 쓰는 잡

기존 셀프호스트 워크플로우는 호스트의 경로를 많이 쓴다: 공용 캐시(pnpm store, gradle home,
브라우저), 잡 사이에 넘기는 산출물, 호스트 사용자의 자격(키체인, `~/.npmrc`), 호스트의 docker.
이런 워크플로우를 **고치지 않고** 옮길 수 있어야 한다.

Mac·Windows 는 러너가 호스트 사용자의 프로세스라 이미 된다(`platforms.md`). Linux 에 세 가지를 더한다.

## 1. Linux 에서도 프로세스 러너 — `driver: native`

```yaml
classes:
  - { name: linux-s, cpus: 1, memory: 1G }                    # 컨테이너 (기본)
  - { name: build,   cpus: 4, memory: 8G, driver: native }    # 호스트 프로세스
```

러너를 호스트 사용자의 프로세스로 띄우되 **cgroup 에 넣는다**(systemd user scope:
`systemd-run --user --scope -p CPUQuota= -p MemoryMax=`). 그래서

- 호스트 경로·자격·docker 가 지금의 고정 러너와 똑같이 보인다.
- CPU·메모리 한도, 수치(cpu 초, peak, 압박, OOM), 남은 자식 정리(`cgroup.kill`)는 컨테이너 러너와 같다 —
  같은 `internal/cgroup` 을 읽는다.
- 사용자 cgroup 위임이 없는 호스트(systemd user 세션 없음, cpu·memory 컨트롤러 미위임)에서는
  한도 없이 띄우지 않는다. `doctor`/프리플라이트가 그 이름으로 실패하고 그 호스트는 이 클래스를 받지 않는다.

감추지 않는 것: 잡이 호스트 docker 로 띄운 컨테이너는 이 cgroup 밖이다. 한도에도 수치에도 안 잡힌다.
격리는 Mac·Windows 의 native 와 같이 약하다.

`driver` 를 안 적으면 linux 는 docker, macos·windows 는 native 다. macos·windows 에 docker 는 없다.

## 2. 컨테이너 러너의 마운트 — `mounts`

```yaml
  - { name: linux-l, cpus: 2, memory: 3G, mounts: [/srv/ci-cache] }
```

같은 경로로 컨테이너에 붙인다(`:ro` 를 붙이면 읽기 전용). 호스트는 그 경로가 있는지 스스로 말하고,
controller 는 **모든 경로가 있는 호스트에만** 그 클래스를 놓는다. 없어서 못 놓으면 대기 사유에 경로가 나온다:
`no host has /srv/ci-cache`. native 클래스에 적으면 마운트는 하지 않고 배치 조건으로만 쓴다.

## 3. 클래스를 호스트에 묶기 — `hosts`

```yaml
  - { name: release, cpus: 4, memory: 8G, driver: native, hosts: [server1] }
```

한 잡이 호스트 디렉터리에 쓴 것을 다음 잡이 읽는 워크플로우는 두 잡이 같은 머신에 떨어져야 한다.
GitHub 은 "앞 잡과 같은 머신" 을 지정하게 해 주지 않으므로, 그런 잡의 클래스를 호스트 하나에 묶는다.
묶인 클래스는 분산되지 않는다 — 분산하려면 그 상태를 공유 저장소나 artifact 로 옮겨야 하고,
그것은 워크플로우의 일이다. 묶인 호스트가 없거나 끊기면 대기 사유에 나온다.

## 옮기는 순서 (README 에 한 절)

1. 고정 러너의 라벨 하나 = 클래스 하나. 우선 `driver: native`, 그 라벨이 머신 한 대였으면 `hosts:` 로 묶는다.
   워크플로우는 `runs-on` 만 바뀐다.
2. 잡 사이 상태가 없는 클래스부터 `hosts:` 를 풀어 여러 머신으로 넓힌다. 캐시는 머신마다 생긴다.
3. 호스트를 건드릴 이유가 없는 잡은 컨테이너 클래스로 옮긴다.

## 수용

- native linux 러너: 잡이 호스트의 디렉터리에 쓰고 다음 잡이 읽는다. 호스트 docker 를 쓴다.
  메모리 한도를 넘으면 OOM 사건, 잡이 남긴 자식은 끝나면 죽는다. 수치가 장부에 찬다.
- cgroup 위임이 없는 조건에서는 한도 없이 뜨지 않고 이유를 말한다.
- `mounts`: 경로가 있는 호스트에만 놓이고 컨테이너 안에서 같은 경로로 읽고 쓴다. 없으면 대기 사유.
- `hosts`: 묶인 호스트에만 놓인다. 그 호스트가 차면 다른 호스트가 비어 있어도 대기한다.
- 기존 클래스(설정 변경 없음)의 동작은 그대로다.

## 만든 모양 (2026-11-25, 1~3)

- **Linux 의 native 러너** = `systemd-run --user --scope --unit=pumasi-<러너> -p CPUQuota= -p MemoryMax= -p MemorySwapMax=0`
  안의 `run.sh`. systemd-run 이 자신을 scope 에 넣고 나서 러너의 셸이 되므로, 러너는 무엇을 띄우기 전부터 한도 아래다.
  scope 는 systemd 의 것이고 에이전트의 것이 아니다 — 에이전트가 죽어도 러너는 돌고, 새 에이전트는 systemd 에게
  scope 의 cgroup 을 물어 이어받는다. 러너 디렉터리·배포판·exit code 파일은 Mac·Windows 의 native 와 같다(`platforms.md`).
- **한도를 넘은 프로세스만 죽는다**(`OOMPolicy=continue`, systemd 253 이상). 러너는 살아서 실패한 step 을 보고한다. 컨테이너 러너와 같다.
- **정리**는 `cgroup.kill` 한 번(없는 커널은 `systemctl kill`). `down` 은 에이전트가 죽어 있어도 `pumasi-*.scope` 를 찾아 끝낸다.
- **띄울 수 없는 호스트는 스스로 말한다**: systemd 가 없다 / 그 사용자의 systemd user 세션이 없다 / cpu·memory 가 위임되지 않았다 /
  linger 가 꺼져 있다(로그아웃하면 러너가 죽는다). 에이전트가 Snapshot 의 `noNative` 로 알리고, controller 는 그 호스트에 native 클래스를
  놓지 않으며 대기 사유에 그 말이 나온다. 컨테이너 클래스는 그 호스트에 그대로 간다.
- **io 는 위임되지 않은 호스트가 많다**(기본 위임은 cpu·memory·pids). 그때 native 러너의 I/O 바이트는 비워 둔다.
- **`mounts`** 는 `docker run --mount type=bind`(`-v` 가 아니다 — 없는 경로를 docker 가 root 로 만들지 않게). controller 가 Snapshot 을
  물을 때 클래스들의 경로를 같이 묻고, 호스트는 그중 있는 것을 답한다.
- **`hosts`** 에 적힌 호스트가 아직 붙지 않았으면 대기 사유에 `<이름> has not joined`.
- 설정 키는 `driver`·`mounts`·`hosts` 셋뿐이다. 플래그는 없다.

### 실측 (WSL2 Ubuntu, systemd 255, 20코어 데스크톱 — ssh `hosts:` 로 붙임)

| | 결과 |
|---|---|
| native 러너 잡 둘(쓰기 → 읽기) | 앞 잡이 `~/.cache/…` 에 쓴 것을 다른 러너의 다음 잡이 읽었다. 호스트 docker 로 컨테이너도 띄웠다 |
| 러너 기동(요청 → Listening) | 2.9 ~ 3.2초. 배포판(683M)을 처음 받는 러너만 9.9초 |
| 수치 | 1코어 20초 + 200M 을 쥔 잡 = cpu 21.9초, peak 514M(러너·docker 클라이언트 포함), 압박 시간도 찬다. I/O 는 비었다(io 미위임) |
| 클래스(1G)에서 1500M 요청 | 그 프로세스만 `Killed`(exit 137), 잡은 실패로 보고, `hit the build 1.0G memory limit` 사건, oom 1, peak 1024M |
| 잡이 남긴 자식 | `sleep 600 &` 와 `setsid` 로 빠져나간 것 둘 다 잡이 끝나면 없다. scope 0, 러너 디렉터리 0 |
| 잡 도중 에이전트를 죽임 | scope 와 잡은 그대로. controller 가 에이전트를 다시 띄워 이어받고 잡은 성공, cpu 초도 이어진다(23.9초) |
| 잡 도중 controller 를 내리고 에이전트도 죽인 뒤 `down --force` | scope·프로세스·`~/.cache/pumasi` 0 |
| `mounts: [<dir>/rw, <dir>/ro:ro]` | 컨테이너가 호스트의 seed 를 읽고, rw 에 쓴 파일이 호스트에 보이며(uid 1001), ro 에는 못 쓴다 |
| 어디에도 없는 경로의 클래스 | `no host has /tmp/pumasi-test/absent` 로 대기 |
| `hosts: [desk1]` (3cpu, 예산 6cpu) 잡 셋 | 둘이 뜨고 하나는 `no host with 3.0cpu/2.0G free (desk1 0.0cpu/5.0G)` 로 대기 뒤 실행 |
| 붙지 않은 호스트에 묶인 클래스 | `(nohost has not joined)` 로 대기 — 다른 호스트가 비어 있어도 |
| 설정을 안 바꾼 컨테이너 클래스 | 그대로 성공. cpu·peak·I/O·압박이 장부에 찬다(Linux 경로의 Meter 정리도 이것으로 확인) |
| systemd user 세션이 없는 조건 | 러너를 만들지 않고 `no systemd user session for …` 를 말한다(드라이버 테스트, 세션 주소를 지워서) |

호스트가 둘 이상일 때 "묶인 호스트가 차면 빈 다른 호스트로 가지 않는다"는 단위 테스트(`TestClassBoundToHosts`)로만 확인했다 — 실물 호스트는 한 대였다.
native 잡의 로그에는 러너 디렉터리 경로(호스트 사용자의 홈)가 나온다.

## 고정 러너에 기대던 것들 (실제 파이프라인을 훑어 나온 목록)

호스트 경로 말고도, 오래 떠 있는 고정 러너라서 저절로 되던 것들이 있다. 일회용 러너로 바꾸면 조용히 느려지거나 깨진다.

### 4. 작업 디렉터리를 남긴다 — `workdir: keep`

`actions/checkout` 의 `clean: false`, 워크스페이스 안의 `node_modules`·빌드 산출물, 얕은 `.git` 재사용은
모두 "지난 잡의 작업 디렉터리가 그대로 있다" 에 기댄다. 러너마다 새 디렉터리를 주면 큰 모노레포는
잡마다 전체 체크아웃과 설치를 다시 한다.

```yaml
  - { name: build, cpus: 4, memory: 8G, driver: native, workdir: keep }
```

- 호스트·클래스마다 **슬롯**(`~/.cache/pumasi/slots/<class>/<n>/`)을 두고, 러너는 일회용이되 `_work` 는 슬롯의 것을 쓴다.
  동시에 도는 러너 수만큼 슬롯이 생기고, 빈 슬롯 중 가장 최근에 쓴 것을 먼저 준다.
- 러너에 `PUMASI_SLOT=<n>` 을 준다. 러너 이름으로 캐시 디렉터리를 나누던 워크플로우는 이것으로 나눈다
  (일회용 러너는 이름이 매번 달라 디렉터리가 끝없이 늘어난다).
- 러너 이름도 가능하면 `<class>-<host>-<n>` 으로 고정한다(앞 등록이 GitHub 에 남아 있으면 접미사를 붙인다).
- `down` 은 슬롯을 지우지 않는다. `down --purge` 가 지운다. 오래 안 쓴 슬롯은 디스크 임계 때 먼저 지운다.
- 기본은 지금처럼 새 디렉터리(`workdir` 없음).

### 5. 도구 캐시는 호스트에 하나

`RUNNER_TOOL_CACHE` 를 러너 디렉터리 밖(`~/.cache/pumasi/tools`)으로 둔다. 안 그러면 `setup-node`·`setup-java` 가 잡마다 다시 받는다.

### 6. 지울 수 없는 잔여물

잡이 컨테이너로 root 소유 파일을 작업 디렉터리에 남기면 러너 사용자는 못 지운다. 정리가 실패하면
그 디렉터리를 `trash/` 로 옮기고 사건을 낸다(조용히 쌓이지 않게). docker 가 있으면 컨테이너로 지운다.

### 7. Windows 의 짧은 경로

러너 디렉터리 깊이가 MAX_PATH(260)를 먹는다. Windows 의 러너 루트는 짧게(`C:\pm\<n>`) 잡고 설정으로 바꿀 수 있게 한다.

### 8. 러너 그룹과 조직 단위 등록

`runs-on: { group: X, labels: [...] }` 로 그룹을 지정하는 워크플로우가 있다. scale set 을 만들 때 러너 그룹을 정할 수 있어야 한다:

```yaml
repo: github.com/org            # 조직 단위
group: builders                 # 없으면 Default
```

### 9. "빈 러너 수" 를 묻는 워크플로우

GitHub API 로 idle 러너를 세어 shard 수를 정하는 워크플로우는 일회용 러너에서 늘 0 을 본다.
대신 물을 곳을 준다: controller 의 `/api/capacity?class=X` → 지금 바로 띄울 수 있는 수.
(GitHub 호스트 러너에서 도는 준비 잡은 controller 에 닿지 못하므로, 그런 워크플로우는 shard 수를 고정하는 편이 낫다.)

### pumasi 가 풀지 못하는 것 (README 에 적는다)

- **한 run 의 잡들을 같은 머신에.** scale set 안에서는 어느 러너가 어느 잡을 받을지 정할 수 없다.
  호스트에 띄운 스택을 뒤 잡이 이어 쓰는 워크플로우는 호스트마다 클래스를 하나씩 두고(`hosts:` 묶기)
  준비 잡이 run 전체의 클래스를 고르게 한다.
- **잡이 호스트 docker 로 띄운 컨테이너의 사용량.** 러너의 수치에 안 잡힌다. 호스트 전체 압박으로만 보인다.
  그런 잡의 클래스 크기는 스택까지 넣어 잡는다.
- **native 러너와 에이전트는 같은 사용자다.** 잡이 에이전트의 가입 자격을 읽을 수 있다.
  배포 자격을 쥔 호스트는 믿는 워크플로우의 클래스만 받게 `hosts:` 로 가른다.
- **한 머신의 여러 OS 사용자.** 에이전트는 사용자마다 하나고 서로의 사용량을 호스트 압박으로만 안다.
