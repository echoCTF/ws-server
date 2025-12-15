# ws-server

A simple ws-server to publish messages to connected players.

A lightweight, production-ready WebSocket push server written in Go.
It allows backend services to publish events to connected players in real time using authenticated WebSocket connections
and a secure HTTP publish API.

Designed for echoCTF and real-time applications.

## Features

* WebSocket connections per player (multiple concurrent connections supported)
* Token-based authentication (player tokens & server tokens)
* HTTP API for server-side message publishing
* Safe concurrent connection management
* Heartbeat (ping/pong)
* Rate limiting per server token
* Prometheus metrics
* SQLite or MySQL backend
* Graceful shutdown

## Architecture Overview

```
Clients (Players)
  |
  | WebSocket (/ws?token=PLAYER_TOKEN)
  v
ws-server
  ^
  | HTTP POST /publish (Bearer SERVER_TOKEN)
  |
Backend Services
```

* Players connect via WebSocket using a player token
* Backend servers send messages to players using an authenticated HTTP API
* Messages are delivered to all active connections for a player

## Authentication Model

Tokens are stored in a database table:

* Player tokens authenticate WebSocket connections
* Server tokens authenticate publish requests

### Token Behavior

Player token:

* Used with `/ws`
* Can only receive messages

Server token:

* Used with `/publish`
* Can only send messages

## Database Schema

Table: ws_token

```sql
REATE TABLE IF NOT EXISTS ws_token (
    token       VARBINARY(32) NOT NULL PRIMARY KEY,
    player_id   INTEGER(10) UNSIGNED DEFAULT NULL,
    subject_id  VARBINARY(32) NOT NULL,
    is_server   BOOLEAN NOT NULL,
    expires_at  DATETIME(6) NOT NULL
);
CREATE INDEX idx_ws_token_server_exp ON ws_token (is_server, expires_at);
```

* For player tokens, player_id should be set
* For server tokens, subject_id identifies the publishing service

## API Endpoints

### WebSocket Connect

`GET /ws?token=PLAYER_TOKEN`

* Authenticates using a player token
* Keeps connection alive using ping/pong
* Multiple connections per player supported

### Publish Message

`POST /publish`

Headers:

```
Authorization: Bearer SERVER_TOKEN
Content-Type: application/json
```

Request body:

```
{
  "player_id": "player123",
  "event": "match_found",
  "payload": {
    "match_id": "abc123"
  }
}
```

Behavior:

* Delivers the message to all active connections for the player
* Rate-limited per server token
* Returns 200 OK even if player is not connected

### Metrics

`GET /metrics`

Prometheus metrics exposed:

* ws_active_connections
* ws_messages_published_total
* ws_messages_delivered_total

## Configuration

Command-line flags:

```
 -addr     : HTTP server address (default :8080)
 -db       : Database driver: sqlite or mysql
 -dsn      : Database DSN
 -origins  : Comma-separated allowed WebSocket origins
```

SQLite Example:

```
./ws-server \
  -addr :8080 \
  -db sqlite \
  -dsn file:ws_tokens.db \
  -origins https://example.com,https://app.example.com
```

MySQL Example:
```
./wsserver \
 -addr ":8888" \
 -db mysql \
 -dsn "root@/echoCTF"
```

If -origins is not set, all origins are allowed.

## Rate Limiting

* Applied per server token
* Default: 10 messages per second
* Enforced in-memory
* Returns HTTP 429 when exceeded

## Running Locally

Donwload latest release binary for your platform or build locally by running

```
go build -o ws-server
./ws-server
```

## Testing

In order to test that the system is running properly

1. Import sample sql to create an SQLite database for local testing  `sqlite3 ws_tokens.db < scripts/ws_tokens.sql`
2. Run the server `./ws-server`
3. Install the needed modules for the scripts to run `cd scripts && npm i`
4. Run the player test client `node scripts/player_test.js`
5. Run the server test client `node scripts/server_publish.js`

You should see a message from the server showing the player connected.

## Logging

* Structured JSON logs via logrus
* Includes:
  * Player connect/disconnect
  * Message delivery
  * Invalid token attempts
  * Rate limit violations

## Graceful Shutdown

* Handles `SIGINT` / `SIGTERM`
* Stops accepting new connections
* Closes all active WebSocket connections
* Shuts down within 5 seconds

## Use Cases

* Game event delivery
* Notifications
* Real-time messaging
* Presence-aware systems

## Notes

This server is intentionally minimal:

* No message persistence
* No reconnect replay
* No client-to-client messaging

It is designed to be fast, predictable, and easy to integrate with existing backend services.

## License

MIT
