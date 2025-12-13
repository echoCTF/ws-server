-- ws_tokens.sql
CREATE TABLE IF NOT EXISTS ws_token (
    token        VARCHAR(64) PRIMARY KEY,
    subject_id   VARCHAR(64) NOT NULL unique,
    expires_at   DATETIME NOT NULL,
    is_server    BOOLEAN DEFAULT 0
);

-- Player token
INSERT INTO ws_token (token, subject_id, expires_at, is_server)
VALUES ('player123token', 'player1', DATETIME('now', '+1 day'), 0);

-- Server token
INSERT INTO ws_token (token, subject_id, expires_at, is_server)
VALUES ('server123token', 'server1', DATETIME('now', '+1 day'), 1);
