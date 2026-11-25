# 사람 우선

데스크톱은 누군가의 책상이다. 그 사람이 앉아 있는 동안에는 새 러너를 받지 않는다.
돌고 있는 잡은 건드리지 않는다 — GitHub 에는 잡을 옮기는 길이 없어서, 죽이면 실패한 잡만 남는다.

설정은 없다. `desktop: true`(join 이면 `--desktop`)인 호스트에 저절로 적용된다.

## 신호

에이전트는 "마지막 키보드·마우스 입력이 몇 초 전인가"만 말한다(Snapshot 의 `inputIdleSec`). 알 수 없으면 비운다.

| OS | 읽는 곳 | 알 수 없는 때 |
|---|---|---|
| macOS | HID 시스템의 `HIDIdleTime`(`ioreg -c IOHIDSystem`) | — |
| Windows | `GetLastInputInfo` | 에이전트가 사용자 세션 밖(ssh·서비스, 세션 0)에서 뜬 때 |
| Linux | logind 의 화면 세션(x11·wayland, 로컬, active)의 `IdleHint`·`IdleSinceHint` | 화면 세션이 없는 호스트(서버, WSL) |

Linux 데스크톱 환경은 몇 분 입력이 없어야 `IdleHint` 를 켠다. 그 전까지는 "지금 있다"로 읽는다.

## 규칙

- controller: `desktop` 호스트의 마지막 입력이 **15분** 안이면 그 호스트는 닫힌다(`pumasi status` 의 STATE 와 대기 사유에 `in use`).
  15분이 지나면 다시 열린다. 서버는 콘솔에 누가 있어도 닫지 않는다. 신호를 모르는 호스트는 열려 있다.
- 닫힌 호스트의 러너는 그대로 돈다. 러너를 지우는 규칙은 하나도 바뀌지 않는다.
- macOS 에이전트: 사람이 있는 동안 러너의 프로세스를 background 대역으로 내린다(`taskpolicy -b` 와 같은 `PRIO_DARWIN_BG`:
  낮은 CPU 우선순위, 디스크 throttle, 효율 코어 먼저). 15분 자리를 비우면 되돌린다. Mac 은 CPU 상한이 없어서
  이것이 도는 잡이 사람을 덜 방해하게 하는 유일한 수단이다. 이것은 `desktop` 여부와 무관하다(입력이 없는 Mac 서버에서는 일어나지 않는다).

## 확인한 것 (2026-11-25)

- Mac: `HIDIdleTime` 읽기, 프로세스를 background 대역으로 넣고 빼기(`ps` 의 PRI 31 ↔ 4, 그 뒤에 띄운 자식도 4) — 이 노트북에서 테스트로.
- 15분 규칙과 Linux 세션 읽기는 단위 테스트. Linux 데스크톱 실물은 없었다.
- Windows `GetLastInputInfo` 는 로그온 세션에서 뜬 에이전트가 있어야 해서 join 뒤에 확인한다.

## 하지 않은 것

- 사람 몫 CPU 가 높으면 닫기: 남이 쓰는 CPU·메모리는 이미 빈 자리 계산에서 빠진다.
- 화면 잠금 읽기: 잠근 뒤 15분이면 같은 결과다.
- Linux·Windows 러너의 우선순위 낮추기: 둘은 CPU 상한이 강제된다.
