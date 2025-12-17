# WebSocket Message Gateway

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

* `-addr` – HTTP server address (default `:8080`)
* `-db` – Database driver (`sqlite` or `mysql`)
* `-dsn` – Database DSN
* `-origins` – Comma-separated allowed WS origins
* `-max-conns` – Max WS connections per player
* `-max-queued` – Max offline queued messages per player
* `-offline-ttl` – Offline message TTL
* `-rate-limit` – Messages per rate period per server token
* `-rate-period` – Rate limit window
* `-revalidate-period` – Token revalidation interval

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

./ws-gateway \
  -db mysql \
  -dsn "root@/echoCTF"
```

## Notes

* Designed to be stateless except for in-memory queues
* Offline messages are best-effort (not persisted)
* Suitable for game backends, notification systems, and real-time apps
