## Create a token to test
```sql
INSERT INTO ws_token (token, subject_id, is_server, expires_at)
VALUES ('test-server-token', 'test-server', 1, '2099-01-01 00:00:00');
```

## Test ws with bogus token

```sh
curl -i \
  -H "Connection: Upgrade" \
  -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  "http://127.0.0.1:8080/ws?token=bogus"
```

## Publish to a single player

```sh
curl -i -X POST http://127.0.0.1:8080/publish \
  -H "Authorization: Bearer YOUR_SERVER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "player_id": "1",
    "event": "notification",
    "payload": {"title": "Hello", "type": "info"}
  }'
```

## Broadcast to all connected players

```sh
curl -i -X POST http://127.0.0.1:8080/broadcast \
  -H "Authorization: Bearer YOUR_SERVER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "event": "reload",
    "payload": {}
  }'
```
