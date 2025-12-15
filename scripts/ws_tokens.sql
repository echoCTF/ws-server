-- ws_tokens.sql
CREATE TABLE IF NOT EXISTS ws_token (
    token       VARBINARY(32) NOT NULL PRIMARY KEY,
    player_id   INTEGER(10) UNSIGNED DEFAULT NULL,
    subject_id  VARBINARY(32) NOT NULL,
    is_server   BOOLEAN NOT NULL,
    expires_at  DATETIME(6) NOT NULL
);
CREATE INDEX idx_ws_token_server_exp ON ws_token (is_server, expires_at);
-- Player token
INSERT INTO ws_token (token, subject_id, expires_at, is_server)
VALUES ('player123token', 'player1', DATETIME('now', '+1 day'), 0);

-- Server token
INSERT INTO ws_token (token, subject_id, expires_at, is_server)
VALUES ('server123token', 'server1', DATETIME('now', '+1 day'), 1);
