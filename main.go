package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	rateLimit                  int
	ratePeriod                 time.Duration
	tokenRevalidationPeriod    time.Duration
	logFile                    string
	logLevel                   string
	daemonize                  bool
	pidFile                    string
	logFormat                  string
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

	// Rate limiting
	limiters = make(map[string]*limiter)
	lm       sync.Mutex

	// logFileHandle holds the open log file so it can be closed on shutdown.
	logFileHandle *os.File

	// ErrTokenNotFound means the token row does not exist. Any other error from
	// validateToken is a backend failure (DB down, timeout, etc.).
	ErrTokenNotFound = errors.New("token not found")
)

type limiter struct {
	tokens int
	last   time.Time
}

// parseFlags parses command-line flags into global configuration variables.
// It supports database driver, DSN, server address, log file/level, PID file, and various WS server limits.
func parseFlags() {
	var origins string

	flag.StringVar(&dbDriver, "db", "sqlite", "Database driver")
	flag.StringVar(&dbDSN, "dsn", "file:ws_tokens.db?cache=shared", "Database DSN")
	flag.StringVar(&serverAddr, "addr", ":8080", "Server address")
	flag.StringVar(&origins, "origins", "", "Allowed WS origins")
	flag.StringVar(&logFile, "log-file", "", "Path to log file")
	flag.StringVar(&logLevel, "log-level", "info", "Log level")
	flag.StringVar(&logFormat, "log-format", "json", "Log format: json or text")
	flag.StringVar(&pidFile, "pid-file", "", "Path to PID file")
	flag.IntVar(&maxQueuedMessagesPerPlayer, "max-queued", 100, "")
	flag.IntVar(&rateLimit, "rate-limit", 200, "")
	flag.DurationVar(&ratePeriod, "rate-period", time.Second, "")
	flag.IntVar(&maxConnectionsPerPlayer, "max-conns", 10, "")
	flag.DurationVar(&tokenRevalidationPeriod, "revalidate-period", time.Minute, "")
	flag.DurationVar(&offlineTTL, "offline-ttl", 10*time.Second, "")
	flag.BoolVar(&daemonize, "daemon", false, "")
	flag.Parse()

	if origins != "" {
		allowedOrigins = strings.Split(origins, ",")
	}
}

// allow implements a simple token-based rate limiter for a given key.
// Returns true if the action is allowed, false if the rate limit has been exceeded.
func allow(key string, rate int, per time.Duration) bool {
	lm.Lock()
	defer lm.Unlock()

	l, ok := limiters[key]
	if !ok {
		limiters[key] = &limiter{tokens: rate, last: time.Now()}
		return true
	}

	elapsed := time.Since(l.last)
	refill := int(elapsed / per)
	if refill > 0 {
		l.tokens += refill
		if l.tokens > rate {
			l.tokens = rate
		}
		l.last = time.Now()
	}

	if l.tokens > 0 {
		l.tokens--
		return true
	}
	return false
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
		"event":  "ws_token_check",
		"reason": "db_error",
		"token":  token,
		"err":    err.Error(),
	}).Warn("Token validation failed: database error")
	return "", err
}

// registerConnection registers a websocket connection for a player, storing the associated token.
// It also increments the active connections metric and flushes any pending messages to the new connection.
// Returns the wsConnection so the caller can use its serialized write helpers.
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
					"event":     "ws_deliver",
					"reason":    "offline_flush",
					"player_id": playerID,
					"app_event": pm.msg.Event,
					"queued_at": pm.timestamp,
					"age_ms":    time.Since(pm.timestamp).Milliseconds(),
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
func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	// Validate before upgrade, but do not short-circuit: a rejection is
	// signalled via X-WS-Reject on the 101 response plus a close frame.
	var rejectReason string
	var playerID string

	if token == "" {
		rejectReason = "missing_token"
	} else {
		var err error
		playerID, err = validateToken(token, false)
		if err != nil {
			if errors.Is(err, ErrTokenNotFound) {
				rejectReason = "invalid_token"
			} else {
				rejectReason = "token_check_failed"
			}
		}
	}

	// Response headers for the 101. X-WS-Reject is only set when the
	// connection will be closed immediately after the upgrade; nginx maps
	// it to a real status code in the access log.
	respHeader := http.Header{}
	if rejectReason != "" {
		respHeader.Set("X-WS-Reject", rejectReason)
	}

	conn, err := upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"event":  "ws_reject",
			"reason": "upgrade_failed",
			"token":  token,
			"err":    err.Error(),
			"ip":     r.RemoteAddr,
			"origin": r.Header.Get("Origin"),
			"xff":    r.Header.Get("X-Forwarded-For"),
		}).Warn("Connection rejected: upgrade failed")
		return
	}

	// If validation failed, log the rejection and close the upgraded socket.
	// Client sees a normal close frame, not an HTTP-level error.
	if rejectReason != "" {
		switch rejectReason {
		case "missing_token":
			logrus.WithFields(logrus.Fields{
				"event":  "ws_reject",
				"reason": "missing_token",
				"ip":     r.RemoteAddr,
				"origin": r.Header.Get("Origin"),
				"xff":    r.Header.Get("X-Forwarded-For"),
			}).Warn("Connection rejected: missing token")
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "missing token"))

		case "invalid_token":
			logrus.WithFields(logrus.Fields{
				"event":  "ws_reject",
				"reason": "invalid_token",
				"token":  token,
				"ip":     r.RemoteAddr,
				"origin": r.Header.Get("Origin"),
				"xff":    r.Header.Get("X-Forwarded-For"),
			}).Warn("Connection rejected: invalid token")
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid token"))

		case "token_check_failed":
			logrus.WithFields(logrus.Fields{
				"event":  "ws_reject",
				"reason": "token_check_failed",
				"token":  token,
				"ip":     r.RemoteAddr,
				"origin": r.Header.Get("Origin"),
				"xff":    r.Header.Get("X-Forwarded-For"),
			}).Warn("Connection rejected: token check failed")
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "token check failed"))
		}

		_ = conn.Close()
		return
	}

	// check connection limit
	mu.Lock()
	current := len(players[playerID])
	if current >= maxConnectionsPerPlayer {
		mu.Unlock()
		logrus.WithFields(logrus.Fields{
			"event":     "ws_reject",
			"reason":    "too_many_connections",
			"player_id": playerID,
			"token":     token,
			"current":   current,
			"limit":     maxConnectionsPerPlayer,
			"ip":        r.RemoteAddr,
			"origin":    r.Header.Get("Origin"),
			"xff":       r.Header.Get("X-Forwarded-For"),
		}).Warn("Connection rejected: too many connections")
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many connections"))
		_ = conn.Close()
		return
	}
	mu.Unlock()

	// register connection with token
	wc := registerConnection(playerID, conn, token)
	defer unregisterConnection(playerID, conn)

	logrus.WithFields(logrus.Fields{
		"event":     "ws_connect",
		"reason":    "accepted",
		"player_id": playerID,
		"token":     token,
		"ip":        r.RemoteAddr,
		"origin":    r.Header.Get("Origin"),
		"xff":       r.Header.Get("X-Forwarded-For"),
	}).Info("Player connected")

	// heartbeat
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

	// main read loop
	for {
		mt, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
		// Clients are not allowed to send data; only control frames are expected.
		_ = wc.writeClose(websocket.ClosePolicyViolation, "client messages not accepted")
		logrus.WithFields(logrus.Fields{
			"event":     "ws_reject",
			"reason":    "client_data_frame",
			"player_id": playerID,
			"token":     token,
			"type":      mt,
			"ip":        r.RemoteAddr,
			"origin":    r.Header.Get("Origin"),
			"xff":       r.Header.Get("X-Forwarded-For"),
		}).Warn("Client sent unexpected message, closing")
		break
	}

	logrus.WithFields(logrus.Fields{
		"player_id": playerID,
		"token":     token,
		"event":     "ws_disconnect",
		"ip":        r.RemoteAddr,
		"origin":    r.Header.Get("Origin"),
		"xff":       r.Header.Get("X-Forwarded-For"),
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
			"event":      "ws_queue",
			"reason":     "queued",
			"subject_id": subjectID,
			"player_id":  msg.PlayerID,
			"app_event":  msg.Event,
		}).Info("Player not connected, queueing message")

		pendingMu.Lock()
		queue := pendingMessages[msg.PlayerID]
		if len(queue) >= maxQueuedMessagesPerPlayer {
			queue = queue[1:]
			logrus.WithFields(logrus.Fields{
				"event":     "ws_queue",
				"reason":    "queue_overflow",
				"player_id": msg.PlayerID,
				"app_event": msg.Event,
				"limit":     maxQueuedMessagesPerPlayer,
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
			"event":       "ws_deliver",
			"reason":      "live",
			"subject_id":  subjectID,
			"player_id":   msg.PlayerID,
			"app_event":   msg.Event,
			"connections": numConns,
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
			"event":      "ws_deliver",
			"reason":     "broadcast_player",
			"subject_id": subjectID,
			"player_id":  *req.PlayerID,
			"app_event":  req.Event,
			"delivered":  delivered,
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
			"event":      "ws_deliver",
			"reason":     "broadcast_all",
			"subject_id": subjectID,
			"app_event":  req.Event,
			"delivered":  delivered,
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
			"event":     "ws_reject",
			"reason":    "token_not_found",
			"player_id": e.playerID,
			"token":     e.token,
			"source":    "revalidation",
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
			"event":          "ws_revalidate",
			"reason":         "sweep_backend_errors",
			"checked":        checked,
			"closed":         closed,
			"backend_errors": backendErrors,
			"duration_ms":    elapsed,
		}).Warn("Token revalidation sweep completed with DB errors")
	} else {
		logrus.WithFields(logrus.Fields{
			"event":       "ws_revalidate",
			"reason":      "sweep_completed",
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
	parseFlags()

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
	startTokenRevalidation(tokenRevalidationPeriod, stopCh)

	return runServer(buildServer(), stopCh)
}
