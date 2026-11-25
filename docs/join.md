# 호스트가 스스로 붙는다 (join)

처음에는 controller 가 ssh 로 호스트에 들어가 터널로 에이전트를 불렀다. 그러면 controller 가 모든 호스트에 닿아야 하고,
호스트마다 ssh 설정·셸 차이·터널 상태가 문제의 원인이 된다. 그래서 방향을 뒤집었고, 지금은 이 길 하나뿐이다:
**호스트가 controller 로 연결을 연다.** controller 는 받는 포트 하나만 연다.

## 쓰는 모습

controller 쪽:

```yaml
# pumasi.yaml
url: https://github.com/me/project
listen: 0.0.0.0:7411        # 기본값이라 안 적어도 된다 (hosts: 는 없어도 된다)
classes:
  - {name: linux-s, cpus: 1, memory: 1G}
  - {name: linux-l, cpus: 2, memory: 3G}
```

```
$ pumasi up
$ pumasi invite
Run this on the host that should join. It works for one host, until 12:57.

Linux, macOS:

  curl -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.sh | PUMASI_TOKEN=<token> sh -s --

Windows (PowerShell, in a window on that machine's desktop):

  $env:PUMASI_TOKEN='<token>'; & ([scriptblock]::Create((curl.exe -fsSk --pinnedpubkey 'sha256//…' https://10.0.0.5:7411/join.ps1 | Out-String)))
```

`listen` 이 `0.0.0.0` 이면 초대 줄의 주소는 기본 경로의 주소다. 머신에 주소가 여럿이면 나머지도 알려 준다.
호스트는 스크립트를 받은 그 주소로 계속 붙는다.

호스트 쪽은 그 한 줄이 전부다. 스크립트는 controller 에서 자기 OS·아키텍처의 바이너리를 받아
`~/.local/bin/pumasi`(Windows: `pumasi.exe`)에 두고 `pumasi join` 을 서비스로 올린다. 예산과 종류는 호스트가 말한다:

```
… | PUMASI_TOKEN=<token> sh -s -- --name desk1 --cpus 6 --mem 9G --desktop
```

값을 안 주면 이름은 hostname, 예산은 그 머신의 절반, 종류는 Mac·Windows 와 화면 세션이 있는 Linux 가 desktop 이고 나머지가 server 다.
토큰은 인자가 아니라 환경으로 간다(프로세스 목록에 남지 않게). 떼려면 호스트에서 `pumasi leave`.

`hosts:` 에 `ssh: X` 로 적은 호스트는 controller 가 ssh 로 들어가 이 초대 줄을 그 호스트의 이름·예산·종류로 대신 실행한다
(그 호스트 이름에 묶인 1회용 토큰, 스크립트는 stdin 으로). 붙은 뒤는 같다. 호스트가 `listen` 주소에 닿지 못하면 `up` 이
"X cannot reach this controller at …" 로 말한다. 안 붙어 있는 그런 호스트를 `down` 이 치울 때도 ssh 로 `pumasi leave` 를 실행한다.

## 어떻게 붙는가

- **연결 하나.** 호스트가 TLS 로 controller 에 붙고, 그 연결 위에 스트림을 여러 개 얹는다(yamux).
  controller 는 그 스트림으로 지금과 **같은 HTTP 계약**(`/v1/snapshot`, `/v1/runners`)을 호출한다.
  `api.Client` 의 transport 가 이 세션이다.
  `Decide`, 장부, 사건, 복구 코드는 호스트가 어떻게 붙었는지 모른다.
- **끊기면 호스트가 다시 건다.** 1초부터 30초까지 늘려 가며 계속. controller 가 내려가 있어도
  호스트의 러너와 잡은 그대로 돈다(지금과 같다). 다시 붙으면 controller 가 이어받는다.
- **하트비트.** 세션 keepalive 10초. 20초 응답이 없으면 양쪽이 연결을 버리고 호스트가 다시 건다.

## 서로를 믿는 방법

- controller 는 처음 `up`(또는 `invite`) 때 TLS 키와 인증서를 만들어 상태 디렉터리에 둔다
  (`join.key`·`join.crt`, 가입한 호스트는 `hosts.json`, 초대 토큰은 해시 이름의 파일로 `invites/`).
- **호스트가 controller 를 확인**: 초대 줄에 인증서 공개키 지문이 들어 있다(`--pinnedpubkey`).
  `pumasi join` 도 같은 지문으로만 붙는다. 호스트는 시키는 이미지를 실행하므로 이 확인이 필요하다.
- **controller 가 호스트를 확인**: 가입 토큰. `pumasi invite` 때마다 새로 만들고 기본은 호스트 하나·1시간(`--uses`, `--ttl`). 호스트는 가입할 때 자기 id 와 비밀값을 만들어 토큰과 함께
  보내고, controller 는 비밀값의 해시만 적어 둔다. 그 뒤 연결은 id 와 비밀값으로 한다. 그래서
  토큰이 다 쓰였거나 만료돼도 이미 붙은 호스트는 유지된다.
  스크립트(`join.sh`·`join.ps1`)는 요청의 Host 헤더가 주소 모양일 때만 내준다(스크립트에 그 값이 들어가므로).
- JIT 설정은 TLS 안으로만 간다. GitHub 토큰은 지금처럼 controller 밖으로 나가지 않는다.
- 대시보드와 status 는 계속 loopback 에만 열린다. `listen` 포트는 가입·바이너리 내려받기·세션만 받는다.

## ssh 가 조용히 해 주던 일의 새 주인

| 일 | 옛 ssh 방식(없어짐) | 지금 |
|---|---|---|
| 바이너리 전달 | up 이 올림 | 호스트가 controller 에서 받음 (`/dl/pumasi-<os>-<arch>`, Windows 는 `.exe`, sha 확인) |
| 판 맞추기 | up 이 sha 로 경로를 나눔 | 붙을 때 판(바이너리 sha256)을 말하고, 다르면 controller 가 세션을 주지 않고 자기 판을 알려 준다. 호스트가 **자기 OS 의** 판을 받아 sha 를 확인하고 자기 자신을 바꿔 실행한다(러너는 건드리지 않음). 못 바꾸면 이유를 말하며 옛 판으로 붙는다 |
| 죽은 에이전트 다시 올리기 | controller 가 ssh 로 | 호스트의 서비스(아래 표). 서비스로 못 올리는 호스트에서는 스스로 다시 뜨는 백그라운드 프로세스(재부팅 뒤에는 안 올라온다 — 가입할 때 그렇게 말한다) |
| `down` 의 호스트 정리 | ssh 로 지움 | 세션으로 `leave` 를 시킴. 안 붙어 있는 호스트는 `hosts:` 의 것이면 ssh 로 `pumasi leave` 를 실행하고, 스스로 붙은 것이면 "거기서 `pumasi leave`" 라고 말함 |
| 호스트 이름 충돌 | 설정에서 드러남 | 호스트 id(가입 때 만든 난수)가 다르면 거부하고 이유를 말함 |

### OS 마다 다른 것

| | Linux | Mac | Windows |
|---|---|---|---|
| 초대 줄 | sh | sh (같은 줄) | PowerShell |
| 러너 | docker 컨테이너 | 그 사용자의 프로세스 | 그 사용자의 프로세스(Job Object) |
| 에이전트를 지키는 것 | systemd user unit (linger 일 때) | LaunchAgent `dev.pumasi.agent` — 화면에 로그인한 사용자 세션 | HKCU `Run` 의 `pumasi` 값 — 로그온 때 뜨는 감독 프로세스(`agent -join -supervise`) |
| 그게 안 되면 | 스스로 다시 뜨는 프로세스 | 같음(화면에 로그인하지 않은 Mac) | — (ssh·서비스 세션이면 그렇다고 말한다) |
| 붙기 전 확인 | docker, cgroup v2 | 러너 배포판 받기(github.com), 디스크, 로그인 세션 | 같음 |
| 판 교체 | 같은 pid 로 exec | 같은 pid 로 exec | 파일 이름을 바꿔 두고 끝남 → 감독 프로세스가 새 판을 띄움 |

Mac 과 Windows 의 에이전트가 **로그인한 사용자의 세션**에서 도는 것은 잡 때문이다: 서명은 그 사용자의 키체인을,
브라우저 시험은 화면을 쓴다. 그래서 Windows 는 서비스(세션 0)가 아니다. Mac 의 LaunchAgent 는 가입할 때의 `PATH` 를
물려받는다(launchd 의 기본 `PATH` 에는 그 사용자가 깐 도구가 없다).

## 연결 문제를 말로 한다

연결·네트워크가 늘 말썽이라는 것이 이 작업의 이유다. 그래서:

- **`pumasi join` 은 붙기 전에 하나씩 확인하고 실패한 것의 이름을 말한다.**
  ```
  ok    resolve 10.0.0.5
  ok    tcp 10.0.0.5:7411 (3ms)
  ok    controller certificate matches the invite
  FAIL  token: the controller refused it — run `pumasi invite` again
  ok    docker 27.3 reachable as this user
  ok    cgroup v2 with memory and cpu controllers
  ```
  하나라도 FAIL 이면 아무것도 등록하지 않고, 처음 가입이었으면 받은 바이너리도 지우고 끝난다. 호스트에서 `pumasi doctor` 로 언제든 다시 볼 수 있고,
  거기에는 에이전트가 도는지와 지금 붙어 있는지(아니면 언제부터 왜)가 더 나온다.
- **status 와 대시보드의 Hosts 표**에 호스트별로: 마지막으로 본 시각,
  왕복 시간(스냅샷 한 번의 왕복), 에이전트 판, 마지막으로 끊긴 이유(`keepalive timeout`,
  `connection reset`, `agent exited`). 호스트가 먼저 끊고 다시 건 경우
  (controller 가 멈춰 있었다)에는 호스트가 붙으면서 말한 이유가 남는다.
- **사건**: 지금의 "host unreachable" 이 그대로 쓰이고 문구에 끊긴 이유가 붙는다.
  하루에 여러 번 끊겼다 붙는 호스트는 flapping 으로 한 건에 묶인다(이미 있음).
  판이 달라 스스로 바꾸지 못하면 "agent version" 사건.

## 떼기

- 호스트에서 `pumasi leave`: 에이전트를 멈추고 pumasi 컨테이너·`~/.cache/pumasi`·바이너리·서비스를 지우고
  controller 에 알린다. 러너가 떠 있으면 목록을 보이고 거부한다(`--force` 로 강행, `down` 과 같다).
  controller 에 못 닿으면 호스트 쪽만 지우고 그렇게 말한다.
- controller 가 호스트를 잊으면(`leave`, `down`) 그 호스트의 러너는 장부에서 "host … is not configured" 로 닫힌다.

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
8. Mac 을 초대 줄로 붙여 `os: macos` 클래스의 잡이 끝까지, 에이전트를 죽이거나 판을 바꿔도 러너는 그대로, 떼면 남는 것 없음.
9. Windows 에서 같은 것(PowerShell 초대 줄).

## 실측 (2026-11-25)

| | 결과 |
|---|---|
| 4. 틀린 토큰 / 틀린 지문 / docker 없음 | 각각 `FAIL token`, `FAIL controller certificate`, `FAIL docker: not installed` 로 끝나고 등록되지 않음 |
| 5. controller 판을 바꿔 올림 | 호스트가 새 판을 받아 같은 pid 로 바뀜, 도는 러너 2개 그대로, 잡 성공 (Linux·Mac) |
| 6. `pumasi leave`, `down --force` | 러너가 떠 있으면 목록을 보이고 거부. 끝난 뒤에는 서비스·바이너리·`~/.cache/pumasi`·러너 0 (Linux·Mac) |
| 호스트 쪽 재연결 | controller 를 40초 멈춤 → 호스트가 13초 뒤 `keepalive timeout` 으로 끊고 2·4초 간격으로 다시 걸어, 풀린 지 4초 뒤 붙음. 도는 잡은 이어받음 |
| join 일원화 뒤 (같은 날 밤) | `hosts: - ssh:` 호스트 하나(Linux, controller 가 초대 줄을 대신 실행) + 초대 줄을 직접 실행한 Mac 하나로 수용 1~7 다시 통과. 닿지 못하는 `listen` 주소면 `up` 이 "cannot reach this controller at …" 로 끝남. 한 번 쓴 토큰·모르는 토큰은 `FAIL token`, 토큰은 호스트의 프로세스 인자·파일에 남지 않음. 안 붙어 있는 ssh 호스트는 `down` 이 ssh 로 치움 |
| 45분 잡 | Linux(컨테이너)·Mac(프로세스) 각각 하나: 도는 동안 controller kill -9(LaunchAgent 가 다시 띄움)·판 교체 재시작·에이전트 60초 멈춤(keepalive timeout)·controller 머신 load 120 을 넣었고 잡은 성공 |
| 에이전트를 죽임 | Linux: systemd 가 2초 뒤, Mac: launchd 가 1초 안에 다시 올리고 바로 붙음. 러너 그대로 |
| 8. Mac(Apple silicon 노트북) | 초대 줄 → LaunchAgent, `macos` 클래스 잡 3개 + 2개 성공, 끝난 뒤 러너 디렉터리·남은 프로세스 0 |
| 9. Windows | 코드와 크로스 빌드, 단위 테스트까지. 실물 미확인 |
| 남의 부하로 스래싱하는 호스트 | 에이전트가 10초 넘게 못 돌아 keepalive 가 끊긴 일이 있었다(load 200, swap 가득). 사건이 이유와 함께 열리고 닫혔고 잡은 성공 |
