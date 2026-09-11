#!/bin/bash
# Behavioral regression check: runs the SAME black-box scenario suite
# against two builds (this repo's current main.go, and another main.go you
# point it at, e.g. the pre-refactor version) and diffs the results plus
# the structured JSON logs field-for-field (timestamps/durations excluded).
#
# Usage, from repo root:
#   ./testing/diff_against.sh /path/to/other/main.go
#
# A clean run means: the two versions are behaviorally indistinguishable
# on every scenario this suite covers. It does NOT mean identical on
# everything, only on what's exercised, extend testing/harness for
# anything you specifically changed.
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

if [ $# -ne 1 ]; then
  echo "usage: $0 /path/to/other/main.go" >&2
  exit 2
fi
OTHER_MAIN="$1"
[ -f "$OTHER_MAIN" ] || { echo "not found: $OTHER_MAIN" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$WORK"' EXIT

echo "== building test tools =="
go build -o "$WORK/fixture" ./testing/fixture || exit 1
go build -o "$WORK/harness" ./testing/harness || exit 1

echo "== building THIS repo's server (new) =="
go build -o "$WORK/wsserver-new" . || exit 1

echo "== building the OTHER server (old), in its own throwaway module =="
mkdir -p "$WORK/othermod"
cp "$OTHER_MAIN" "$WORK/othermod/main.go"
( cd "$WORK/othermod" && go mod init othermod >/dev/null 2>&1 && go mod tidy >/dev/null 2>&1 && go build -o "$WORK/wsserver-old" . ) || {
  echo "failed to build $OTHER_MAIN, does it have the same import set as this repo's main.go?" >&2
  exit 1
}

wait_up() {
  for i in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:$1/metrics" && return 0
    sleep 0.1
  done
  return 1
}

run_core() {
  local tag=$1 bin=$2 port=$3; shift 3
  "$WORK/fixture" "$WORK/$tag.db" >/dev/null
  "$bin" -addr ":$port" -db sqlite -dsn "file:$WORK/$tag.db?cache=shared" \
    -log-file "$WORK/$tag.log" -log-format json "$@" &
  local pid=$!
  wait_up "$port" || { kill $pid 2>/dev/null; return 1; }
  "$WORK/harness" -addr "127.0.0.1:$port" -mode core > "$WORK/$tag-results.json" 2> "$WORK/$tag-summary.txt"
  local status=$?
  kill $pid 2>/dev/null; wait $pid 2>/dev/null
  return $status
}

echo
echo "== running core scenarios against OLD (generous rate limits so they can't interfere) =="
run_core old "$WORK/wsserver-old" 8071 \
  -rate-limit-ip 100000 -rate-burst-ip 100000 -rate-limit-player 100000 -rate-burst-player 100000
OLD_STATUS=$?

echo "== running core scenarios against NEW (rate-limit flags left unset, shipped default) =="
run_core new "$WORK/wsserver-new" 8072
NEW_STATUS=$?

echo
echo "old: $(cat "$WORK/old-summary.txt")"
echo "new: $(cat "$WORK/new-summary.txt")"

echo
echo "== diffing per-scenario results =="
python3 - "$WORK/old-results.json" "$WORK/new-results.json" << 'PYEOF'
import json, sys
old = {r["name"]: r for r in json.load(open(sys.argv[1]))}
new = {r["name"]: r for r in json.load(open(sys.argv[2]))}
names = sorted(set(old) | set(new))
diffs = 0
for n in names:
    o, ne = old.get(n), new.get(n)
    if o is None or ne is None:
        print(f"SCENARIO ONLY ON ONE SIDE: {n} old={o} new={ne}")
        diffs += 1
        continue
    if o["pass"] != ne["pass"]:
        print(f"PASS/FAIL DIFFERS: {n} old.pass={o['pass']} new.pass={ne['pass']}")
        print(f"    old.detail={o['detail']}")
        print(f"    new.detail={ne['detail']}")
        diffs += 1
if diffs == 0:
    print("No pass/fail differences across any scenario.")
sys.exit(1 if diffs else 0)
PYEOF
RESULTS_DIFF=$?

echo
echo "== structurally diffing JSON logs (event/reason groups: counts + field sets, time & duration_ms excluded) =="
python3 - "$WORK/old.log" "$WORK/new.log" << 'PYEOF'
import json, sys

def load(path):
    out = []
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        d = json.loads(line)
        d.pop("time", None)
        d.pop("duration_ms", None)
        out.append(d)
    return out

def group(entries):
    g = {}
    for d in entries:
        # msg often embeds the port for the startup line; ignore msg for
        # grouping so that expected, incidental difference doesn't count.
        key = (d.get("event"), d.get("reason"))
        g.setdefault(key, []).append(d)
    return g

old = group(load(sys.argv[1]))
new = group(load(sys.argv[2]))
keys = sorted(set(old) | set(new), key=lambda k: (str(k[0]), str(k[1])))
mismatches = 0
for k in keys:
    if k == (None, None):
        continue  # lifecycle lines (listening/shutdown), expected to differ (port, timing)
    o, n = old.get(k, []), new.get(k, [])
    ofields = set().union(*[set(d) for d in o]) if o else set()
    nfields = set().union(*[set(d) for d in n]) if n else set()
    if len(o) != len(n) or ofields != nfields:
        mismatches += 1
        print(f"MISMATCH {k}: old_count={len(o)} new_count={len(n)} old_fields={ofields} new_fields={nfields}")
if mismatches == 0:
    print("Every event/reason group has identical counts and identical field sets.")
sys.exit(1 if mismatches else 0)
PYEOF
LOG_DIFF=$?

echo
if [ $OLD_STATUS -eq 0 ] && [ $NEW_STATUS -eq 0 ] && [ $RESULTS_DIFF -eq 0 ] && [ $LOG_DIFF -eq 0 ]; then
  echo "ALL GREEN: old and new are behaviorally identical on every covered scenario."
  exit 0
else
  echo "DIFFERENCES FOUND, see above."
  exit 1
fi
