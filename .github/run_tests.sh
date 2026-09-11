#!/bin/bash
# Runs the full test suite against the server package in the repo root:
#   1. go vet
#   2. go test -race (unit tests in main_test.go)
#   3. black-box scenarios (testing/harness) against a real running instance,
#      once with rate limiting untouched (disabled, the shipped default) and
#      once with it explicitly enabled, to check both modes actually work.
#
# Run from the repo root: ./run_tests.sh
set -uo pipefail
cd "$(dirname "$0")/.."   # repo root (contains go.mod, main.go)
ROOT="$(pwd)"
WORK="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$WORK"' EXIT

echo "== go vet =="
go vet ./... || exit 1

echo
echo "== go test -race (unit tests) =="
go test -race ./... || exit 1

echo
echo "== building server + test tools =="
go build -o "$WORK/wsserver" . || exit 1
go build -o "$WORK/fixture" ./testing/fixture || exit 1
go build -o "$WORK/harness" ./testing/harness || exit 1

wait_up() {
  for i in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:$1/metrics" && return 0
    sleep 0.1
  done
  echo "server on port $1 never came up" >&2
  return 1
}

run_scenario() {
  local label=$1 port=$2 db=$3 mode=$4; shift 4
  "$WORK/fixture" "$db" >/dev/null
  "$WORK/wsserver" -addr ":$port" -db sqlite -dsn "file:$db?cache=shared" \
    -log-file "$WORK/$label.log" -log-format json "$@" &
  local pid=$!
  wait_up "$port" || { kill $pid 2>/dev/null; return 1; }
  "$WORK/harness" -addr "127.0.0.1:$port" -mode "$mode"
  local status=$?
  kill $pid 2>/dev/null; wait $pid 2>/dev/null
  return $status
}

fail=0

echo
echo "== core scenarios, rate limiting left at shipped default (disabled) =="
run_scenario core 8081 "$WORK/core.db" core || fail=1

echo
echo "== rate limiting NOT set on the command line -> must never 429 under a burst =="
run_scenario rl-default 8082 "$WORK/rl-default.db" ratelimit-default || fail=1

echo
echo "== rate limiting explicitly set low -> must 429 once the burst is exceeded =="
run_scenario rl-explicit 8083 "$WORK/rl-explicit.db" ratelimit-explicit -rate-limit-ip 2 -rate-burst-ip 2 -rate-limit-player 2 -rate-burst-player 2 || fail=1

echo
if [ $fail -eq 0 ]; then
  echo "ALL GREEN"
else
  echo "SOME SCENARIOS FAILED, see output above"
fi
exit $fail
