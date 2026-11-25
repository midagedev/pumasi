# M1 실측 결과

계획은 [m1-plan.md](m1-plan.md). 호스트 2대(Linux 서버 상한 4코어/6G, WSL2 데스크톱 상한 6코어/9G), 클래스 `linux-s` 1코어/1G · `linux-l` 2코어/3G. 워크플로 `m1`(large 4 + small 6)을 4회 돌렸다. 러너 43개, 잡 39개 성공.

## 수용 기준

| # | 기준 | 결과 | 근거 |
|---|---|---|---|
| 1 | 몰아 넣어도 호스트 상한을 넘지 않는다 | 통과 | 에이전트 스냅샷 622개에서 러너 크기 합의 최대값: 서버 4코어/6G(상한 4/6), 데스크톱 6코어/7G(상한 6/9) |
| 2 | 미룬 잡이 자리가 나면 새 메시지 없이 시작된다 | 통과 | 상한을 1코어로 줄여 large 3개를 미뤘다. GitHub 재배정은 5분 간격 2번 뒤 끊겼고, 상한을 되돌리자 다음 재평가에서 배치됐다(처음 본 뒤 1024초, 배치 뒤 6~11초에 잡 시작). `defer` 결정 37건 모두 사유 문자열 포함 |
| 3 | 잡마다 cpu·메모리 peak·기동 시간·결과가 남고 `/metrics` 와 같다 | 통과(1회차 일부 누락) | 성공 39건 모두 cpu_sec·mem_peak·result 있음. 장부와 마지막 `/metrics` 긁은 값: mem peak 일치, cpu 는 0.6초 이내 차이. `provision_ms` 는 1회차 6건이 비었고(로그 읽기 버그) 고친 뒤 회차는 전부 기록 |
| 4 | 러너를 강제로 죽이면 장부가 정리된다 | 통과(결함 1건 발견·수정) | 아래 |
| 5 | 단위 테스트 | 통과 | `Decide` 표 기반, cgroup·PSI 파서(실제 호스트 고정 파일), 장부, 에이전트 |

## 숫자

| 항목 | 값 |
|---|---|
| 기동 시간(배치 → Listening) | 데스크톱 중앙값 4.9초(4.3~6.8, n=24) · 서버 중앙값 11.2초(5.4~23.6, n=11). 서버는 다른 CI 로 붐비는 중이었다 |
| `linux-s` 잡 | cpu 67.5~91.0초, mem peak 709~835MiB (상한 1G) |
| `linux-l` 잡 | cpu 165.5~179.9초, mem peak 2142~2330MiB (상한 3G) |
| 러너 cgroup cpu 압력 | 잡당 최대 2.3초 |
| 배치 | 서버 16 · 데스크톱 27. 서버 먼저 채우고 넘치면 데스크톱 |
| `JobStarted` 메시지 지연 | 러너가 잡을 잡은 뒤 30~50초 늦게 온다(long poll 이 이 메시지로 깨지 않음) |

## 강제 종료 시험 (기준 4)

- 기동 중인 러너를 죽임: 10초 안에 `exited before a job (code 137)` 로 정리, 등록 삭제, 다른 러너가 그 잡을 받아 성공.
- 기동이 멈춘 러너: 3분 뒤 `provision timeout` 으로 정리.
- **잡을 돌리던 러너를 죽임**: 잡은 실패(M0 와 같다, 재시도 없음). 등록은 GitHub 이 잡을 실패 처리할 때까지 "busy" 라 지울 수 없고, 9분 30초 뒤 삭제됐다.
- 결함: `JobStarted` 가 늦게 오는 사이에 죽으면 controller 는 "잡 받기 전에 죽었다"로 보고 대체 러너를 하나 더 띄웠다. 그 러너는 5분 빈 채로 있다가 idle timeout 으로 정리됐다. 고침: 에이전트가 러너 출력의 `Running job` 줄을 읽어 busy 를 먼저 알린다. 다시 쟀다(2026-11-25, `pumasi up` 으로 잡 10개): 10개 모두 controller 가 busy 를 GitHub 의 `JobStarted` 메시지보다 먼저 봤다(32.7~33.5초 앞). 같은 회차 기동 시간은 서버 중앙값 7.6초(7.3~9.2, n=6) · 데스크톱 4.7초(4.6~4.9, n=4).

## controller 를 껐다 켜도 잡이 산다 (2026-11-25)

잡 10개 중 7개가 도는 중에 `pumasi up` 을 Ctrl-C 로 끄고(`JobStarted` 메시지가 오기 전) 20초 뒤 다시 켰다. 러너 7개와 에이전트는 그대로 돌았고, 다시 켠 controller 가 이어받았다. 놓친 `JobStarted`·`JobCompleted` 는 새 메시지 세션에서 다시 왔다. 잡 10개 모두 성공, 장부 10행 모두 cpu_sec·mem_peak·provision_ms·result 있음. 그사이 `pumasi down` 은 러너 7개를 나열하고 거부했다.

## 재시작·터널 끊김·down 수용 시험 (2026-11-25)

controller 하나만 띄우고 다시 쟀다. 잡 10개(각 300초), `hostLostAfter: 20s`.

| 시험 | 한 일 | 결과 |
|---|---|---|
| A 재시작 | 7개가 도는 중에 `pumasi up` 을 Ctrl-C, 30초 뒤 다시 실행 | `stopped  7 jobs still running on the hosts` → `agent already running` → `adopted 7 runners left on the hosts (7 running jobs)`. 놓친 `JobStarted` 7건은 새 세션에서 다시 왔다 |
| B 터널 끊김 | 같은 런에서 데스크톱 터널을 2초마다 90초, 이어서 0.3초마다 70초 죽임 | 호스트가 30초 넘게 unreachable(`desk1 down`, 상한 20초 초과). busy 러너에 stop 0건, 그동안 그 호스트에 새 배치 없음, 돌아오자 스냅샷으로 맞춤 |
| A·B 잡 결과 | | 10개 모두 성공. stop 10건 모두 `job completed`. 장부 10행 모두 cpu_sec·mem_peak·provision_ms·result 있음(large cpu 576~602초·peak 2.2G, small 288~301초·0.7G) |
| C down | 잡 7개가 도는 중 `pumasi down` | controller 가 떠 있으면 `another pumasi up or down is already running (pid …)`. controller 를 내린 뒤엔 러너 7개를 나열하고 거부(exit 1). `--force` 는 컨테이너 7개·에이전트·scale set 을 지웠다 |

- 5분 넘게 기다린 잡은 GitHub 이 배정을 거두고 새 job id 로 다시 배정한다. 묵은 배정 id 의 취소 메시지(러너 없는 `JobCompleted canceled`)가 오고, 장부는 처음 본 시각을 잇는다.
- controller 둘이 같은 장부로 뜨면 메시지 세션·에이전트·status 주소를 서로 깬다(한 번 겪음). 상태 디렉터리 flock 으로 `up`·`down` 은 한 번에 하나만 돈다.

## M1 에서 배운 것

- ssh 연결 다중화(ControlMaster)는 터널이 같이 죽어서 끈다.
- Windows 호스트의 ssh 는 PowerShell 로 떨어진다. 스크립트를 stdin 으로 `wsl -e sh -s` 에 넘기면 인용 문제가 없다.
- 아무도 받지 않아 취소된 배정은 새 job id 로 다시 온다. 장부는 처음 본 시각을 잇는다.
- 붐비는 서버는 기동 시간이 2~5배 흔들린다. 서버 우선 순위는 PSI 를 같이 봐야 한다.
