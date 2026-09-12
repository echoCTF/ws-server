package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// farFutureExpiry is the expires_at used by every fixture row except the
// deliberately-expired one; validateToken doesn't check it anyway (see
// main.go), it just needs to be a valid, non-past-looking datetime.
const farFutureExpiry = "2099-01-01 00:00:00"

// Builds a ws_token sqlite DB with a fixed, disjoint set of fixture rows so
// each test scenario has its own token/player and none interfere with each
// other's connection-count state. Identical rows are used for both the old
// and new server builds so they're tested against the same data.
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fixture <dsn-path>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()

	rows := []struct {
		token, subject string
		playerID       *int
		isServer       int
		expires        string
	}{
		{"player-token-1", "player-1", intp(1), 0, farFutureExpiry}, // solo connect + client-data-frame test
		{"player-token-2", "player-2", intp(2), 0, farFutureExpiry}, // max-conns test, exclusive use
		{"player-token-3", "player-3", intp(3), 0, farFutureExpiry}, // publish-to-online test
		{"player-token-4", "player-4", intp(4), 0, farFutureExpiry}, // broadcast-all participant A
		{"player-token-5", "player-5", intp(5), 0, farFutureExpiry}, // broadcast-all participant B
		{"player-token-6", "player-6", intp(6), 0, farFutureExpiry}, // offline publish then connect+flush
		{"player-token-7", "player-7", intp(7), 0, farFutureExpiry}, // broadcast to specific offline player, never connects
		{"server-token-1", "server-1", nil, 1, farFutureExpiry},
		{"expired-player-token", "player-99", intp(99), 0, "2000-01-01 00:00:00"}, // expires_at in the past
	}

	if _, err := db.Exec(`DROP TABLE IF EXISTS ws_token`); err != nil {
		panic(err)
	}
	if _, err := db.Exec(`CREATE TABLE ws_token (
		token       TEXT NOT NULL PRIMARY KEY,
		player_id   INTEGER DEFAULT NULL,
		subject_id  TEXT NOT NULL,
		is_server   INTEGER NOT NULL DEFAULT 0,
		expires_at  DATETIME NOT NULL
	)`); err != nil {
		panic(err)
	}

	for _, rrow := range rows {
		_, err := db.Exec(
			`INSERT INTO ws_token (token, player_id, subject_id, is_server, expires_at) VALUES (?,?,?,?,?)`,
			rrow.token, rrow.playerID, rrow.subject, rrow.isServer, rrow.expires,
		)
		if err != nil {
			panic(fmt.Errorf("insert %s: %w", rrow.token, err))
		}
	}
	fmt.Println("fixture DB ready:", os.Args[1])
}

func intp(i int) *int { return &i }
