# Mac 과 Windows 호스트

Linux 호스트는 러너를 docker 컨테이너로 띄운다. Mac 과 Windows 에는 그 잡이 꼭 그 OS 여야 해서
오는 것이므로(서명, 네이티브 빌드, 그 OS 의 브라우저) 러너를 **그 OS 의 프로세스로** 띄운다.
`design.md` 의 `native` 드라이버다. VM(tart, Hyper-V)은 그 뒤다.

## 쓰는 모습

```yaml
classes:
  - { name: linux-s, cpus: 1, memory: 1G }
  - { name: macos,   cpus: 4, memory: 8G, os: macos }
  - { name: windows, cpus: 4, memory: 8G, os: windows }
```

워크플로우는 `runs-on: [self-hosted, pumasi-macos]`. 호스트는 자기 OS 를 스스로 말하고,
controller 는 클래스의 `os` 와 같은 호스트에만 그 클래스를 놓는다. `os` 를 안 적으면 linux 다.
그 밖의 배치 규칙(예산, 압박, 격리, 서버 먼저)은 같다.

## native 드라이버가 하는 일

러너 하나 =

1. `~/.cache/pumasi/runners/<name>/` 를 만든다. 러너 배포판은 호스트에 한 번 받아 둔 것을
   복제한다(Mac: APFS clone, Windows: 하드 링크 — 수백 MB 를 매번 풀지 않는다).
2. JIT 설정으로 러너를 그 디렉터리에서 실행한다. 작업 디렉터리·`RUNNER_TEMP`·`RUNNER_TOOL_CACHE`
   는 그 안이고, `HOME` 은 건드리지 않는다(서명 키체인·자격 증명은 호스트 사용자의 것을 쓴다).
3. 끝나면 프로세스 트리를 끝까지 정리하고 디렉터리를 지운다.

| | Mac | Windows |
|---|---|---|
| 프로세스 묶음 | 프로세스 그룹 + 자손 추적 | Job Object (`KILL_ON_JOB_CLOSE`) |
| 남은 자식 정리 | 그룹에 SIGTERM → SIGKILL | Job 종료 |
| CPU 초·메모리 peak | `proc_pid_rusage` 를 트리에 합산 | Job Object 회계 |
| 메모리 상한 | **없다**(강제 수단이 없음) — 넘으면 사건만 | Job Object 한도로 강제 |
| CPU 상한 | 없다. 사람 있을 때 `taskpolicy -b` | Job Object CPU rate |
| 호스트 압박 | PSI 가 없다 → 최근 CPU 사용률·memory pressure | 최근 CPU 사용률·commit |
| 사람이 쓰는 중 | HID idle 시간 | `GetLastInputInfo` |

docker 드라이버와 같은 `Driver` 인터페이스 뒤에 있고, 에이전트의 HTTP 계약·장부·사건은 같다.
`Runner` 의 수치 중 그 OS 에서 못 재는 것은 비워 둔다(0 으로 채우지 않는다).

## 만든 모양 (2026-11-25)

```
~/.cache/pumasi/dist/actions-runner-<os>-<arch>-<판>/   러너 배포판, 처음 한 번 내려받는다
~/.cache/pumasi/runners/<이름>/runner/                  그 복제 (러너마다)
~/.cache/pumasi/runners/<이름>/{pumasi.json,log,exit}   pumasi 가 그 러너에 대해 적어 두는 것
```

- **드라이버는 호스트 OS 가 고른다.** 플래그도 설정도 없다. Linux 는 docker, Mac·Windows 는 native.
- **러너 판은 `image` 의 태그에서 온다**(`ghcr.io/actions/actions-runner:2.338.2` → 2.338.2). 컨테이너와 프로세스 러너가 설정 하나를 따른다.
- **JIT 설정은 환경 변수로 넘긴다**(`ACTIONS_RUNNER_INPUT_JITCONFIG`). `--jitconfig` 인자는 `ps` 에 보이므로 docker 드라이버와 같은 길을 쓴다.
- **에이전트의 환경에서 자격처럼 보이는 것은 러너에 넘기지 않는다**(`*TOKEN*`, `*SECRET*`, `PUMASI_*` …). 토큰을 쥔 셸에서 띄운 에이전트가 그것을 잡에 건네지 않게.
- **Mac 의 러너는 에이전트가 죽어도 산다.** 자기 세션으로 띄우고, 출력은 파일로, exit code 는 러너를 감싼 셸이 파일에 남긴다.
  새 에이전트는 디렉터리와 프로세스 표로 러너를 다시 찾는다. 장부의 수치(cpu 초는 이어서 센다)와 "Listening"·"Running job" 시각도 그대로다.
- **Mac 의 프로세스 묶음** = 러너의 프로세스 그룹 + 아는 구성원의 자손 + 이미 아는 구성원. `setsid`·`nohup` 으로 그룹을 벗어난 자식도
  부모가 살아 있을 때 한 번 보였으면 끝까지 따라간다(벗어난 것은 `pids` 파일에 적어 에이전트 재시작을 넘긴다).
- **Mac 의 수치**: `proc_pid_rusage` 를 cgo 없이 부른다(libSystem 을 x/sys 와 같은 trampoline 으로; `CGO_ENABLED=0` 그대로).
  cpu 초 = 살아 있는 구성원의 자기 시간 + 이미 거둔 자식의 시간(샘플 사이에 왔다 간 컴파일러도 센다). 메모리 = `phys_footprint` 합, peak 는 샘플(5초)의 최댓값.
  디스크 I/O 는 살아 있는 프로세스 것만 나와서 비워 둔다. 러너가 뜨자마자 죽으면 한 번도 못 잰다.
- **Mac 호스트의 여유**: 남이 쓰는 CPU 는 Mach `host_statistics`, 메모리는 `kern.memorystatus_level`(`memory_pressure` 가 보이는 값).
  커널의 memory pressure 가 warning 이상이면 호스트가 스스로 닫는다(status 의 STATE 에 그 말이 나온다).
- **Windows 의 러너도 에이전트가 죽어도 산다.** Job 에 이름을 붙이고 `KILL_ON_JOB_CLOSE` 를 쓰지 않는다. Job 은 안에 프로세스가
  있는 동안 살아 있고, 새 에이전트가 이름으로 다시 연다. 끝낼 때만 `TerminateJobObject` 로 전부 정리한다.
  (표의 `KILL_ON_JOB_CLOSE` 는 처음 설계였다 — "도는 잡은 죽이지 않는다"와 어긋나서 버렸다.)
  이름은 로그온 세션 안의 것이라, 다른 세션에서 뜬 에이전트는 그 러너를 못 찾고 exited 로 본다.
  Job 의 이름은 그 Job 을 가리키는 마지막 핸들이 닫히면 (안에 프로세스가 남아 있어도) 사라진다. 그래서 러너의 셸이
  자기 Job 의 핸들을 물려받는다 — 러너의 프로세스가 사는 동안 이름도 산다. 에이전트가 죽어 있는 사이에 러너가 끝나고
  핸들을 안 물려받은 자식만 남았다면 새 에이전트는 그 자식을 못 찾는다(남는다).
- **러너 배포판은 sha256 을 확인하고서만 푼다.** GitHub release API 가 주는 asset digest 와 대조하고, 못 받거나 다르면
  이유를 말하고 러너를 띄우지 않는다(호스트 사용자 권한으로 도는 바이너리다).
- **Windows 의 메모리 한도**: 넘는 할당은 실패한다(죽이지 않는다). Job 이 completion port 로 알려 오는 횟수를 세어 Linux 의 OOM 과 같은 사건으로 낸다.
  이어받은 에이전트는 앞 에이전트의 port 를 떼고 자기 것을 붙인다. Windows 는 거절한 요청까지 peak 에 더하므로 peak 는 한도에서 자른다.

### 실측 (Mac, Apple silicon 노트북, ssh 로 붙임)

| | 결과 |
|---|---|
| 잡 5개 동시 / 5개 순서대로 | 모두 성공. 끝난 뒤 러너 디렉터리 0, 남은 프로세스 0 |
| 러너 기동(요청 → Listening) | 3.4 ~ 5.4초. 배포판(440M)을 처음 받는 러너만 9 ~ 13초, 복제는 APFS clone |
| 잡이 남긴 자식 | `sleep 600 &` 와, 러너의 정리를 피하고 그룹도 벗어난 `setsid` 자식 둘 다 잡이 끝나면 죽는다 |
| cpu 초 | 1코어를 40초 태운 잡 = 42.3 ~ 43.4초(러너 자신 약 3초 포함). `ps` 의 합과 일치 |
| 메모리 peak | 1000M 을 쥔 잡 = 1186M(러너 약 190M 포함). `top` 의 MEM 과 일치 |
| 클래스(1G)를 넘은 잡 | `ran over its class: macos is 1.0G, it used 1.2G` 사건 한 번, 잡은 성공 |
| 에이전트를 죽임 | controller 가 다시 올리고, 러너 3개는 그대로 돌며 시각·cpu 초가 이어진다 |
| controller 를 내렸다 올림 | `adopted 3 runners (3 running jobs)` |
| 도는 러너를 죽임 | `runner … died while running … (exit code -1)` — Linux 와 같은 사건 |

### 실측 (Windows 11, x64 데스크톱, 드라이버와 에이전트 수준 — GitHub 잡은 아직)

러너 자리에 스크립트를 넣은 가짜 배포판과 진짜 배포판(등록 없이)을 에이전트 API 로 띄웠다.

| | 결과 |
|---|---|
| Job 안 실행·exit code | 셸이 남긴 `exit` 파일에서 5 를 읽는다. 러너가 찍은 "Listening" 시각도 읽는다 |
| 회계 | 1코어를 4초 태운 스크립트 = cpu 4.3초. 메모리·I/O 가 찬다 |
| 메모리 한도(300M)에서 600M 요청 | 할당이 거절되고(`OutOfMemoryException`) 거절 1회가 센다. peak 는 300M |
| 에이전트를 죽임 | 러너와 자식은 그대로 돈다. 새 에이전트가 Job 을 이름으로 다시 열고 cpu 초가 이어진다. 그 뒤의 거절도 센다 |
| 남긴 자식(`start /b ping`) | 러너를 지우면 죽는다. 러너 디렉터리 0 |
| 러너 배포판 | sha256 확인 뒤 zip 을 푸는 데 5.0초, 하드 링크 복제 0.1초(`bin` 은 링크, 맨 위 파일은 복사) |
| 진짜 러너 | 요청 → 실행 4.2초(첫 내려받기 포함). 틀린 JIT 설정을 거절하고 끝난 것까지 본다 |

실물에서 나온 결함: Job 을 이름으로 다시 여는 것이 에이전트가 정말 죽은 뒤에는 안 됐다(위의 핸들 이야기). 고쳤다.
몇 초 만에 끝난 러너는 한 번도 못 잰 채 끝날 수 있다(Mac 과 같다).

## 컨테이너와 다른 점 (감추지 않는다)

- **격리가 약하다.** 러너는 호스트 사용자로 돈다. 잡이 `~` 에 쓰거나 전역 도구를 바꾸면 남는다.
  믿는 레포의 잡에만 쓴다. README 에 적는다.
- **Mac 은 메모리를 강제로 막지 못한다.** 클래스 크기는 배치할 때의 약속일 뿐이고, 넘은 러너는
  "ran over its class" 사건으로 드러난다. 그래서 Mac 호스트의 예산은 넉넉히 남겨 잡는다.
- **GUI 세션.** 브라우저·서명이 필요한 잡은 에이전트가 로그인한 사용자 세션에서 돌아야 한다.
  Mac 은 LaunchAgent, Windows 는 로그온 시 시작(서비스가 아니다). `pumasi doctor` 가 확인한다.

## 호스트를 붙이는 방법

Mac·Windows 는 `join` 으로 붙는 것을 기본으로 한다(`docs/join.md`). Windows 에 ssh 로 들어가
PowerShell 따옴표와 싸우지 않는다. 초대 줄은 OS 별로 하나씩 나온다(sh, PowerShell).
Mac 은 ssh `hosts:` 로도 붙는다(ssh 세션에서 뜬 에이전트는 로그인 세션의 키체인을 못 쓸 수 있다).
join 으로 붙은 Mac 의 에이전트는 LaunchAgent, Windows 는 로그온 때 시작하는 프로세스다 — `docs/join.md` 의 "OS 마다 다른 것".

## 순서

1. ~~`Driver` 뒤에 native 드라이버(Mac) — 이 노트북을 호스트로 붙여 잡을 끝까지. 프로세스 정리, 수치, 사건.~~ 됐다.
2. Windows — Job Object 로 같은 것. 한도 강제 확인. **드라이버·에이전트 수준 실물까지. GitHub 잡은 join 뒤.**
3. ~~클래스 `os` 와 배치, status·대시보드의 OS 열.~~ 됐다.
4. join 초대 줄(sh·PowerShell), LaunchAgent / 로그온 시작. **Mac 은 됐다(실물). Windows 는 코드까지.**
5. ~~사람이 쓰는 중이면 새 러너를 받지 않는다.~~ 됐다 — `people-first.md`.

## 수용

- Mac 호스트에서 잡 5개가 순서대로·동시에 성공, 끝난 뒤 러너 디렉터리와 남은 프로세스 0.
- 잡이 띄워 둔 자식 프로세스(`sleep 600 &`)가 잡이 끝나면 죽는다.
- 도는 러너를 죽이면 Linux 와 같은 사건이 난다. controller 를 내렸다 올리면 이어받는다.
- Windows 에서 같은 것, 그리고 메모리 한도를 넘는 잡이 한도에서 막히고 사건이 난다.
- linux 클래스 잡은 Mac·Windows 호스트에 가지 않고, 그 반대도 같다.
