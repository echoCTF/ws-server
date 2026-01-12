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
* Per-server-token rate limiting
* Broadcast and targeted messaging
* Automatic token revalidation
* Prometheus metrics endpoint
* Graceful shutdown

## Architecture

* **Players** connect via WebSocket (`/ws`) using player tokens
* **Servers** publish messages via HTTP (`/publish`, `/broadcast`) using server tokens
* Tokens are validated against a SQL database
* Messages are delivered immediately or queued if the player is offline

## Endpoints

### WebSocket

* `GET /ws?token=PLAYER_TOKEN`

### Publish (single player)

* `POST /publish`
* Header: `Authorization: Bearer SERVER_TOKEN`
* Body:

```json
{
  "player_id": "player123",
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
  "player_id": "player123",
  "event": "event_name",
  "payload": { "any": "json" }
}
```

### Metrics

* `GET /metrics` (Prometheus format)

## Configuration (Flags)

* `-addr` – Server address (default `:8080`)
* `-db` – Database driver (default `sqlite`)
* `-dsn` – Database DSN (default `file:ws_tokens.db?cache=shared`)
* `-origins` – Comma-separated allowed WS origins
* `-log-file` – Path to log file
* `-log-level` – Log level (default `info`)
* `-pid-file` – Path to PID file
* `-max-conns` – Max WS connections per player (default `5`)
* `-max-queued` – Max offline queued messages per player (default `100`)
* `-offline-ttl` – Offline message TTL (default `10s`)
* `-rate-limit` – Messages per rate period per server token (default `10`)
* `-rate-period` – Rate limit window (default `1s`)
* `-revalidate-period` – Token revalidation interval (default `1m`)
* `-daemon` – Run process as a daemon

## Database

Required table:

```sql
CREATE TABLE IF NOT EXISTS ws_token (
    token       VARBINARY(32) NOT NULL PRIMARY KEY,
    player_id   INTEGER(10) UNSIGNED DEFAULT NULL,
    subject_id  VARBINARY(32) NOT NULL,
    is_server   BOOLEAN NOT NULL,
    expires_at  DATETIME(6) NOT NULL
);
CREATE INDEX idx_ws_token_server_exp ON ws_token (is_server, expires_at);
```

* `player_id` is used for player tokens
* `subject_id` is used for server tokens

## Metrics

* `ws_active_connections`
* `ws_messages_published_total`
* `ws_messages_delivered_total`

## Build & Run

```bash
go build
./ws-gateway \
  -db sqlite \
  -dsn file:ws_tokens.db?cache=shared

# or
./ws-gateway \
  -db mysql \
  -dsn "root@/echoCTF"
```

## Notes

* Designed to be stateless except for in-memory queues
* Offline messages are best-effort (not persisted)
* Suitable for game backends, notification systems, and real-time apps
