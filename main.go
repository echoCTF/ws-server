package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	_ "modernc.org/sqlite"
)

// ///////////////////
// GLOBAL CONFIG
// ///////////////////
var (
	dbDSN                      string
	dbDriver                   string
	serverAddr                 string
	allowedOrigins             []string
	pongWait                   = 60 * time.Second
	pingPeriod                 = 30 * time.Second
	offlineTTL                 time.Duration
	maxQueuedMessagesPerPlayer int
	maxConnectionsPerPlayer    int
	tokenRevalidationPeriod    time.Duration
	logFile                    string
	logLevel                   string
	daemonize                  bool
	pidFile                    string
	logFormat                  string
	rateLimitIP                int
	rateBurstIP                int
	rateLimitPlayer            int
	rateBurstPlayer            int
	rateLimitIPEnabled         bool
	rateLimitPlayerEnabled     bool
	trustXFF                   bool
	maxHeaderValueLen          int
)

// Log field names and event-name values that repeat across many
// logrus.Fields{} literals. Named here once so a rename only touches one
// place (and so goconst doesn't flag the duplication).
const (
	fieldEvent     = "event"
	fieldReason    = "reason"
	fieldToken     = "token"
	fieldPlayerID  = "player_id"
	fieldAppEvent  = "app_event"
	fieldOrigin    = "origin"
	fieldXFF       = "xff"
	fieldCookie    = "cookie"
	fieldSubjectID = "subject_id"

	eventWSDeliver = "ws_deliver"
	eventWSReject  = "ws_reject"
)

type wsConnection struct {
	conn  *websocket.Conn
	token string
	mu    sync.Mutex
}

// writeJSON serializes a JSON write against any other writer on this conn.
// Gorilla requires at most one concurrent writer per connection for all
// write methods except WriteControl and Close.
func (wc *wsConnection) writeJSON(v interface{}) error {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.conn.WriteJSON(v)
}

// writeClose sends a close frame, serialized against other writers.
func (wc *wsConnection) writeClose(code int, reason string) error {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
	)
}

// revalEntry is a snapshot of a live connection, taken before the DB sweep,
// so the sweep can run without holding mu.
type revalEntry struct {
	playerID string
	conn     *websocket.Conn
	token    string
}

// ///////////////////
// GLOBALS
// ///////////////////
var (
	db       *sql.DB
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			if len(allowedOrigins) == 0 {
				return true
			}
			origin := r.Header.Get("Origin")
			for _, o := range allowedOrigins {
				if origin == o {
					return true
				}
			}
			return false
		},
	}

	mu sync.Mutex

	// Map of playerID -> connections
	// Each connection stores the websocket.Conn and its token
	players = make(map[string]map[*websocket.Conn]*wsConnection)

	// Offline messages
	pendingMu       sync.Mutex
	pendingMessages = make(map[string][]pendingMessage)

	// Metrics
	connections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ws_active_connections",
		Help: "Number of active WS connections",
	})
	messagesPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ws_messages_published_total",
		Help: "Total messages published via the API",
	})
	messagesDelivered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ws_messages_delivered_total",
		Help: "Total messages successfully sent to connected players",
	})

	// logFileHandle holds the open log file so it can be closed on shutdown.
	logFileHandle *os.File
)

type rateEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

var (
	ipLimiters     = make(map[string]*rateEntry)
	ipMu           sync.Mutex
	playerLimiters = make(map[string]*rateEntry)
	playerMu       sync.Mutex
)

// clientIP returns the address used for IP-keyed rate limiting. When
// trustXFF is set, it trusts X-Forwarded-For, which nginx overwrites with
// $remote_addr. Otherwise it uses the direct peer address, which is not
// spoofable but is the proxy's address when behind nginx.
func clientIP(r *http.Request) string {
	if trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// nginx sets a single value; take the leftmost to be safe
			// against a client that pre-populated the header.
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func allowIP(ip string) bool {
	if !rateLimitIPEnabled {
		return true
	}
	ipMu.Lock()
	defer ipMu.Unlock()
	e, ok := ipLimiters[ip]
	if !ok {
		e = &rateEntry{
			lim: rate.NewLimiter(rate.Limit(rateLimitIP), rateBurstIP),
		}
		ipLimiters[ip] = e
	}
	e.lastSeen = time.Now()
	return e.lim.Allow()
}

func allowPlayer(playerID string) bool {
	if !rateLimitPlayerEnabled {
		return true
	}
	playerMu.Lock()
	defer playerMu.Unlock()
	e, ok := playerLimiters[playerID]
	if !ok {
		e = &rateEntry{
			lim: rate.NewLimiter(rate.Limit(rateLimitPlayer), rateBurstPlayer),
		}
		playerLimiters[playerID] = e
	}
	e.lastSeen = time.Now()
	return e.lim.Allow()
}

func startRateLimiterCleanup(stopCh <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				cutoff := time.Now().Add(-30 * time.Minute)
				ipMu.Lock()
				for k, e := range ipLimiters {
					if e.lastSeen.Before(cutoff) {
						delete(ipLimiters, k)
					}
				}
				ipMu.Unlock()
				playerMu.Lock()
				for k, e := range playerLimiters {
					if e.lastSeen.Before(cutoff) {
						delete(playerLimiters, k)
					}
				}
				playerMu.Unlock()
			}
		}
	}()
}

// parseFlags parses command-line flags into global configuration variables.
// It supports database driver, DSN, server address, log file/level/format, PID
// file, WS server limits, and the /ws connection-attempt rate limiter.
//
// Rate limiting is opt-in: the limiter only turns on if the operator actually
// passed one of the rate flags. Returns an error if a rate flag was passed
// with a value that would make the limiter reject everything.
func parseFlags() error {
	var origins string

	flag.StringVar(&dbDriver, "db", "sqlite", "Database driver")
	flag.StringVar(&dbDSN, "dsn", "file:ws_tokens.db?cache=shared", "Database DSN")
	flag.StringVar(&serverAddr, "addr", ":8080", "Server address")
	flag.StringVar(&origins, "origins", "", "Allowed WS origins")
	flag.StringVar(&logFile, "log-file", "", "Path to log file")
	flag.StringVar(&logLevel, "log-level", "info", "Log level")
	flag.StringVar(&logFormat, "log-format", "json", "Log format: json or text")
	flag.StringVar(&pidFile, "pid-file", "", "Path to PID file")
	flag.IntVar(&maxQueuedMessagesPerPlayer, "max-queued", 100, "Max offline queued messages per player")
	flag.IntVar(&maxConnectionsPerPlayer, "max-conns", 10, "Max concurrent WS connections per player")
	flag.DurationVar(&tokenRevalidationPeriod, "revalidate-period", time.Minute, "Token revalidation interval")
	flag.DurationVar(&offlineTTL, "offline-ttl", 10*time.Second, "Offline message TTL")
	flag.IntVar(&rateLimitIP, "rate-limit-ip", 20, "WS connection attempts per second per client IP (disabled unless this or -rate-burst-ip is set)")
	flag.IntVar(&rateBurstIP, "rate-burst-ip", 60, "WS connection attempt burst per client IP (disabled unless this or -rate-limit-ip is set)")
	flag.IntVar(&rateLimitPlayer, "rate-limit-player", 3, "WS connection attempts per second per player (disabled unless this or -rate-burst-player is set)")
	flag.IntVar(&rateBurstPlayer, "rate-burst-player", 10, "WS connection attempt burst per player (disabled unless this or -rate-limit-player is set)")
	flag.BoolVar(&trustXFF, "trust-xff", false, "Trust X-Forwarded-For for client IP (only behind a trusted proxy)")
	flag.IntVar(&maxHeaderValueLen, "max-header-value", 8192, "Max bytes for the Cookie, Origin, or X-Forwarded-For header on /ws; oversized values are rejected before any other work")
	flag.BoolVar(&daemonize, "daemon", false, "")
	flag.Parse()

	if origins != "" {
		allowedOrigins = strings.Split(origins, ",")
	}

	// flag.Visit only calls back for flags actually present on the command
	// line, never for ones left at their zero-value default, so the limiter
	// only turns on if the operator set it.
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "rate-limit-ip", "rate-burst-ip":
			rateLimitIPEnabled = true
		case "rate-limit-player", "rate-burst-player":
			rateLimitPlayerEnabled = true
		}
	})

	// rate.NewLimiter(0, 0) rejects every request. An operator passing 0 to
	// a rate flag almost certainly meant "unlimited", which is what omitting
	// the flag already does. Reject it so they find out at startup.
	if rateLimitIPEnabled && (rateLimitIP <= 0 || rateBurstIP <= 0) {
		return fmt.Errorf("rate limiting for IPs is enabled but -rate-limit-ip=%d and -rate-burst-ip=%d must both be > 0; omit the flags to disable",
			rateLimitIP, rateBurstIP)
	}
	if rateLimitPlayerEnabled && (rateLimitPlayer <= 0 || rateBurstPlayer <= 0) {
		return fmt.Errorf("rate limiting for players is enabled but -rate-limit-player=%d and -rate-burst-player=%d must both be > 0; omit the flags to disable",
			rateLimitPlayer, rateBurstPlayer)
	}
	// Unlike the rate limiters, this one isn't opt-in: -max-header-value <= 0
	// would reject every /ws connection outright, which is never what's
	// meant, so it's a startup error rather than a silent lockout.
	if maxHeaderValueLen <= 0 {
		return fmt.Errorf("-max-header-value=%d must be > 0", maxHeaderValueLen)
	}

	return nil
}

// ///////////////////
// MESSAGE TYPES
// ///////////////////
type WSMessage struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload"`
}

type Message struct {
	PlayerID string      `json:"player_id"`
	Event    string      `json:"event"`
	Payload  interface{} `json:"payload"`
}

type BroadcastMessage struct {
	PlayerID *string     `json:"player_id,omitempty"`
	Event    string      `json:"event"`
	Payload  interface{} `json:"payload"`
}

type pendingMessage struct {
	msg       WSMessage
	timestamp time.Time
}

// initDB initializes the global database connection based on dbDriver and dbDSN.
// Returns an error if the driver is unsupported or if the connection cannot be established.
func initDB() error {
	var err error

	switch dbDriver {
	case "sqlite":
		db, err = sql.Open("sqlite", dbDSN)
		if err == nil {
			db.SetMaxOpenConns(1)
		}
	case "mysql":
		db, err = sql.Open("mysql", dbDSN)
		if err == nil {
			db.SetConnMaxLifetime(5 * time.Minute)
			db.SetMaxOpenConns(25)
			db.SetMaxIdleConns(25)
		}
	default:
		return fmt.Errorf("unsupported db driver: %s", dbDriver)
	}

	if err != nil {
		return err
	}
	return db.Ping()
}

// ErrTokenNotFound means the token row does not exist. Any other error from
// validateToken is a backend failure (DB down, timeout, etc.).
var ErrTokenNotFound = errors.New("token not found")

// validateToken checks whether a given token exists in the database.
// Returns the associated player/subject ID on success, ErrTokenNotFound if no
// matching row exists, or the underlying DB error on backend failure.
func validateToken(token string, isServer bool) (string, error) {
	const q = `
		SELECT IFNULL(player_id, subject_id)
		FROM ws_token
		WHERE token = ?
		  AND is_server = ?
		LIMIT 1
	`
	var sid string
	err := db.QueryRow(q, token, isServer).Scan(&sid)
	if err == nil {
		return sid, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTokenNotFound
	}
	logrus.WithFields(logrus.Fields{
		fieldEvent:  "ws_token_check",
		fieldReason: "db_error",
		fieldToken:  token,
		"err":       err.Error(),
	}).Warn("Token validation failed: database error")
	return "", err
}

// registerConnection registers a websocket connection for a player, storing the associated token.
// It also increments the active connections metric and flushes any pending messages to the new connection.
// Returns the wsConnection so the caller can use its serialized write helpers.
//
// Not ordered relative to live traffic: the connection is inserted into
// players (making it visible to publishHandler/broadcastHandler as "online")
// before flushPendingMessages runs. A /publish call that lands in that
// window sees the player as connected and delivers live immediately,
// which can reach the client before an older message still sitting in
// their queue gets flushed a moment later. wsConnection.mu only serializes
// the writes against each other (no frame corruption), it doesn't order
// them; whichever call reaches wc.mu first is written first. Not fixed
// here because it would mean holding mu for the duration of the flush,
// blocking every other player's publish/broadcast on this one player's
// queue length; if strict ordering is ever needed, it belongs in the
// message payload (sequence number/timestamp), not in this lock.
func registerConnection(playerID string, c *websocket.Conn, token string) *wsConnection {
	wc := &wsConnection{conn: c, token: token}

	mu.Lock()
	if players[playerID] == nil {
		players[playerID] = make(map[*websocket.Conn]*wsConnection)
	}
	players[playerID][c] = wc
	mu.Unlock()

	connections.Inc()

	flushPendingMessages(playerID, wc)

	return wc
}

// unregisterConnection removes a websocket connection for a player, explicitly closes the underlying
// websocket (releasing the file descriptor), and decrements the active connections metric.
// If no connections remain for the player, the player's entry is removed from the players map.
func unregisterConnection(playerID string, c *websocket.Conn) {
	mu.Lock()
	defer mu.Unlock()
	delete(players[playerID], c)
	if len(players[playerID]) == 0 {
		delete(players, playerID)
	}

	_ = c.Close()
	connections.Dec()
}

// closeAllConnections closes all active websocket connections for all players.
// Typically used during server shutdown.
func closeAllConnections() {
	mu.Lock()
	defer mu.Unlock()
	for _, conns := range players {
		for _, wc := range conns {
			wc.conn.Close()
		}
	}
}

// flushPendingMessages sends any queued offline messages to a newly connected websocket.
// Messages older than offlineTTL are ignored and removed.
func flushPendingMessages(playerID string, wc *wsConnection) {
	pendingMu.Lock()
	msgs := pendingMessages[playerID]
	if len(msgs) > 0 {
		for _, pm := range msgs {
			if time.Since(pm.timestamp) <= offlineTTL {
				_ = wc.writeJSON(pm.msg)
				messagesDelivered.Inc()
				logrus.WithFields(logrus.Fields{
					fieldEvent:    eventWSDeliver,
					fieldReason:   "offline_flush",
					fieldPlayerID: playerID,
					fieldAppEvent: pm.msg.Event,
					"queued_at":   pm.timestamp,
					"age_ms":      time.Since(pm.timestamp).Milliseconds(),
				}).Info("Delivered queued WS message")
			}
		}
		delete(pendingMessages, playerID)
	}
	pendingMu.Unlock()
}

// wsHandler handles incoming websocket upgrade requests from clients.
// Validates the token, enforces connection limits, sets up heartbeat, and reads messages.
// Connections are automatically unregistered on disconnect.
//
// The token is validated *before* the upgrade so that a rejected attempt can be
// tagged on the 101 response via X-WS-Reject, which nginx maps to a real status
// code in the access log. The upgrade still completes and the client receives a
// normal close frame, so browser-side code can still branch on the close code.
//
// The work is split across preUpgradeCheck (IP/player rate limits + token
// validation), sendPostUpgradeReject (the token-related rejections that must
// complete the upgrade first), checkConnectionLimit, and serveConnection (the
// heartbeat + read loop once a connection is accepted), so this function is
// just the sequence, not the logic.
func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	ip := clientIP(r)

	playerID, rejectReason, ok := preUpgradeCheck(w, r, ip, token)
	if !ok {
		// A real HTTP-level rejection (429) was already written.
		return
	}

	// Response headers for the 101. X-WS-Reject is only set when the
	// connection will be closed immediately after the upgrade; nginx maps
	// it to a real status code in the access log. X-Player-Uid is set
	// whenever a token resolved to a real player, rejected-after-upgrade
	// or not, so nginx's access log can show who without needing the app
	// log. player_id only ever contains the ws_token.player_id column
	// (INT UNSIGNED), so it's always a plain digit string, no escaping to
	// worry about.
	respHeader := http.Header{}
	if rejectReason != "" {
		respHeader.Set("X-WS-Reject", rejectReason)
	} else {
		respHeader.Set("X-Player-Uid", playerID)
	}

	conn, err := upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		logReject(r, ip, "upgrade_failed", logrus.Fields{fieldToken: token, "err": err.Error()},
			"Connection rejected: upgrade failed")
		return
	}

	// If validation failed, log the rejection and close the upgraded socket.
	// Client sees a normal close frame, not an HTTP-level error.
	if rejectReason != "" {
		sendPostUpgradeReject(conn, r, ip, token, rejectReason)
		return
	}

	if !checkConnectionLimit(conn, r, ip, token, playerID) {
		return
	}

	wc := registerConnection(playerID, conn, token)
	defer unregisterConnection(playerID, conn)

	logrus.WithFields(logrus.Fields{
		fieldEvent:    "ws_connect",
		fieldReason:   "accepted",
		fieldPlayerID: playerID,
		fieldToken:    token,
		"ip":          ip,
		fieldOrigin:   r.Header.Get("Origin"),
		fieldXFF:      r.Header.Get("X-Forwarded-For"),
		fieldCookie:   r.Header.Get("Cookie"),
	}).Info("Player connected")

	serveConnection(wc, conn, playerID, token, ip, r)
}

// logReject logs a ws_reject line with the fields every rejection carries
// (ip/origin/xff/cookie) merged with reason-specific fields. extra may be
// nil. Not used for header_too_large: that reason exists specifically
// because one of those header values is oversized, so logging it raw here
// would defeat the point. See rejectOversizedHeader.
func logReject(r *http.Request, ip, reason string, extra logrus.Fields, msg string) {
	fields := logrus.Fields{
		fieldEvent:  eventWSReject,
		fieldReason: reason,
		"ip":        ip,
		fieldOrigin: r.Header.Get("Origin"),
		fieldXFF:    r.Header.Get("X-Forwarded-For"),
		fieldCookie: r.Header.Get("Cookie"),
	}
	for k, v := range extra {
		fields[k] = v
	}
	logrus.WithFields(fields).Warn(msg)
}

// oversizedHeader returns the name of the first of Cookie, Origin, or
// X-Forwarded-For whose value exceeds maxHeaderValueLen bytes, or "" if all
// three are within bounds.
//
// This is a per-value cap, separate from and much tighter than
// http.Server's own MaxHeaderBytes (which only bounds the sum of every
// header on the request combined, see buildServer). It exists because
// these three specific values get logged raw on every ws_reject/ws_connect/
// ws_disconnect line: without this check, a single client could force
// every log line for its connection to carry a multi-hundred-KB value.
func oversizedHeader(r *http.Request) string {
	for _, name := range []string{"Cookie", "Origin", "X-Forwarded-For"} {
		if len(r.Header.Get(name)) > maxHeaderValueLen {
			return name
		}
	}
	return ""
}

// rejectOversizedHeader logs and responds to a request that failed
// oversizedHeader's check. It logs the offending header's name and length
// only, never its value, since the value being oversized is the whole
// reason this exists.
func rejectOversizedHeader(w http.ResponseWriter, r *http.Request, ip, header string) {
	logrus.WithFields(logrus.Fields{
		fieldEvent:  eventWSReject,
		fieldReason: "header_too_large",
		"ip":        ip,
		"header":    header,
		"len":       len(r.Header.Get(header)),
		"max":       maxHeaderValueLen,
	}).Warn("Connection rejected: header too large")
	http.Error(w, "header too large", http.StatusRequestHeaderFieldsTooLarge)
}

// preUpgradeCheck runs every check that must happen before the HTTP response
// is committed: header size limits, the IP rate limit, token validation, and
// the player rate limit (checked only once the token is known valid).
//
// ok=false means a real HTTP-level rejection (429 or 431) has already been
// written and the caller must not call Upgrade(). ok=true with rejectReason
// set means the caller should still complete the upgrade and reject via a
// close frame instead (see wsHandler's doc comment for why). ok=true with
// rejectReason empty means proceed normally.
func preUpgradeCheck(w http.ResponseWriter, r *http.Request, ip, token string) (playerID, rejectReason string, ok bool) {
	// Checked first and before anything else, including the rate limiter:
	// it's the cheapest possible rejection, and every other rejection path
	// below logs Cookie/Origin/X-Forwarded-For raw, which is exactly what
	// this is guarding against.
	if h := oversizedHeader(r); h != "" {
		rejectOversizedHeader(w, r, ip, h)
		return "", "", false
	}

	if !allowIP(ip) {
		logReject(r, ip, "rate_limit_ip", nil, "Connection rejected: IP rate limit")
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return "", "", false
	}

	if token == "" {
		return "", "missing_token", true
	}

	var err error
	playerID, err = validateToken(token, false)
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			return "", "invalid_token", true
		}
		return "", "token_check_failed", true
	}

	if !allowPlayer(playerID) {
		logReject(r, ip, "rate_limit_player", logrus.Fields{fieldPlayerID: playerID, fieldToken: token},
			"Connection rejected: player rate limit")
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return "", "", false
	}

	return playerID, "", true
}

// postUpgradeReject describes how to close a connection for one of the
// rejectReason values that can only be signalled after the upgrade
// completes. tokenInLog controls whether the token is included in the log
// line, matching LOGGING.md (absent for missing_token, present otherwise).
type postUpgradeReject struct {
	closeCode  int
	closeMsg   string
	logMsg     string
	tokenInLog bool
}

var postUpgradeRejects = map[string]postUpgradeReject{
	"missing_token": {
		closeCode: websocket.ClosePolicyViolation,
		closeMsg:  "missing token",
		logMsg:    "Connection rejected: missing token",
	},
	"invalid_token": {
		closeCode:  websocket.ClosePolicyViolation,
		closeMsg:   "invalid token",
		logMsg:     "Connection rejected: invalid token",
		tokenInLog: true,
	},
	"token_check_failed": {
		closeCode:  websocket.CloseTryAgainLater,
		closeMsg:   "token check failed",
		logMsg:     "Connection rejected: token check failed",
		tokenInLog: true,
	},
}

// sendPostUpgradeReject logs and closes an already-upgraded connection for
// one of the postUpgradeRejects reasons.
func sendPostUpgradeReject(conn *websocket.Conn, r *http.Request, ip, token, reason string) {
	spec := postUpgradeRejects[reason]

	var extra logrus.Fields
	if spec.tokenInLog {
		extra = logrus.Fields{fieldToken: token}
	}
	logReject(r, ip, reason, extra, spec.logMsg)

	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(spec.closeCode, spec.closeMsg))
	_ = conn.Close()
}

// checkConnectionLimit reports whether playerID is under maxConnectionsPerPlayer.
// On the limit being hit, it logs the rejection and closes conn itself.
func checkConnectionLimit(conn *websocket.Conn, r *http.Request, ip, token, playerID string) bool {
	mu.Lock()
	current := len(players[playerID])
	mu.Unlock()

	if current < maxConnectionsPerPlayer {
		return true
	}

	logReject(r, ip, "too_many_connections", logrus.Fields{
		fieldPlayerID: playerID,
		fieldToken:    token,
		"current":     current,
		"limit":       maxConnectionsPerPlayer,
	}, "Connection rejected: too many connections")
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many connections"))
	_ = conn.Close()
	return false
}

// serveConnection runs an accepted, registered connection until it closes:
// heartbeat ping/pong, and a read loop that rejects any client-sent data
// frame (players are not allowed to send us application data).
func serveConnection(wc *wsConnection, conn *websocket.Conn, playerID, token, ip string, r *http.Request) {
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetReadLimit(256)
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// WriteControl is safe to call concurrently with other writes.
				_ = conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
			}
		}
	}()

	// A single call is all this ever does: ReadMessage blocks until either
	// an error (client closed, network error, or the read deadline expiring
	// when a pong doesn't arrive in time) or a genuine data frame, and a
	// data frame is itself grounds to close, so there's never a need for a
	// second read. Written straight-line rather than as a for-loop that
	// always exits on its first pass (staticcheck SA4004).
	mt, _, err := conn.ReadMessage()
	if err == nil {
		// Clients are not allowed to send data; only control frames are expected.
		_ = wc.writeClose(websocket.ClosePolicyViolation, "client messages not accepted")
		logReject(r, ip, "client_data_frame", logrus.Fields{
			fieldPlayerID: playerID,
			fieldToken:    token,
			"type":        mt,
		}, "Client sent unexpected message, closing")
	}

	logrus.WithFields(logrus.Fields{
		fieldPlayerID: playerID,
		fieldToken:    token,
		fieldEvent:    "ws_disconnect",
		"ip":          ip,
		fieldOrigin:   r.Header.Get("Origin"),
		fieldXFF:      r.Header.Get("X-Forwarded-For"),
		fieldCookie:   r.Header.Get("Cookie"),
	}).Info("Player disconnected")
}

// publishHandler handles incoming messages from authorized servers to a specific player.
// Validates server token, delivers message immediately if the player is connected,
// or queues the message for offline delivery if not.
func publishHandler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	token := auth[7:]

	subjectID, err := validateToken(token, true)
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			http.Error(w, "invalid server token", http.StatusForbidden)
		} else {
			http.Error(w, "token check failed", http.StatusServiceUnavailable)
		}
		return
	}

	var msg Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	wsMsg := WSMessage{Event: msg.Event, Payload: msg.Payload}

	mu.Lock()
	conns := players[msg.PlayerID]
	numConns := len(conns)

	if numConns == 0 {
		logrus.WithFields(logrus.Fields{
			fieldEvent:     "ws_queue",
			fieldReason:    "queued",
			fieldSubjectID: subjectID,
			fieldPlayerID:  msg.PlayerID,
			fieldAppEvent:  msg.Event,
		}).Info("Player not connected, queueing message")

		pendingMu.Lock()
		queue := pendingMessages[msg.PlayerID]
		if len(queue) >= maxQueuedMessagesPerPlayer {
			queue = queue[1:]
			logrus.WithFields(logrus.Fields{
				fieldEvent:    "ws_queue",
				fieldReason:   "queue_overflow",
				fieldPlayerID: msg.PlayerID,
				fieldAppEvent: msg.Event,
				"limit":       maxQueuedMessagesPerPlayer,
			}).Warn("Offline queue full, dropping oldest message")
		}
		pendingMessages[msg.PlayerID] = append(queue, pendingMessage{msg: wsMsg, timestamp: time.Now()})
		pendingMu.Unlock()
	} else {
		for _, wc := range conns {
			_ = wc.writeJSON(wsMsg)
			messagesDelivered.Inc()
		}
		logrus.WithFields(logrus.Fields{
			fieldEvent:     eventWSDeliver,
			fieldReason:    "live",
			fieldSubjectID: subjectID,
			fieldPlayerID:  msg.PlayerID,
			fieldAppEvent:  msg.Event,
			"connections":  numConns,
		}).Info("Message delivered to player")
	}
	mu.Unlock()

	messagesPublished.Inc()
	w.WriteHeader(http.StatusOK)
}

// broadcastHandler handles incoming broadcast messages from authorized servers.
// Can target a specific player or all connected players.
// Increments metrics for delivered messages.
func broadcastHandler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	token := auth[7:]

	subjectID, err := validateToken(token, true)
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			http.Error(w, "invalid server token", http.StatusForbidden)
		} else {
			http.Error(w, "token check failed", http.StatusServiceUnavailable)
		}
		return
	}

	var req BroadcastMessage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	wsMsg := WSMessage{Event: req.Event, Payload: req.Payload}

	mu.Lock()
	defer mu.Unlock()

	delivered := 0
	if req.PlayerID != nil {
		for _, wc := range players[*req.PlayerID] {
			_ = wc.writeJSON(wsMsg)
			messagesDelivered.Inc()
			delivered++
		}
		logrus.WithFields(logrus.Fields{
			fieldEvent:     eventWSDeliver,
			fieldReason:    "broadcast_player",
			fieldSubjectID: subjectID,
			fieldPlayerID:  *req.PlayerID,
			fieldAppEvent:  req.Event,
			"delivered":    delivered,
		}).Info("Broadcast delivered to player")
	} else {
		for _, conns := range players {
			for _, wc := range conns {
				_ = wc.writeJSON(wsMsg)
				messagesDelivered.Inc()
				delivered++
			}
		}
		logrus.WithFields(logrus.Fields{
			fieldEvent:     eventWSDeliver,
			fieldReason:    "broadcast_all",
			fieldSubjectID: subjectID,
			fieldAppEvent:  req.Event,
			"delivered":    delivered,
		}).Info("Broadcast delivered to all players")
	}

	messagesPublished.Inc()
	w.WriteHeader(http.StatusOK)
}

// initMetrics registers Prometheus metrics for connections, messages published, and messages delivered.
func initMetrics() {
	prometheus.MustRegister(connections, messagesPublished, messagesDelivered)
}

// startOfflineMessageCleanup periodically removes expired pending messages from the offline queue.
// It stops when stopCh is closed.
func startOfflineMessageCleanup(stopCh <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				now := time.Now()
				pendingMu.Lock()
				for pid, msgs := range pendingMessages {
					filtered := msgs[:0]
					for _, pm := range msgs {
						if now.Sub(pm.timestamp) <= offlineTTL {
							filtered = append(filtered, pm)
						}
					}
					if len(filtered) == 0 {
						delete(pendingMessages, pid)
					} else {
						pendingMessages[pid] = filtered
					}
				}
				pendingMu.Unlock()
			}
		}
	}()
}

// buildServer constructs and returns the HTTP server and its ServeMux with all routes registered.
func buildServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/publish", publishHandler)
	mux.HandleFunc("/broadcast", broadcastHandler)
	mux.Handle("/metrics", promhttp.Handler())

	return &http.Server{
		Addr:              serverAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Go defaults this to 1MB (DefaultMaxHeaderBytes) for every header
		// on the request combined, which is far more than anything here
		// legitimately needs. This bounds it before the request even
		// reaches a handler; oversizedHeader (in preUpgradeCheck) is the
		// separate, tighter, per-value check on Cookie/Origin/XFF that this
		// doesn't replace.
		MaxHeaderBytes: 16 << 10, // 16KB
	}
}

// runServer starts the HTTP server and blocks until it shuts down.
// It listens for SIGINT/SIGTERM, closes stopCh to signal background goroutines,
// then performs a graceful HTTP shutdown followed by closing all websocket connections.
// Returns an error if the server exits unexpectedly.
func runServer(server *http.Server, stopCh chan struct{}) error {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		logrus.Info("Shutting down server...")
		// Signal all background goroutines (cleanup, revalidation) to stop.
		close(stopCh)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		closeAllConnections()
	}()

	logrus.Infof("Server listening on %s", serverAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// startTokenRevalidation periodically validates all active websocket tokens.
// Invalid tokens cause connections to be closed and removed.
// The provided stopCh can be closed to stop the revalidation loop and its ticker.
func startTokenRevalidation(interval time.Duration, stopCh <-chan struct{}) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				revalidateOnce()
			}
		}
	}()
}

// revalidateOnce runs a single token-revalidation sweep in three phases:
//
//  1. Snapshot the live connections under a brief lock.
//  2. Query the database for each token with no lock held, so publishes,
//     connects, disconnects, and broadcasts are never blocked by DB I/O.
//  3. Re-acquire the lock briefly and close only the connections that are
//     still exactly the ones we snapshotted - a connection that disconnected
//     or was replaced mid-sweep is left alone.
func revalidateOnce() {
	// Phase 1: snapshot.
	mu.Lock()
	entries := make([]revalEntry, 0, len(players))
	for playerID, conns := range players {
		for c, wc := range conns {
			entries = append(entries, revalEntry{
				playerID: playerID,
				conn:     c,
				token:    wc.token,
			})
		}
	}
	mu.Unlock()

	checked := len(entries)
	if checked == 0 {
		return
	}

	start := time.Now()

	// Phase 2: query outside the lock.
	var invalid []revalEntry
	backendErrors := 0
	for _, e := range entries {
		_, err := validateToken(e.token, false)
		switch {
		case err == nil:
			// Still valid, leave the connection alone.
		case errors.Is(err, ErrTokenNotFound):
			invalid = append(invalid, e)
		default:
			// Backend error - the token may still be valid. Skip and retry
			// next tick so a DB blip doesn't mass-disconnect.
			// validateToken already logged the underlying error.
			backendErrors++
		}
	}

	// Phase 3: reconcile under a brief lock.
	closed := 0
	mu.Lock()
	for _, e := range invalid {
		conns, ok := players[e.playerID]
		if !ok {
			// Player has no connections any more.
			continue
		}
		wc, ok := conns[e.conn]
		if !ok || wc.token != e.token {
			// This exact connection is gone or was replaced during the sweep.
			continue
		}
		logrus.WithFields(logrus.Fields{
			fieldEvent:    eventWSReject,
			fieldReason:   "token_not_found",
			fieldPlayerID: e.playerID,
			fieldToken:    e.token,
			"source":      "revalidation",
		}).Info("Closing connection: token not found")
		_ = wc.writeClose(websocket.ClosePolicyViolation, "token not found")
		_ = wc.conn.Close()
		delete(conns, e.conn)
		if len(conns) == 0 {
			delete(players, e.playerID)
		}
		closed++
	}
	mu.Unlock()

	// Sweep summary. Only emitted at Warn when something needed attention;
	// otherwise Debug, so a healthy server doesn't log once per tick.
	elapsed := time.Since(start).Milliseconds()
	if backendErrors > 0 {
		logrus.WithFields(logrus.Fields{
			fieldEvent:       "ws_revalidate",
			fieldReason:      "sweep_backend_errors",
			"checked":        checked,
			"closed":         closed,
			"backend_errors": backendErrors,
			"duration_ms":    elapsed,
		}).Warn("Token revalidation sweep completed with DB errors")
	} else {
		logrus.WithFields(logrus.Fields{
			fieldEvent:    "ws_revalidate",
			fieldReason:   "sweep_completed",
			"checked":     checked,
			"closed":      closed,
			"duration_ms": elapsed,
		}).Debug("Token revalidation sweep completed")
	}
}

// writePIDFile writes the current process PID to the specified file path.
// Returns an error if writing fails.
func writePIDFile(path string) error {
	pid := os.Getpid()
	data := []byte(fmt.Sprintf("%d\n", pid))
	return os.WriteFile(path, data, 0644)
}

// removePIDFile deletes the PID file at the specified path.
// Any errors are ignored.
func removePIDFile(path string) {
	_ = os.Remove(path)
}

// pidFileExists checks if the PID file exists and its PID is still running.
// Returns the PID and true only if the file exists, contains a valid integer,
// and that process is alive. A stale PID file returns 0, false.
func pidFileExists(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is gone; treat the PID file as stale.
		return 0, false
	}
	return pid, true
}

// daemonizeSelf re-launches the current executable as a background daemon process.
// It returns an error if the executable cannot be determined or if the child process fails to start.
// If successful, the parent process will exit immediately using os.Exit(0) to allow the daemon to continue independently.
func daemonizeSelf() error {
	if os.Getenv("DAEMONIZED") == "1" {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot get executable path: %w", err)
	}

	args := []string{}
	for _, a := range os.Args[1:] {
		if a != "-daemon" {
			args = append(args, a)
		}
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "DAEMONIZED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to daemonize: %w", err)
	}

	os.Exit(0) // safe here, because no defers in daemon parent context
	return nil // unreachable, but satisfies compiler
}

// setupLogging configures logrus logging for the application.
// It sets the output destination and log level based on global flags.
// Returns an error if the log file cannot be opened or if the log level is invalid.
// The opened log file handle is stored in logFileHandle so it can be closed on shutdown.
func setupLogging() error {
	switch strings.ToLower(logFormat) {
	case "json":
		logrus.SetFormatter(&logrus.JSONFormatter{})
	case "text":
		logrus.SetFormatter(&logrus.TextFormatter{
			FullTimestamp: true,
			DisableColors: logFile != "",
		})
	default:
		return fmt.Errorf("invalid log format: %s (want json or text)", logFormat)
	}

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file %s: %w", logFile, err)
		}

		logFileHandle = f
		logrus.SetOutput(f)
	} else {
		logrus.SetOutput(os.Stdout)
	}

	level, err := logrus.ParseLevel(strings.ToLower(logLevel))
	if err != nil {
		return fmt.Errorf("invalid log level: %s", logLevel)
	}
	logrus.SetLevel(level)

	return nil
}

// handlePIDFile ensures that the PID file is created and removed properly.
// If the PID file already exists, it returns an error.
// The PID file is automatically removed when the function that called this defers cleanup.
// Returns an error if writing the PID file fails.
func handlePIDFile() error {
	if pidFile == "" {
		return nil
	}

	if pid, ok := pidFileExists(pidFile); ok {
		return fmt.Errorf("pid file already exists for PID: %d", pid)
	}

	if err := writePIDFile(pidFile); err != nil {
		return fmt.Errorf("failed to write pid file: %w", err)
	}

	// The caller should defer removePIDFile(pidFile) to ensure cleanup
	return nil
}

// ///////////////////
// MAIN
// ///////////////////
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := parseFlags(); err != nil {
		return err
	}

	// Daemonize first: the parent exits before touching the PID file,
	// log file, or DB, so the child owns all of those resources.
	if daemonize {
		if err := daemonizeSelf(); err != nil {
			return fmt.Errorf("failed to daemonize: %w", err)
		}
	}

	// Handle PID file
	if err := handlePIDFile(); err != nil {
		return err
	}
	if pidFile != "" {
		defer removePIDFile(pidFile)
	}

	// Setup logging
	if err := setupLogging(); err != nil {
		return fmt.Errorf("failed to setup logging: %w", err)
	}

	if logFileHandle != nil {
		defer logFileHandle.Close()
	}

	// Initialize DB
	if err := initDB(); err != nil {
		return fmt.Errorf("failed to init DB: %w", err)
	}

	defer db.Close()

	initMetrics()

	stopCh := make(chan struct{})
	startOfflineMessageCleanup(stopCh)
	startRateLimiterCleanup(stopCh)
	startTokenRevalidation(tokenRevalidationPeriod, stopCh)

	return runServer(buildServer(), stopCh)
}
