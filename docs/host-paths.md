# 호스트의 디렉터리를 쓰는 잡

기존 셀프호스트 워크플로우는 호스트의 경로를 많이 쓴다: 공용 캐시(pnpm store, gradle home,
브라우저), 잡 사이에 넘기는 산출물, 호스트 사용자의 자격(키체인, `~/.npmrc`), 호스트의 docker.
이런 워크플로우를 **고치지 않고** 옮길 수 있어야 한다.

Mac·Windows 는 러너가 호스트 사용자의 프로세스라 이미 된다(`platforms.md`). Linux 에 세 가지를 더한다.

## 1. Linux 에서도 프로세스 러너 — `driver: native`

```yaml
classes:
  - { name: linux-s, cpus: 1, mem: 1G }                    # 컨테이너 (기본)
  - { name: build,   cpus: 4, mem: 8G, driver: native }    # 호스트 프로세스
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
  - { name: linux-l, cpus: 2, mem: 3G, mounts: [/srv/ci-cache] }
```

같은 경로로 컨테이너에 붙인다(`:ro` 를 붙이면 읽기 전용). 호스트는 그 경로가 있는지 스스로 말하고,
controller 는 **모든 경로가 있는 호스트에만** 그 클래스를 놓는다. 없어서 못 놓으면 대기 사유에 경로가 나온다:
`no host has /srv/ci-cache`. native 클래스에 적으면 마운트는 하지 않고 배치 조건으로만 쓴다.

## 3. 클래스를 호스트에 묶기 — `hosts`

```yaml
  - { name: release, cpus: 4, mem: 8G, driver: native, hosts: [server1] }
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
