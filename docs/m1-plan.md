# M1 계획 — 여러 호스트 · 용량 배치 · 장부 · 러너 메트릭

설계는 [design.md](design.md). M0 실측 표의 결론을 전제로 한다.

## 범위

- 호스트 2대: Linux 서버(48스레드) + Windows 데스크톱의 WSL2(20스레드, 상한 28G). 둘 다 cgroup v2·PSI·Docker.
- 클래스 2개: `linux-s`(2코어/4G), `linux-l`(8코어/16G). 클래스 = scale set 하나.
- 드라이버: `docker` 만. 러너마다 컨테이너 하나, `--cpus`·`--memory` 를 클래스 크기로.
- **호스트 상한(budget)**: 호스트마다 pumasi 가 쓸 수 있는 CPU·메모리 상한을 설정으로 둔다. 남의 일(다른 CI·사람)이 이미 도는 호스트에서 빌리는 몫을 정하는 손잡이다. 여유 = min(상한 − 러너 몫, 실측 여유).
- 시험 중엔 두 호스트 모두 다른 일이 돌고 있으므로 작게 잡는다: 클래스 `linux-s` 1코어/1G, `linux-l` 2코어/3G, 상한 서버 4코어/6G · 데스크톱 6코어/9G. 수치는 설정값이라 실제 운용에선 바꾼다.
- 사람 우선·VM 드라이버·캐시는 M2 이후.

## 구성

| 바이너리 | 어디서 | 하는 일 |
|---|---|---|
| `pumasi-controller` | 노트북 (GitHub 자격은 여기만) | 클래스마다 listener, SQLite 장부, `Decide`, 주기적 재평가 |
| `pumasi-agent` | 각 Linux 호스트 (WSL 포함) | 127.0.0.1 HTTP API: 상태 보고, 러너 시작·정지, `/metrics` |

전송: M1 은 controller 가 호스트마다 `ssh -L` 로 에이전트 포트에 붙는다(사내망·ProxyCommand 그대로 쓰고, 에이전트를 밖으로 열지 않는다). 에이전트가 먼저 연결하는 방식은 뒤로 미룬다. 전송은 인터페이스 뒤에 둔다.

## 에이전트 API (JSON)

- `GET /v1/snapshot` → `{host, cpus, memBytes, psi:{cpu,memory,io: some.avg10}, nonRunnerCpu, nonRunnerMem, runners:[{name, class, state, cpuSec, memPeak, ioBytes, startedAt, exitedAt, exitCode}]}`
  - 용량은 cgroup 루트·`/proc/meminfo` 기준. 러너 몫은 컨테이너 cgroup 합, 그 나머지가 "사람·기타 몫".
- `POST /v1/runners {name, class, cpus, memBytes, image, jitConfig}` → 컨테이너 시작. JIT 설정은 env-file 로 넘기고 디스크에 남기지 않는다.
- `DELETE /v1/runners/{name}` → 정지·삭제, 마지막 cgroup 수치를 반환.
- `GET /metrics` → OpenMetrics. 러너별 `pumasi_runner_cpu_seconds_total`, `pumasi_runner_memory_peak_bytes`, `pumasi_runner_io_bytes_total`, `pumasi_runner_pressure_seconds_total{resource}`, 호스트 `pumasi_host_pressure_avg10{resource}`.
- 러너 cgroup: Docker 컨테이너의 cgroup 경로(`/sys/fs/cgroup/system.slice/docker-<id>.scope` 또는 `--cgroup-parent` 로 지정한 `pumasi.slice`)에서 `cpu.stat`·`memory.peak`·`io.stat`·`*.pressure` 를 읽는다. 컨테이너가 끝나기 직전 값을 잃지 않도록 주기적으로(5초) 수집해 둔다.

## 장부 (SQLite)

- `runners(name PK, class, host, state, created_at, assigned_job_id, repo, workflow, job, run_id, started_at, finished_at, result, cpu_sec, mem_peak, io_bytes, provision_ms)`
- `demand(job_id PK, class, repo, workflow, job, run_id, labels, first_seen, last_seen, served_by)` — `JobAssigned` 로 들어오고 `JobStarted` 로 닫힌다. **재배정 3번 뒤 메시지가 끊겨도 여기 남아 있으므로, 주기 재평가가 자리가 나면 러너를 띄운다**(M0 실측).
- `decisions(at, class, host, action, reason)` — `Decide` 결과 그대로.

## Decide

`Decide(snapshot{hosts, classes, demand, runners}) -> Plan{start:[{class, host}], stop:[name], defer:[{class, reason}]}` 순수 함수. 규칙:

1. 클래스별 필요 = 열린 demand 수 − (starting + idle) 러너 수.
2. 후보 호스트 = 클래스를 허용하고, `min(상한 − Σ러너 cpus, cpus − Σ러너 cpus − 사람 몫) ≥ 클래스 cpus` 이고 메모리도 같으며, 호스트 PSI cpu avg10 < 임계(기본 40).
3. 순위 = 서버 먼저 → 여유 큰 순. 같은 판단 안에서 배치한 몫은 즉시 여유에서 뺀다.
4. 자리 없으면 `defer` 와 사유("linux-l: no host with 8 cpus free; server free=3.5 desktop free=6").
5. 시작 후 `provisionTimeout`(기본 3분) 안에 Listening/JobStarted 가 없거나 컨테이너가 종료되면 그 러너를 지우고 다시 결정한다.

## 수용 기준

1. `linux-l` 잡 4개 + `linux-s` 잡 6개를 동시에 넣어도, 어느 호스트에서든 클래스 크기 합이 용량(사람 몫 제외)을 넘지 않는다 — `decisions` 와 에이전트 스냅샷으로 확인.
2. 자리가 없어 미룬 잡이 자리가 나면(앞 잡 종료) 새 메시지 없이도 시작된다.
3. 잡마다 장부에 `cpu_sec`·`mem_peak`·`provision_ms`·`result` 가 남는다. `/metrics` 를 긁어 같은 값이 보인다.
4. 러너 컨테이너를 강제 종료하면 `provisionTimeout` 이나 종료 감지로 장부가 정리되고, 등록이 남으면 `RemoveRunner` 를 시도한다.
5. 단위 테스트: `Decide` 표 기반 테스트(호스트·수요 조합 → 기대 Plan), cgroup 파서 테스트(고정 파일).
