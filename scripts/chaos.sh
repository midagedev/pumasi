#!/usr/bin/env bash
# chaos: provoke the failures of docs/design.md on real hosts and check
# what pumasi does about each: the job's result, the incident it opens, the
# notification it sends and the incident's end.
#
#   PUMASI_GITHUB_TOKEN=$(gh auth token) \
#   CHAOS_CONFIG=pumasi.yaml CHAOS_REPO=owner/repo \
#   CHAOS_HOST=server1 CHAOS_SSH='ssh -o BatchMode=yes server1 sh -s' \
#   make chaos                      # or: scripts/chaos.sh oom runner-kill
#
# CHAOS_CONFIG   a working pumasi.yaml without notify:, rerun:, hostLostAfter:,
#                diskMinFree:, evaluateEvery: or provisionTimeout: (the script
#                adds them), with classes written as {name: linux-s, ...}.
# CHAOS_REPO     the repository of CHAOS_CONFIG's url; it must have
#                .github/workflows/chaos.yml on its default branch.
# CHAOS_HOST     the host (name in the config) that gets the linux-s jobs first:
#                the first host that is not a desktop.
# CHAOS_SSH      a command that runs a script from stdin on that host.
# CHAOS_JOIN     set to 1 when CHAOS_HOST is not in CHAOS_CONFIG's hosts: the
#                script then runs the invite line on the host itself (with
#                CHAOS_JOIN_FLAGS, default "--server --cpus 4 --mem 6G").
#                Otherwise `pumasi up` makes the host join over ssh.
#
# On the host it touches only containers labelled pumasi, pumasi's agent and
# its service, ~/.cache/pumasi and ~/.local/bin/pumasi.
# Rows that wait for GitHub to give up on a vanished runner take 10-15 minutes each. Needs gh, jq and sqlite3.
set -u

: "${PUMASI_GITHUB_TOKEN:?}" "${CHAOS_CONFIG:?}" "${CHAOS_REPO:?}" "${CHAOS_HOST:?}" "${CHAOS_SSH:?}"
cd "$(dirname "$0")/.."
BIN=$PWD/dist/pumasi
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pumasi-chaos.XXXXXX")
NOTES=$WORK/notices.jsonl
LOG=$WORK/up.log
CFG=$WORK/pumasi.yaml
RESULTS=${CHAOS_RESULTS:-$WORK/results.tsv}
LEDGER=${CHAOS_LEDGER:-$HOME/.local/state/pumasi/pumasi.db}
PID=""
: > "$NOTES"; : > "$LOG"
export GH_TOKEN=$PUMASI_GITHUB_TOKEN

say() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

# config [extra yaml lines…]: the base config plus what every row needs.
config() {
  { cat "$CHAOS_CONFIG"
    printf '\nhostLostAfter: 20s\nnotify:\n  command: "cat >> %s"\n  level: all\n' "$NOTES"
    for line in "$@"; do printf '%s\n' "$line"; done
  } > "$CFG"
}

up() {
  : > "$LOG"
  "$BIN" up -config "$CFG" >> "$LOG" 2>&1 &
  PID=$!
  wait_for 180 "controller listening" grep -q "listening for jobs" "$LOG" || return 1
  [ -z "${CHAOS_JOIN:-}" ] || join_host
}

# join_host: the host joins once (a `down` makes the controller forget it) and
# is waited for after every later start.
join_host() {
  if ! grep -q "\"name\": \"$CHAOS_HOST\"" "$(dirname "$LEDGER")/hosts.json" 2>/dev/null; then
    local line
    line=$("$BIN" invite -config "$CFG" | grep 'join.sh') || return 1
    echo "$line --name $CHAOS_HOST ${CHAOS_JOIN_FLAGS:---server --cpus 4 --mem 6G}" | host >> "$LOG" 2>&1
  fi
  wait_for 90 "$CHAOS_HOST connected" host_reachable
}
host_reachable() { [ "$(status | jq -r --arg h "$CHAOS_HOST" '.hosts[] | select(.name == $h) | .reachable')" = true ]; }

agent_pid_differs() { local now; now=$(echo "echo $AGENT_PID" | host); [ -n "$now" ] && [ "$now" != "$1" ]; }

# The agent's pid, as a shell expression for the host.
AGENT_PID='$(pgrep -fx ".*/pumasi agent -join")'

# stop: Ctrl-C. Agents and running jobs stay.
stop() {
  [ -n "$PID" ] || return 0
  kill -INT "$PID" 2>/dev/null
  wait "$PID" 2>/dev/null
  PID=""
}

down() {
  stop
  "$BIN" down -force -config "$CFG" >> "$LOG" 2>&1
}

# wait_for <seconds> <what> <command…>: poll until the command succeeds.
wait_for() {
  local limit=$1 what=$2 t=0
  shift 2
  until "$@" 2>/dev/null; do
    sleep 3; t=$((t + 3))
    if [ "$t" -ge "$limit" ]; then say "  timeout after ${limit}s: $what"; return 1; fi
  done
}

host() { $CHAOS_SSH 2>&1 | tr -d '\r\000'; }

status() { "$BIN" status -config "$CFG" --json; }

busy_on_host() { status | jq -r --arg h "$CHAOS_HOST" '[.runners[] | select(.host == $h and .state == "busy")] | .[].name'; }

two_busy() { [ "$(busy_on_host | wc -l | tr -d ' ')" -ge 2 ]; }

# dispatch <seconds> <mb>: start the chaos workflow, print its run id.
dispatch() {
  local before
  before=$(gh run list -R "$CHAOS_REPO" --workflow chaos.yml --limit 1 --json databaseId --jq '.[0].databaseId // 0')
  gh workflow run chaos.yml -R "$CHAOS_REPO" -f seconds="$1" -f mb="$2" > /dev/null || return 1
  local id=$before t=0
  while [ "$id" = "$before" ] && [ $t -lt 60 ]; do
    sleep 3; t=$((t + 3))
    id=$(gh run list -R "$CHAOS_REPO" --workflow chaos.yml --limit 1 --json databaseId --jq '.[0].databaseId // 0')
  done
  [ "$id" != "$before" ] && echo "$id"
}

run_field() { gh run view "$1" -R "$CHAOS_REPO" --json "$2" --jq ".$2"; }

# conclusion <run> <seconds>: wait for the run to complete, print its conclusion.
conclusion() {
  wait_for "$2" "run $1 completed" test_completed "$1" || { echo "still-running"; return 1; }
  run_field "$1" conclusion
}
attempt_is() { [ "$(run_field "$1" attempt)" = "$2" ]; }
test_completed() { [ "$(run_field "$1" status)" = completed ]; }

# noticed <seconds> <text>: a notification containing the text arrived in this row.
noticed() {
  wait_for "$1" "notification: $2" notice_has "$2"
}
notice_has() { tail -n +"$((ROW_FROM + 1))" "$NOTES" | grep -q -- "$1"; }
notice_line() { tail -n +"$((ROW_FROM + 1))" "$NOTES" | grep -- "$1" | head -1 | jq -r .text; }

sql() { sqlite3 "$LEDGER" "$1" 2>/dev/null; }

FAILED=""
check() { # check <what> <command…>
  local what=$1
  shift
  if "$@"; then say "  ok    $what"; else say "  FAIL  $what"; FAILED="$FAILED$what; "; fi
}

# ---- rows -------------------------------------------------------------------

row_two_controllers() {
  config; up || return 1
  local out
  out=$("$BIN" up -config "$CFG" 2>&1)
  check "second controller exits naming the first" grep -q "already running (pid $PID)" <<< "$out"
  NOTE="$(tail -1 <<< "$out")"
}

row_bad_token() {
  config
  local out
  out=$(PUMASI_GITHUB_TOKEN=ghp_notavalidtoken000000000000000000000 "$BIN" up -config "$CFG" 2>&1)
  check "up exits with an error" test $? -ne 0
  check "the error names the rejection" grep -q -i -E "401|403|credential|unauthor" <<< "$out"
  NOTE="start-up only: $(tail -1 <<< "$out" | cut -c1-120)"
}

row_oom() {
  config "rerun: 1"; up || return 1
  local run; run=$(dispatch 30 1500) || return 1
  check "incident names the limit" noticed 240 "hit the linux-s 1.0G memory limit"
  check "job failed" test "$(conclusion "$run" 600)" = failure
  sleep 15
  check "not queued for rerun" test "$(sql "SELECT count(*) FROM reruns WHERE run_id=$run")" = 0
  check "recorded on the runner row" test "$(sql "SELECT count(*) FROM runners WHERE run_id=$run AND oom_kills > 0")" -ge 1
  NOTE="$(notice_line "memory limit")"
}

row_agent_kill() {
  config; up || return 1
  local run; run=$(dispatch 120 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  local before; before=$(echo "echo $AGENT_PID" | host)
  echo "kill -9 $AGENT_PID" | host
  # The host's own service starts the agent again within seconds, usually
  # before the controller's next snapshot: the controller sees one session
  # end and another begin, not an unreachable host.
  check "the host's service started another agent" wait_for 60 "a new agent pid" agent_pid_differs "$before"
  check "host connected again" wait_for 60 "$CHAOS_HOST connected" host_reachable
  check "the drop is named" test "$(status | jq -r --arg h "$CHAOS_HOST" '.hosts[] | select(.name == $h) | .lastDrop')" = "agent exited"
  NOTE="restarted by the host's service: agent pid $before -> $(echo "echo $AGENT_PID" | host), last drop 'agent exited'"
  check "jobs succeeded" test "$(conclusion "$run" 600)" = success
}

# The jobs end while nobody collects them, then the agent dies: the numbers of
# the finished containers exist only in the agent's state file.
row_agent_restart() {
  config; up || return 1
  local run; run=$(dispatch 40 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  stop
  check "jobs succeeded without a controller" test "$(conclusion "$run" 600)" = success
  sleep 10
  echo "kill -9 $AGENT_PID" | host
  up || return 1
  sleep 30
  check "final cgroup numbers kept" test "$(sql "SELECT count(*) FROM runners WHERE run_id=$run AND cpu_sec > 0 AND mem_peak > 0 AND removed_at IS NOT NULL")" = 2
  check "results recorded" test "$(sql "SELECT count(*) FROM runners WHERE run_id=$run AND result='succeeded'")" = 2
  NOTE="cpu seconds of the two runners: $(sql "SELECT group_concat(printf('%.1f', cpu_sec), ', ') FROM runners WHERE run_id=$run")"
}

row_controller_kill() {
  config; up || return 1
  local run; run=$(dispatch 120 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  kill -9 "$PID"; wait "$PID" 2>/dev/null; PID=""
  sleep 20
  up || return 1
  check "restart told with what it adopted" noticed 120 "controller is back"
  check "jobs succeeded" test "$(conclusion "$run" 600)" = success
  sleep 15
  check "both results in the ledger" test "$(sql "SELECT count(*) FROM runners WHERE run_id=$run AND result='succeeded'")" = 2
  NOTE="$(notice_line "controller is back")"
}

# The host's connection goes dead without closing: its agent is frozen for 60s.
row_tunnel_cut() {
  config; up || return 1
  local run; run=$(dispatch 150 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  echo "p=$AGENT_PID; kill -STOP \$p; sleep 60; kill -CONT \$p" | host
  check "incident names why" noticed 30 "unreachable for .* (keepalive timeout)"
  check "incident while unreachable" noticed 30 "unreachable for"
  check "incident ends" noticed 180 "host unreachable resolved"
  check "jobs succeeded" test "$(conclusion "$run" 600)" = success
  check "no running job was stopped" test "$(sql "SELECT count(*) FROM runners WHERE run_id=$run AND state IN ('lost','failed')")" = 0
  NOTE="$(notice_line "unreachable for")"
}

row_runner_kill() {
  config "rerun: 1"; up || return 1
  local run; run=$(dispatch 150 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  local victim; victim=$(busy_on_host | head -1)
  echo "docker kill $victim" | host > /dev/null
  check "incident names the runner and job" noticed 120 "runner $victim died while running"
  check "rerun asked once GitHub closed the run" noticed 1500 "reran the failed jobs of run $run"
  wait_for 120 "second attempt started" attempt_is "$run" 2
  check "second attempt succeeded" test "$(conclusion "$run" 900)" = success
  NOTE="$(notice_line "reran the failed jobs")"
}

row_reboot() { # simulated: nobody reboots a shared host for a test
  config "rerun: 1"; up || return 1
  local run; run=$(dispatch 150 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  stop
  # The agent is frozen while its world changes and killed last: the host's
  # service starts it again and it finds the state file of another boot.
  host <<EOF
d=~/.cache/pumasi
p=$AGENT_PID
kill -STOP \$p
ids=\$(docker ps -q --filter label=pumasi)
[ -n "\$ids" ] && docker kill \$ids > /dev/null
sed -i 's/"bootId":"[^"]*"/"bootId":"00000000-0000-0000-0000-000000000000"/' \$d/runners.json
kill -9 \$p
EOF
  up || return 1
  check "one incident for the host, with the jobs" noticed 180 "rebooted: 2 runners lost with 2 running jobs"
  check "rerun asked once GitHub closed the run" noticed 1500 "reran the failed jobs of run $run"
  wait_for 120 "second attempt started" attempt_is "$run" 2
  check "second attempt succeeded" test "$(conclusion "$run" 900)" = success
  NOTE="simulated (agent and its containers killed, boot id in the state file changed): $(notice_line "rebooted:")"
}

row_ledger() {
  config; up || return 1
  local run; run=$(dispatch 150 100) || return 1
  wait_for 240 "two jobs running" two_busy || return 1
  stop
  head -c 8192 /dev/urandom > "$LEDGER"
  rm -f "$LEDGER-wal" "$LEDGER-shm"
  up || return 1
  check "incident says the ledger was set aside" noticed 120 "ledger was damaged and set aside"
  check "running runners adopted from the host" test "$(grep -c "not in the ledger" "$LOG")" -ge 2
  check "jobs succeeded" test "$(conclusion "$run" 600)" = success
  sleep 15
  check "results recorded on the adopted rows" test "$(sql "SELECT count(*) FROM runners WHERE result='succeeded' AND removed_at IS NOT NULL")" = 2
  NOTE="$(notice_line "ledger was damaged")"
}

row_max_duration() {
  sed 's/name: linux-s,/name: linux-s, maxDuration: 40s,/' "$CHAOS_CONFIG" > "$WORK/base.yaml"
  CHAOS_CONFIG=$WORK/base.yaml config "rerun: 1"; up || return 1
  local run; run=$(dispatch 600 100) || return 1
  check "incident when the limit passes" noticed 300 "ran longer than the linux-s maxDuration"
  check "job failed on GitHub" test "$(conclusion "$run" 1500)" = failure
  check "not queued for rerun" test "$(sql "SELECT count(*) FROM reruns WHERE run_id=$run")" = 0
  NOTE="$(notice_line "maxDuration")"
}

row_disk() {
  config "diskMinFree: 99.9"; up || return 1
  check "incident with the free space" noticed 60 "free on the disk that holds its runners"
  local run; run=$(dispatch 20 100) || return 1
  wait_for 120 "job waits because of the disk" eval 'status | jq -e ".waiting[] | select(.reason | test(\"disk\"))" > /dev/null'
  check "no runner placed while below the threshold" test "$(status | jq '.runners | length')" = 0
  stop
  config; up || return 1
  check "incident ends when there is room" noticed 60 "disk low resolved"
  check "job then ran" test "$(conclusion "$run" 600)" = success
  NOTE="threshold raised to 99.9%: $(notice_line "free on the disk")"
}

row_quarantine() {
  config "evaluateEvery: 2s" "provisionTimeout: 2s"; up || return 1
  local run; run=$(dispatch 20 100) || return 1
  check "host closed after 3 failed starts" noticed 180 "$CHAOS_HOST quarantined: 3 runners in a row"
  check "host returns after the check passes" noticed 420 "$CHAOS_HOST: host quarantined resolved"
  gh run cancel "$run" -R "$CHAOS_REPO" > /dev/null 2>&1
  NOTE="provisionTimeout 2s makes every start fail: $(notice_line "quarantined: 3")"
}

# ---- driver -----------------------------------------------------------------

ROWS="${*:-two_controllers bad_token oom agent_kill agent_restart controller_kill tunnel_cut ledger disk quarantine runner_kill reboot max_duration}"
[ -x "$BIN" ] || { echo "run make first"; exit 1; }
say "work dir $WORK"
for row in $ROWS; do
  row=${row//-/_}
  ROW_FROM=$(wc -l < "$NOTES" | tr -d ' ')
  FAILED=""; NOTE=""
  say "row $row"
  if ! "row_$row"; then FAILED="${FAILED}did not get as far as the checks; "; fi
  down
  cp "$LOG" "$WORK/$row.log"
  if [ -z "$FAILED" ]; then verdict=pass; else verdict=FAIL; fi
  say "$verdict $row  ${FAILED:-$NOTE}"
  printf '%s\t%s\t%s\n' "$row" "$verdict" "${FAILED:-$NOTE}" >> "$RESULTS"
done
say "results in $RESULTS"
! grep -q "	FAIL	" "$RESULTS"
