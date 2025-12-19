package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
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

// ///////////////////
// RATE LIMITER
// ///////////////////
type limiter struct {
	tokens int
	last   time.Time
}

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

// ///////////////////
// DB
// ///////////////////
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

// validateToken checks token validity in DB
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

// ///////////////////
// CONNECTION MANAGEMENT
// ///////////////////

// registerConnection registers a WS connection and stores its token
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

// unregisterConnection removes a WS connection
func unregisterConnection(playerID string, c *websocket.Conn) {
	mu.Lock()
	defer mu.Unlock()
	delete(players[playerID], c)
	if len(players[playerID]) == 0 {
		delete(players, playerID)
	}
	connections.Dec()
}

// closeAllConnections closes all WS connections (on shutdown)
func closeAllConnections() {
	mu.Lock()
	defer mu.Unlock()
	for _, conns := range players {
		for _, wc := range conns {
			wc.conn.Close()
		}
	}
}

// flushPendingMessages sends queued messages to a newly connected player
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

// ///////////////////
// WEBSOCKET HANDLER
// ///////////////////
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

// ///////////////////
// PUBLISH HANDLER
// ///////////////////
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

// ///////////////////
// BROADCAST HANDLER
// ///////////////////
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

// ///////////////////
// METRICS
// ///////////////////
func initMetrics() {
	prometheus.MustRegister(connections, messagesPublished, messagesDelivered)
}

// ///////////////////
// TOKEN REVALIDATION
// ///////////////////

// startTokenRevalidation periodically checks all WS tokens and closes invalid ones
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

func daemonizeSelf() {
	if os.Getenv("DAEMONIZED") == "1" {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot get executable: %v", err)
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
		log.Fatalf("failed to daemonize: %v", err)
	}

	os.Exit(0)
}

// ///////////////////
// MAIN
// ///////////////////
func main() {
	var origins string

	flag.StringVar(&dbDriver, "db", "sqlite", "Database driver")
	flag.StringVar(&dbDSN, "dsn", "file:ws_tokens.db?cache=shared", "Database DSN")
	flag.StringVar(&serverAddr, "addr", ":8080", "Server address")
	flag.StringVar(&origins, "origins", "", "Allowed WS origins")
	flag.StringVar(&logFile, "log-file", "", "Path to log file (default: stdout)")
	flag.StringVar(&logLevel, "log-level", "info", "Log level (panic, fatal, error, warn, info, debug, trace)")
	flag.IntVar(&maxQueuedMessagesPerPlayer, "max-queued", 100, "Maximum queued messages per player")
	flag.IntVar(&rateLimit, "rate-limit", 10, "Number of messages allowed per rate-period per server token")
	flag.DurationVar(&ratePeriod, "rate-period", time.Second, "Duration for rate limiting (e.g., 1s, 500ms)")
	flag.IntVar(&maxConnectionsPerPlayer, "max-conns", 5, "Maximum concurrent WebSocket connections per player")
	flag.DurationVar(&tokenRevalidationPeriod, "revalidate-period", time.Minute, "Period for WS token revalidation (e.g., 30s, 1m)")
	flag.DurationVar(&offlineTTL, "offline-ttl", 10*time.Second, "Duration that messages will be stored offline (e.g., 30s, 1m)")
	flag.BoolVar(&daemonize, "daemon", false, "Run as daemon (background process)")
	flag.Parse()

	if origins != "" {
		allowedOrigins = strings.Split(origins, ",")
	}

	logrus.SetFormatter(&logrus.JSONFormatter{})

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("failed to open log file %s: %v", logFile, err)
		}
		logrus.SetOutput(f)
	} else {
		logrus.SetOutput(os.Stdout)
	}

	level, err := logrus.ParseLevel(strings.ToLower(logLevel))
	if err != nil {
		log.Fatalf("invalid log level: %s", logLevel)
	}
	logrus.SetLevel(level)

	if daemonize {
		daemonizeSelf()
	}

	if err := initDB(); err != nil {
		log.Fatal(err)
	}

	initMetrics()

	// cleanup expired offline messages
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

	// graceful shutdown
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
	log.Fatal(server.ListenAndServe())
}
