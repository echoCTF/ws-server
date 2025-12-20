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

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	logrus "github.com/sirupsen/logrus"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// ///////////////////
// GLOBAL CONFIG
// ///////////////////
var (
	dbDSN          string
	dbDriver       string
	serverAddr     string
	allowedOrigins []string

	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second

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
)

type wsConnection struct {
	conn  *websocket.Conn
	token string
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
	flag.StringVar(&pidFile, "pid-file", "", "Path to PID file")
	flag.IntVar(&maxQueuedMessagesPerPlayer, "max-queued", 100, "")
	flag.IntVar(&rateLimit, "rate-limit", 10, "")
	flag.DurationVar(&ratePeriod, "rate-period", time.Second, "")
	flag.IntVar(&maxConnectionsPerPlayer, "max-conns", 5, "")
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

// validateToken checks whether a given token is valid in the database.
// Returns the associated player/subject ID and true if valid, or empty string and false if invalid.
func validateToken(token string, isServer bool) (string, bool) {
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
		return sid, true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	logrus.WithError(err).Error("validateToken error")
	return "", false
}

// registerConnection registers a websocket connection for a player, storing the associated token.
// It also increments the active connections metric and flushes any pending messages to the new connection.
func registerConnection(playerID string, c *websocket.Conn, token string) {
	mu.Lock()
	if players[playerID] == nil {
		players[playerID] = make(map[*websocket.Conn]*wsConnection)
	}
	players[playerID][c] = &wsConnection{conn: c, token: token}
	mu.Unlock()

	connections.Inc()

	flushPendingMessages(playerID, c)
}

// unregisterConnection removes a websocket connection for a player and decrements the active connections metric.
// If no connections remain for the player, the player's entry is removed from the players map.
func unregisterConnection(playerID string, c *websocket.Conn) {
	mu.Lock()
	defer mu.Unlock()
	delete(players[playerID], c)
	if len(players[playerID]) == 0 {
		delete(players, playerID)
	}
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
func flushPendingMessages(playerID string, c *websocket.Conn) {
	pendingMu.Lock()
	msgs := pendingMessages[playerID]
	if len(msgs) > 0 {
		for _, pm := range msgs {
			if time.Since(pm.timestamp) <= offlineTTL {
				_ = c.WriteJSON(pm.msg)
				messagesDelivered.Inc()
				logrus.WithFields(logrus.Fields{
					"player_id": playerID,
					"event":     pm.msg.Event,
					"queued_at": pm.timestamp,
					"age_ms":    time.Since(pm.timestamp).Milliseconds(),
					"source":    "offline_queue",
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
func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	playerID, ok := validateToken(token, false)
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// check connection limit
	mu.Lock()
	current := len(players[playerID])
	mu.Unlock()
	if current >= maxConnectionsPerPlayer {
		logrus.WithFields(logrus.Fields{
			"player_id": playerID,
			"current":   current,
			"limit":     maxConnectionsPerPlayer,
		}).Warn("Connection rejected: too many connections")
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}

	// upgrade to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// register connection with token
	registerConnection(playerID, conn, token)
	defer unregisterConnection(playerID, conn)

	logrus.WithFields(logrus.Fields{
		"player_id": playerID,
		"event":     "ws_connect",
		"ip":        r.RemoteAddr,
	}).Info("Player connected")

	// heartbeat
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			_ = conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
		}
	}()

	// main read loop
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	logrus.WithFields(logrus.Fields{
		"player_id": playerID,
		"event":     "ws_disconnect",
	}).Info("Player disconnected")
}

// publishHandler handles incoming messages from authorized servers to a specific player.
// Validates server token, enforces rate limits, delivers message immediately if the player is connected,
// or queues the message for offline delivery if not.
func publishHandler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	token := auth[7:]

	subjectID, ok := validateToken(token, true)
	if !ok {
		http.Error(w, "invalid server token", http.StatusForbidden)
		return
	}

	if !allow(subjectID, rateLimit, ratePeriod) {
		http.Error(w, "rate limit", http.StatusTooManyRequests)
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
			"subject_id": subjectID,
			"player_id":  msg.PlayerID,
			"event":      msg.Event,
		}).Warn("Player not connected, queueing message")

		pendingMu.Lock()
		queue := pendingMessages[msg.PlayerID]
		if len(queue) >= maxQueuedMessagesPerPlayer {
			queue = queue[1:]
			logrus.WithFields(logrus.Fields{
				"player_id": msg.PlayerID,
				"event":     msg.Event,
				"limit":     maxQueuedMessagesPerPlayer,
			}).Warn("Offline queue full, dropping oldest message")
		}
		pendingMessages[msg.PlayerID] = append(queue, pendingMessage{msg: wsMsg, timestamp: time.Now()})
		pendingMu.Unlock()
	} else {
		for _, wc := range conns {
			_ = wc.conn.WriteJSON(wsMsg)
			messagesDelivered.Inc()
		}
		logrus.WithFields(logrus.Fields{
			"subject_id":  subjectID,
			"player_id":   msg.PlayerID,
			"event":       msg.Event,
			"connections": numConns,
		}).Info("Message delivered to player")
	}
	mu.Unlock()

	messagesPublished.Inc()
	w.WriteHeader(http.StatusOK)
}

// broadcastHandler handles incoming broadcast messages from authorized servers.
// Can target a specific player or all connected players.
// Enforces rate limits and increments metrics for delivered messages.
func broadcastHandler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	token := auth[7:]

	subjectID, ok := validateToken(token, true)
	if !ok {
		http.Error(w, "invalid server token", http.StatusForbidden)
		return
	}

	if !allow(subjectID, rateLimit, time.Second) {
		http.Error(w, "rate limit", http.StatusTooManyRequests)
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

	if req.PlayerID != nil {
		for _, wc := range players[*req.PlayerID] {
			_ = wc.conn.WriteJSON(wsMsg)
			messagesDelivered.Inc()
		}
	} else {
		for _, conns := range players {
			for _, wc := range conns {
				_ = wc.conn.WriteJSON(wsMsg)
				messagesDelivered.Inc()
			}
		}
	}

	messagesPublished.Inc()
	w.WriteHeader(http.StatusOK)
}

// initMetrics registers Prometheus metrics for connections, messages published, and messages delivered.
func initMetrics() {
	prometheus.MustRegister(connections, messagesPublished, messagesDelivered)
}

// startTokenRevalidation periodically validates all active websocket tokens.
// Invalid tokens cause connections to be closed and removed.
func startTokenRevalidation(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			mu.Lock()
			for playerID, conns := range players {
				for c, wc := range conns {
					_, valid := validateToken(wc.token, false)
					if !valid {
						logrus.WithFields(logrus.Fields{
							"player_id": playerID,
						}).Info("Token invalid, closing connection")
						_ = wc.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "token expired"))
						_ = wc.conn.Close()
						delete(conns, c)
						connections.Dec()
					}
				}
			}
			mu.Unlock()
		}
	}()
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

// pidFileExists checks if the PID file exists and reads its PID.
// Returns the PID and true if the file exists and contains a valid integer, otherwise 0 and false.
func pidFileExists(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
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
func setupLogging() error {
	logrus.SetFormatter(&logrus.JSONFormatter{})

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file %s: %w", logFile, err)
		}
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

	// Initialize DB
	if err := initDB(); err != nil {
		return fmt.Errorf("failed to init DB: %w", err)
	}

	// Daemonize if needed
	if daemonize {
		if err := daemonizeSelf(); err != nil {
			return fmt.Errorf("failed to daemonize: %w", err)
		}
	}

	initMetrics()

	// Start offline message cleanup
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
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
	}()

	// start WS token revalidation
	startTokenRevalidation(tokenRevalidationPeriod)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/publish", publishHandler)
	mux.HandleFunc("/broadcast", broadcastHandler)
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{Addr: serverAddr, Handler: mux}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logrus.Info("Shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		closeAllConnections()
	}()

	logrus.Infof("Server listening on %s", serverAddr)
	err := server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}
