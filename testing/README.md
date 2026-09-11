# ws-server test suite

## Layout

- `../main_test.go` — white-box unit tests (`go test ./...`), next to `main.go` since
  Go test files must share the package. Covers: rate limiter opt-in gating (disabled by
  default, respects burst once enabled, buckets are per-key), `checkConnectionLimit`,
  the `postUpgradeRejects` table (all three reasons present, `missing_token` excludes the
  token field, the other two include it, per LOGGING.md), `clientIP`'s `-trust-xff`
  behavior, and `validateToken` against a real sqlite DB (valid/invalid/wrong-`is_server`/
  expired-but-present rows).
- `fixture/` — seeds a `ws_token` sqlite DB with a fixed, disjoint set of tokens (one
  per scenario, so nothing shares state across tests).
- `harness/` — a black-box HTTP/WS client that drives a fixed battery of scenarios
  against a *running* server and reports pass/fail per scenario. Three modes:
  `core` (14 scenarios: auth, rate-limit-independent), `ratelimit-default` (confirms no
  429 fires with no rate-limit flags set), `ratelimit-explicit` (confirms 429 fires once
  flags are set and the burst is exceeded).
- `stress/` — concurrent connect/disconnect/publish/broadcast client, for running the
  server under `go build -race` with real parallel load.
- `run_tests.sh` — `go vet` + `go test -race` + the harness against a fresh build of
  *this* repo. Run this on every change.
- `diff_against.sh <path/to/other/main.go>` — builds the given `main.go` in a throwaway
  module, runs the *same* harness against both it and this repo's current build, diffs
  the pass/fail results and the structured JSON logs (grouped by `event`/`reason`, field
  sets and counts compared, `time`/`duration_ms` excluded since those always differ).
  This is what actually answers "did this change break anything?" for two versions.

## Running

```bash
# from the repo root (where main.go and go.mod live)
./testing/run_tests.sh

# compare current main.go against a previous version
./testing/diff_against.sh /path/to/previous/main.go
```

Both exit non-zero on any failure, safe to wire into CI.

## What this run already found

`diff_against.sh` was run against the pre-refactor `main.go` (the version before
`wsHandler` was split into `preUpgradeCheck`/`sendPostUpgradeReject`/
`checkConnectionLimit`/`serveConnection`). Result: all 14 core scenarios pass
identically on both, and every `event`/`reason` log group has identical counts and
identical field sets between the two builds. `go test -race` and a 5-second, 40-worker
concurrent stress run (`stress/`) against a `-race` build of the refactored server
produced no data races.

## What this does NOT cover

- MySQL (`-db mysql`); everything above runs against sqlite only.
- The `startTokenRevalidation` sweep's behavior when the DB is actually unreachable
  mid-sweep (`sweep_backend_errors`) isn't exercised, that needs a DB you can fail on
  demand, not covered here.
- `-daemon` / PID file handling.
- Anything about `-trust-xff` beyond the unit test's header-parsing check. There's no
  scenario running the server behind a real reverse proxy.

Extend `testing/harness/main.go` for anything you specifically want covered before your
next change, that's the file that defines "correct behavior" for `diff_against.sh`.
