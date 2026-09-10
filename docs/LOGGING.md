# Logging

`ws-server` writes JSON by default (`logrus.JSONFormatter`), one object per
line, to `-log-file` or stdout. Pass `-log-format text` for a human-readable
format (`logrus.TextFormatter` with `FullTimestamp`; colors are disabled when
writing to a file).

## Conventions

- **`event` is a family.** It names the subsystem or event class:
  `ws_connect`, `ws_disconnect`, `ws_reject`, `ws_revalidate`, `ws_token_check`,
  `ws_queue`, `ws_deliver`. Multiple distinct situations can share an `event`
  value.
- **`reason` is the specific case.** Any event with more than one cause
  carries a `reason` field. `event` + `reason` together identify the exact
  situation. Events with a single cause omit `reason`.
- **Level is a property of `reason`, not of `event`.** The same `event` can
  appear at different levels; the level reflects whether the specific
  `reason` needs attention. Routine occurrences are `Info` or `Debug`;
  anomalies are `Warn`.

`grep event=ws_reject` collects every rejection regardless of severity.
`grep reason=invalid_token` narrows by situation. `grep 'level=warning'`
narrows by severity. Neither filter needs to know about the other.

The `event=` and `reason=` pairs survive `-log-format text`, so grepping by
them works in either format. JSON is still recommended when any downstream
tool parses log lines.

## Events

| `event` | Has `reason` | Meaning |
|---|---|---|
| `ws_connect` | no | A player connection was accepted and registered. |
| `ws_disconnect` | no | A player connection ended for any cause: clean close, read error, ping timeout, data-frame rejection. |
| `ws_reject` | yes | A connection was refused or closed by the server. |
| `ws_revalidate` | yes | Summary of one token-revalidation sweep. |
| `ws_token_check` | yes | A token-validation query failed against the database. |
| `ws_queue` | yes | Offline-queue behavior in `publishHandler`. |
| `ws_deliver` | yes | Message delivery, live or from the offline queue, and broadcasts. |

## `ws_reject` reasons

| `reason` | Level | Where | Cause | Client sees |
|---|---|---|---|---|
| `rate_limit_ip` | Warn | `wsHandler` | Client IP exceeded `-rate-limit-ip` / `-rate-burst-ip`. | HTTP 429, no WS handshake. Close code 1006. |
| `rate_limit_player` | Warn | `wsHandler` | Player exceeded `-rate-limit-player` / `-rate-burst-player`. | HTTP 429, no WS handshake. Close code 1006. |
| `upgrade_failed` | Warn | `wsHandler` | HTTP upgrade itself failed: bad headers, bad `Sec-WebSocket-Version`, or origin rejected by `upgrader.CheckOrigin`. | Connection error, no WS handshake. |
| `missing_token` | Warn | `wsHandler` | No `?token=` query parameter. | Close frame, code 1008, `"missing token"`. |
| `invalid_token` | Warn | `wsHandler` | Token not present in `ws_token`. | Close frame, code 1008, `"invalid token"`. |
| `token_check_failed` | Warn | `wsHandler` | The DB query for the token failed. The token may be valid; the server could not check. | Close frame, code 1013, `"token check failed"`. |
| `too_many_connections` | Warn | `wsHandler` | Player already has `-max-conns` open connections. | Close frame, code 1013, `"too many connections"`. |
| `client_data_frame` | Warn | `wsHandler` read loop | Client sent a data frame. The gateway never reads application data from players. | Close frame, code 1008, `"client messages not accepted"`. |
| `token_not_found` | Info | `revalidateOnce` | Token was valid at connect time but is no longer in `ws_token`: the expiry sweep removed it, an admin revoked it, or the player was deleted and the FK cascade took their tokens. Carries `source=revalidation`. | Close frame, code 1008, `"token not found"`. |

The two `rate_limit_*` reasons are the only ones rejected before the upgrade:
nginx sees a real `429`, and the browser surfaces the failed handshake as
close code 1006. `wsclient.js`'s existing `scheduleReconnect()` handles this
by backing off exponentially, so no client change is needed.

`token_not_found` is the only non-Warn reason in this family. It fires as a
routine consequence of revalidation doing its job. Every other reason
represents something a human should look at.

## `ws_token_check` reasons

| `reason` | Level | Meaning |
|---|---|---|
| `db_error` | Warn | The token-validation query failed for a reason other than "no matching row" (DB unreachable, timeout, connection refused). Carries `err`. |

A missing token row is **not** logged here. That outcome is returned to the
caller as `ErrTokenNotFound` and logged by whichever handler rejected the
connection (`invalid_token`, `token_not_found`, etc.). This event only fires
when the database could not be queried.

## `ws_revalidate` reasons

| `reason` | Level | Meaning |
|---|---|---|
| `sweep_completed` | Debug | The sweep ran, checked every connection, and found nothing wrong. Hidden at the default `-log-level info`. |
| `sweep_backend_errors` | Warn | At least one DB query failed during the sweep. Connections were **not** closed for those tokens; the sweep retries next tick. A sustained non-zero value points at the database, not at clients. |

Both carry `checked`, `closed`, and `duration_ms`. `sweep_backend_errors`
also carries `backend_errors`.

### Examples

Clean sweep (only visible at `-log-level debug`):

```json
{"event":"ws_revalidate","reason":"sweep_completed","checked":142,"closed":0,"duration_ms":3,"level":"debug","msg":"Token revalidation sweep completed","time":"..."}
```

DB errors during the sweep:

```json
{"event":"ws_revalidate","reason":"sweep_backend_errors","checked":142,"closed":0,"backend_errors":5,"duration_ms":210,"level":"warning","msg":"Token revalidation sweep completed with DB errors","time":"..."}
```

In the error case, connections whose tokens could not be checked were left
open. The server retries on the next tick. A sustained `backend_errors > 0`
means the database is unreachable or slow, not that clients are misbehaving.

## `ws_queue` reasons

| `reason` | Level | Cause |
|---|---|---|
| `queued` | Info | Player not connected; the message was appended to the offline queue. Carries `subject_id`, `player_id`, `app_event`. |
| `queue_overflow` | Warn | The offline queue for this player was at `-max-queued`; the oldest message was dropped to make room. Carries `player_id`, `app_event`, `limit`. |

`queue_overflow` is the only reason in this family that indicates data loss.
It is logged at `Warn`.

## `ws_deliver` reasons

| `reason` | Level | Cause |
|---|---|---|
| `live` | Info | `publishHandler` wrote the message to one or more live connections. Carries `subject_id`, `player_id`, `app_event`, `connections`. |
| `offline_flush` | Info | `flushPendingMessages` delivered a queued message to a newly connected client. Carries `player_id`, `app_event`, `queued_at`, `age_ms`. |
| `broadcast_player` | Info | `broadcastHandler` with a `player_id` wrote the message to that player's connections. Carries `subject_id`, `player_id`, `app_event`, `delivered`. |
| `broadcast_all` | Info | `broadcastHandler` without a `player_id` wrote the message to every connected client. Carries `subject_id`, `app_event`, `delivered`. |

`connections` is the number of connections the message was written to for that
player; `delivered` is the total number of writes for a broadcast. They can
differ when a player has multiple open connections.

## Fields

**All `ws_connect`, `ws_disconnect`, and `ws_reject` lines carry:**

| Field | Source | Notes |
|---|---|---|
| `event` | constant | `ws_connect`, `ws_disconnect`, or `ws_reject`. |
| `reason` | constant | Only on `ws_reject`. |
| `ip` | `clientIP(r)` | The real client IP when `-trust-xff` is enabled and the server is behind a trusted proxy; otherwise the direct peer address. The port is stripped. |
| `origin` | `Origin` header | Passed through by nginx unchanged; only trustworthy if the proxy strips it from clients. |
| `xff` | `X-Forwarded-For` header | The raw header value, logged for diagnostics regardless of `-trust-xff`. |

**Additional fields on specific paths:**

- `token` — the raw player token. Present on `invalid_token`,
  `token_check_failed`, `too_many_connections`, `client_data_frame`,
  `token_not_found`, `ws_connect`, `ws_disconnect`, and
  `rate_limit_player`. Absent on `missing_token` (there is nothing to log),
  `upgrade_failed` (may be present if the token was supplied), and
  `rate_limit_ip` (pre-auth, no token read).
- `player_id` — the resolved player ID. Present on `too_many_connections`,
  `client_data_frame`, `token_not_found`, `ws_connect`, `ws_disconnect`,
  `rate_limit_player`, `ws_queue`, `ws_deliver`.
- `subject_id` — the server token's identity. Present on `ws_queue` and
  `ws_deliver` lines produced by `/publish` and `/broadcast`.
- `app_event` — the payload's own `event` value, when it is not the line's
  `event`. Present on `ws_queue` and `ws_deliver`.
- `err` — the underlying error string. Present on `upgrade_failed`,
  `token_check_failed`, and `ws_token_check`.
- `type` — the WebSocket message type of the rejected data frame. Present on
  `client_data_frame`.
- `current`, `limit` — connection count and cap. Present on
  `too_many_connections`; `limit` also on `queue_overflow`.
- `source` — set to `revalidation` on `token_not_found` lines, to distinguish
  them from any future `token_not_found` produced at connect time.
- `queued_at`, `age_ms` — original queue timestamp and age at delivery.
  Present on `offline_flush`.
- `connections`, `delivered` — write counts. See `ws_deliver` reasons table.

## Lifecycle events

Two lines are exempt from the family convention: they describe the process,
not a connection.

```json
{"level":"info","msg":"Server listening on :8080","time":"..."}
{"level":"info","msg":"Shutting down server...","time":"..."}
```

## Behind nginx

`ip` is derived from `X-Forwarded-For` when `-trust-xff` is set, and from the
direct peer address otherwise. Behind nginx, `-trust-xff` should be enabled,
and nginx must overwrite the incoming header:

```nginx
location /ws {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_read_timeout 300s;
}
```

`proxy_set_header X-Forwarded-For $remote_addr;` replaces whatever the client
sent with the address nginx actually saw, so `ip` (and the raw `xff` field)
reflect the real client. Without this, `xff` is attacker-controlled and
`-trust-xff` must stay off.

`proxy_read_timeout` must exceed `-revalidate-period` plus the `pongWait`
heartbeat, or nginx will close idle connections out from under the server.

### Mapping rejections to access log status codes

`ws-server` sets an `X-WS-Reject` response header on the `101` when a
token-related rejection is about to close the connection. Nginx can map that
header to a status code so the access log shows a real error instead of a
successful upgrade:

```nginx
map $upstream_http_x_ws_reject $ws_logged_status {
    default                 $status;
    "missing_token"         401;
    "invalid_token"         401;
    "token_check_failed"    503;
}

log_format ws '$remote_addr - $remote_user [$time_local] '
              '"$request" $ws_logged_status $body_bytes_sent '
              '"$http_referer" "$http_user_agent" '
              'reject=$upstream_http_x_ws_reject';

location /ws {
    # ... proxy settings as above ...
    access_log /var/log/nginx/ws_access.log ws;
}
```

`rate_limit_ip` and `rate_limit_player` do not need a `map` entry: they are
rejected before the upgrade with a real HTTP `429`, so `$status` already
reflects the rejection.

## Known gaps

- **`token_not_found` and `ws_token_check` do not carry `origin` / `xff` /
  a fresh `ip`.** `token_not_found` fires from `revalidateOnce` with no
  `*http.Request` in scope; `ws_token_check` fires from `validateToken`,
  which is called by `publishHandler` and `broadcastHandler` where the
  request does not correspond to a client connection. Adding connection-time
  metadata would require threading request fields through `wsConnection`.
- **`token` is logged raw.** Anyone with read access to the log can reuse the
  token until it expires or is revoked. If this is a concern, hash it before
  logging — the change is local to the call sites that log it.