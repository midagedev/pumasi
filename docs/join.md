# 호스트가 스스로 붙는다 (join)

지금은 controller 가 ssh 로 호스트에 들어간다. 그러면 controller 가 모든 호스트에 닿아야 하고,
호스트마다 ssh 설정·셸 차이·터널 상태가 문제의 원인이 된다. 방향을 뒤집는다:
**호스트가 controller 로 연결을 연다.** controller 는 받는 포트 하나만 연다.

## 쓰는 모습

controller 쪽:

```yaml
# pumasi.yaml
repo: github.com/me/project
listen: 0.0.0.0:7411        # 이 줄이 있으면 호스트의 가입을 받는다
classes:
  - { name: linux-s, cpus: 1, mem: 1G }
  - { name: linux-l, cpus: 2, mem: 3G }
```

```
$ pumasi up
$ pumasi invite
호스트에서 이 한 줄을 실행하세요:

  curl -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.sh | sh -s -- <token>
```

호스트 쪽은 그 한 줄이 전부다. 스크립트는 controller 에서 자기 아키텍처의 바이너리를 받아
`~/.local/bin/pumasi` 에 두고 `pumasi join` 을 서비스로 올린다. 예산과 종류는 호스트가 말한다:

```
… | sh -s -- <token> --name desk1 --cpus 6 --mem 9G --desktop
```

값을 안 주면 이름은 hostname, 예산은 그 머신의 절반이다. 떼려면 호스트에서 `pumasi leave`.

`hosts:` 에 ssh 로 적는 지금 방식은 그대로 남는다. 한 controller 에 둘이 섞여도 된다.

## 어떻게 붙는가

- **연결 하나.** 호스트가 TLS 로 controller 에 붙고, 그 연결 위에 스트림을 여러 개 얹는다(yamux).
  controller 는 그 스트림으로 지금과 **같은 HTTP 계약**(`/v1/snapshot`, `/v1/runners`)을 호출한다.
  바뀌는 것은 누가 전화를 거는가뿐이다 — `api.Client` 의 transport 가 ssh 터널 대신 이 세션이다.
  `Decide`, 장부, 사건, 복구 코드는 호스트가 어떻게 붙었는지 모른다.
- **끊기면 호스트가 다시 건다.** 1초부터 30초까지 늘려 가며 계속. controller 가 내려가 있어도
  호스트의 러너와 잡은 그대로 돈다(지금과 같다). 다시 붙으면 controller 가 이어받는다.
- **하트비트.** 세션 keepalive 10초. 20초 응답이 없으면 양쪽이 연결을 버리고 호스트가 다시 건다.

## 서로를 믿는 방법

- controller 는 처음 `up` 때 TLS 키와 인증서, 가입 토큰을 만들어 상태 디렉터리에 둔다.
- **호스트가 controller 를 확인**: 초대 줄에 인증서 공개키 지문이 들어 있다(`--pinnedpubkey`).
  `pumasi join` 도 같은 지문으로만 붙는다. 호스트는 시키는 이미지를 실행하므로 이 확인이 필요하다.
- **controller 가 호스트를 확인**: 가입 토큰. `pumasi invite --rotate` 로 바꾸면 이미 붙은 호스트는
  유지되고(호스트별 자격을 가입 때 발급) 새 가입만 새 토큰을 쓴다.
- JIT 설정은 TLS 안으로만 간다. GitHub 토큰은 지금처럼 controller 밖으로 나가지 않는다.
- 대시보드와 status 는 계속 loopback 에만 열린다. `listen` 포트는 가입·바이너리 내려받기·세션만 받는다.

## ssh 가 조용히 해 주던 일의 새 주인

| 일 | ssh 방식 | join 방식 |
|---|---|---|
| 바이너리 전달 | up 이 올림 | 호스트가 controller 에서 받음 (`/dl/pumasi-linux-<arch>`, sha 확인) |
| 판 맞추기 | up 이 sha 로 경로를 나눔 | 붙을 때 판을 말하고, 다르면 호스트가 새 판을 받아 자기 자신을 바꿔 실행(러너는 건드리지 않음) |
| 죽은 에이전트 다시 올리기 | controller 가 ssh 로 | 호스트의 서비스(systemd user unit, 없으면 재시작 루프) |
| `down --force` 의 호스트 정리 | ssh 로 지움 | 세션으로 `leave` 를 시킴. 안 붙어 있는 호스트는 이름을 나열하고 "거기서 `pumasi leave`" 라고 말함 |
| 호스트 이름 충돌 | 설정에서 드러남 | 호스트 id(가입 때 만든 난수)가 다르면 거부하고 이유를 말함 |

## 연결 문제를 말로 한다

연결·네트워크가 늘 말썽이라는 것이 이 작업의 이유다. 그래서:

- **`pumasi join` 은 붙기 전에 하나씩 확인하고 실패한 것의 이름을 말한다.**
  ```
  ok    resolve 10.0.0.5
  ok    tcp 10.0.0.5:7411 (3ms)
  ok    controller certificate matches the invite
  FAIL  token: the controller refused it (rotated on 11-25 15:02) — run `pumasi invite` again
  ok    docker 27.3 reachable as this user
  ok    cgroup v2 with memory and cpu controllers
  ```
  호스트에서 `pumasi doctor` 로 언제든 다시 볼 수 있다.
- **status 와 대시보드의 Hosts 표**에 호스트별로: 붙은 방식(join/ssh), 마지막으로 본 시각,
  왕복 시간, 에이전트 판, 마지막으로 끊긴 이유(`keepalive timeout`, `connection reset`, `agent exited`,
  `token refused`).
- **사건**: 지금의 "host unreachable" 이 그대로 쓰이고 문구에 끊긴 이유가 붙는다.
  하루에 여러 번 끊겼다 붙는 호스트는 flapping 으로 한 건에 묶인다(이미 있음).
  판이 달라 스스로 바꾸지 못하면 "agent version" 사건.

## 하지 않는 것

- NAT 를 뚫는 중계. 호스트가 controller 의 `listen` 주소에 닿아야 한다(같은 망, VPN, tailnet).
- 호스트가 클래스나 이미지를 정하는 것. 호스트가 말하는 것은 이름·예산·종류뿐이다.
- 웹에서 호스트를 붙이고 떼는 버튼. 대시보드는 읽기 전용이다.

## 수용

1. 빈 `hosts:` 로 `up` → 두 호스트에서 초대 줄 → 잡 10개가 두 호스트에 나뉘어 성공.
2. controller 를 내렸다 올림 → 호스트가 다시 붙고 도는 잡을 이어받음.
3. 호스트 쪽 네트워크를 60초 막음 → 사건이 끊긴 이유와 함께 열리고 닫힘, 도는 잡은 성공.
4. 틀린 토큰·틀린 지문·docker 없음 → `join` 이 각각 그 이름으로 실패.
5. 판이 다른 호스트 → 스스로 바꾸고 러너는 그대로.
6. `pumasi leave`, `down --force` → 호스트에 남는 것 없음.
7. `make chaos` 의 행들이 join 으로 붙은 호스트에서도 통과.
