# Mac 과 Windows 호스트

Linux 호스트는 러너를 docker 컨테이너로 띄운다. Mac 과 Windows 에는 그 잡이 꼭 그 OS 여야 해서
오는 것이므로(서명, 네이티브 빌드, 그 OS 의 브라우저) 러너를 **그 OS 의 프로세스로** 띄운다.
`design.md` 의 `native` 드라이버다. VM(tart, Hyper-V)은 그 뒤다.

## 쓰는 모습

```yaml
classes:
  - { name: linux-s, cpus: 1, mem: 1G }
  - { name: macos,   cpus: 4, mem: 8G, os: macos }
  - { name: windows, cpus: 4, mem: 8G, os: windows }
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
Mac 은 ssh `hosts:` 로도 붙는다.

## 순서

1. `Driver` 뒤에 native 드라이버(Mac) — 이 노트북을 호스트로 붙여 잡을 끝까지. 프로세스 정리, 수치, 사건.
2. Windows — Job Object 로 같은 것. 한도 강제 확인.
3. 클래스 `os` 와 배치, status·대시보드의 OS 열.
4. join 초대 줄(sh·PowerShell), LaunchAgent / 로그온 시작.
5. 사람이 쓰는 중이면 새 러너를 받지 않는다(M2 의 사람 우선과 합친다).

## 수용

- Mac 호스트에서 잡 5개가 순서대로·동시에 성공, 끝난 뒤 러너 디렉터리와 남은 프로세스 0.
- 잡이 띄워 둔 자식 프로세스(`sleep 600 &`)가 잡이 끝나면 죽는다.
- 도는 러너를 죽이면 Linux 와 같은 사건이 난다. controller 를 내렸다 올리면 이어받는다.
- Windows 에서 같은 것, 그리고 메모리 한도를 넘는 잡이 한도에서 막히고 사건이 난다.
- linux 클래스 잡은 Mac·Windows 호스트에 가지 않고, 그 반대도 같다.
