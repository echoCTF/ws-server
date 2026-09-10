# WebSocket Message Gateway

[![CodeQL](https://github.com/echoCTF/ws-server/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/echoCTF/ws-server/actions/workflows/github-code-scanning/codeql)
[![Dependabot Updates](https://github.com/echoCTF/ws-server/actions/workflows/dependabot/dependabot-updates/badge.svg)](https://github.com/echoCTF/ws-server/actions/workflows/dependabot/dependabot-updates)
[![Dependency Graph](https://github.com/echoCTF/ws-server/actions/workflows/dependabot/update-graph/badge.svg)](https://github.com/echoCTF/ws-server/actions/workflows/dependabot/update-graph)
[![Go Format Check (format.yml)](https://github.com/echoCTF/ws-server/actions/workflows/format.yml/badge.svg)](https://github.com/echoCTF/ws-server/actions/workflows/format.yml)
[![Lint Go Code (lint.yml)](https://github.com/echoCTF/ws-server/actions/workflows/lint.yml/badge.svg)](https://github.com/echoCTF/ws-server/actions/workflows/lint.yml)

A lightweight WebSocket gateway for delivering real-time messages to players, with server-side publishing, offline queueing, rate limiting, and Prometheus metrics.

## Features

* WebSocket connections per player (multiple connections supported)
* Token-based authentication (players & servers)
* Offline message queue with TTL
* Per-IP and per-player connection-attempt rate limiting on `/ws`
* Broadcast and targeted messaging
* Automatic token revalidation
* Prometheus metrics endpoint
* Graceful shutdown
* JSON or plaintext logging with structured `event` / `reason` fields

## Architecture

* **Players** connect via WebSocket (`/ws`) using player tokens
* **Servers** publish messages via HTTP (`/publish`, `/broadcast`) using server tokens
* Tokens are validated against a SQL database
* Messages are delivered immediately or queued if the player is offline

## Endpoints

### Broadcast vs. Publish: offline behavior

The two endpoints differ in what happens when the target player is not
currently connected:

- **`/publish` with `player_id`** - queues the message for offline delivery.
  If the player connects within `-offline-ttl`, the message is flushed to the
  new connection.
- **`/broadcast` with `player_id`** - writes to the player's live connections
  only. If the player is offline, the message is discarded and the response
  is still `200 OK`.
- **`/broadcast` without `player_id`** - writes to every currently-connected
  client. There is nothing to queue against, since there is no single target.

If the intent is "deliver this to player X, now or later," use `/publish`.
If the intent is "deliver this to player X if they happen to be connected,"
use `/broadcast` with `player_id`.

### WebSocket

* `GET /ws?token=PLAYER_TOKEN`

### Publish (single player)

* `POST /publish`
* Header: `Authorization: Bearer SERVER_TOKEN`
* Body:

```json
{
  "player_id": "1",
  "event": "event_name",
  "payload": { "any": "json" }
}
```

### Broadcast

* `POST /broadcast`
* Header: `Authorization: Bearer SERVER_TOKEN`
* Body (all players):

```json
{
  "event": "event_name",
  "payload": { "any": "json" }
}
```

* Body (single player):

```json
{
  "player_id": "1",
  "event": "event_name",
  "payload": { "any": "json" }
}
```

### Metrics

* `GET /metrics` (Prometheus format)

## Configuration (Flags)

* `-addr` - Server address (default `:8080`)
* `-db` - Database driver (default `sqlite`)
* `-dsn` - Database DSN (default `file:ws_tokens.db?cache=shared`)
* `-origins` - Comma-separated allowed WS origins
* `-log-file` - Path to log file
* `-log-level` - Log level (default `info`)
* `-log-format` - Log format: `json` (default) or `text`
* `-pid-file` - Path to PID file
* `-max-conns` - Max WS connections per player (default `10`)
* `-max-queued` - Max offline queued messages per player (default `100`)
* `-offline-ttl` - Offline message TTL (default `10s`)
* `-rate-limit-ip` - WS connection attempts per second per client IP (default `5`)
* `-rate-burst-ip` - WS connection attempt burst per client IP (default `20`)
* `-rate-limit-player` - WS connection attempts per second per player (default `2`)
* `-rate-burst-player` - WS connection attempt burst per player (default `5`)
* `-trust-xff` - Trust `X-Forwarded-For` for the client IP. Only enable when the server is reachable exclusively through a trusted proxy that overwrites the header (default `false`)
* `-revalidate-period` - Token revalidation interval (default `1m`)
* `-daemon` - Run process as a daemon

NOTE: If `MaxOpenConns(1)` ever shows up as a bottleneck, the alternative is
WAL mode in the DSN, which allows one writer plus concurrent readers:
```
-dsn "file:ws_tokens.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
```

## Database

Required table:

```sql
CREATE TABLE IF NOT EXISTS ws_token (
    token       VARBINARY(32) NOT NULL PRIMARY KEY,
    player_id   INT(10) UNSIGNED DEFAULT NULL,
    subject_id  VARBINARY(32) NOT NULL,
    is_server   TINYINT(1) NOT NULL DEFAULT 0,
    expires_at  DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ws_token_player_id ON ws_token (player_id);
CREATE INDEX IF NOT EXISTS idx_ws_token_server_exp ON ws_token (is_server, expires_at);
```

* `player_id` is used for player tokens
* `subject_id` is used for server tokens

### Token expiration

`ws-server` does not check `expires_at` itself; `validateToken()` only checks that a row with the
given token and `is_server` value exists. Expiration is enforced out-of-band by a DB event:

```sql
CREATE EVENT ev_ws_token_expiration
ON SCHEDULE EVERY 10 MINUTE ON COMPLETION PRESERVE ENABLE
DO
BEGIN
  DELETE FROM ws_token WHERE is_server=0 AND expires_at<=NOW();
END;
```

* Only `is_server=0` (player) rows are swept. A player token can stay usable for up to ~10 minutes
  past its own `expires_at`, until the next event run.
* `is_server=1` (server) rows are never touched by this event and are not deleted by `ws-server`
  either; server token lifecycle is managed wherever tokens are issued.

## Metrics

* `ws_active_connections`
* `ws_messages_published_total`
* `ws_messages_delivered_total`

## Logging

All output is JSON by default, one object per line, written to `-log-file` or
stdout. Set `-log-format text` for a human-readable format (colors disabled
when writing to a file).

Lines are namespaced by `event` (family) and, where a family has more than one
cause, `reason` (specific case). Log level is a property of the reason, not
the event.

See [LOGGING.md](LOGGING.md) for the event/reason/level reference, the fields
on each line, and the nginx configuration required for client IPs to appear
correctly.

## Build & Run

```bash
go build
./ws-server \
  -db sqlite \
  -dsn file:ws_tokens.db?cache=shared

# or
./ws-server \
  -db mysql \
  -dsn "root@/echoCTF"
```

## Notes

* Designed to be stateless except for in-memory queues
* Offline messages are best-effort (not persisted)
* Suitable for game backends, notification systems, and real-time apps
