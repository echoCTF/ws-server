package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
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

// /////////////////////
// GLOBAL CONFIG
// /////////////////////
var (
	dbDSN      string
	serverAddr string

	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second
	rateLimit  = 10 // messages per second per server token
)

// /////////////////////
// GLOBALS
// /////////////////////
var (
	db       *sql.DB
	upgrader = websocket.Upgrader{}
	mu       sync.Mutex
	players  = make(map[string]map[*websocket.Conn]bool)

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

	// Rate limiting per server token
	limiters = make(map[string]*limiter)
	lm       sync.Mutex
)

// /////////////////////
// RATE LIMITER
// /////////////////////
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

// /////////////////////
// MESSAGE STRUCT
// /////////////////////
type Message struct {
	PlayerID string      `json:"player_id"`
	Event    string      `json:"event"`
	Payload  interface{} `json:"payload"`
}

// /////////////////////
// DB FUNCTIONS
// /////////////////////
func initDB() error {
	var err error
	db, err = sql.Open("sqlite", dbDSN) // or MySQL DSN
	if err != nil {
		return err
	}
	return db.Ping()
}

// Returns: subjectID, isServer, valid
func validateToken(token string) (string, bool, bool) {
	const q = `
        SELECT subject_id, is_server
        FROM ws_tokens
        WHERE token = ?
          AND expires_at > CURRENT_TIMESTAMP
          AND revoked = 0
        LIMIT 1
    `
	var sid string
	var isServer bool
	err := db.QueryRow(q, token).Scan(&sid, &isServer)
	if err != nil {
		return "", false, false
	}
	return sid, isServer, true
}

// /////////////////////
// WS CONNECTION MANAGEMENT
// /////////////////////
func registerConnection(playerID string, c *websocket.Conn) {
	mu.Lock()
	defer mu.Unlock()
	if players[playerID] == nil {
		players[playerID] = make(map[*websocket.Conn]bool)
	}
	players[playerID][c] = true
	connections.Inc()
}

func unregisterConnection(playerID string, c *websocket.Conn) {
	mu.Lock()
	defer mu.Unlock()
	delete(players[playerID], c)
	if len(players[playerID]) == 0 {
		delete(players, playerID)
	}
	connections.Dec()
}

func closeAllConnections() {
	mu.Lock()
	defer mu.Unlock()
	for _, conns := range players {
		for c := range conns {
			c.Close()
		}
	}
}

// /////////////////////
// WS HANDLER
// /////////////////////
func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	subjectID, isServer, ok := validateToken(token)
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	if isServer {
		http.Error(w, "servers cannot connect as players", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	registerConnection(subjectID, conn)
	defer unregisterConnection(subjectID, conn)

	logrus.WithFields(logrus.Fields{
		"player_id": subjectID,
		"event":     "ws_connect",
		"ip":        r.RemoteAddr,
	}).Info("Player connected")

	// Heartbeats
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

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	logrus.WithFields(logrus.Fields{
		"player_id": subjectID,
		"event":     "ws_disconnect",
	}).Info("Player disconnected")
}

// /////////////////////
// PUBLISH HANDLER
// /////////////////////
func publishHandler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || auth[:7] != "Bearer " {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	token := auth[7:]

	subjectID, isServer, ok := validateToken(token)
	if !ok || !isServer {
		http.Error(w, "invalid server token", http.StatusForbidden)
		return
	}

	if !allow(subjectID, rateLimit, time.Second) {
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return
	}

	var msg Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	mu.Lock()
	conns := players[msg.PlayerID]
	numConns := len(conns)
	if numConns == 0 {
		// Player not connected
		logrus.WithFields(logrus.Fields{
			"server_id": subjectID,
			"player_id": msg.PlayerID,
			"event":     msg.Event,
		}).Warn("Attempted to publish message, but player not connected")
	} else {
		// Deliver to all active connections
		for c := range conns {
			_ = c.WriteJSON(msg)
		}
		logrus.WithFields(logrus.Fields{
			"server_id":   subjectID,
			"player_id":   msg.PlayerID,
			"event":       msg.Event,
			"connections": numConns,
		}).Info("Message delivered to player")
	}
	mu.Unlock()

	messagesPublished.Inc()
	messagesDelivered.Add(float64(numConns))

	w.WriteHeader(200)
}

// /////////////////////
// METRICS
// /////////////////////
func initMetrics() {
	prometheus.MustRegister(connections, messagesPublished, messagesDelivered)
}

// /////////////////////
// MAIN
// /////////////////////
func main() {
	// Command-line flags
	flag.StringVar(&dbDSN, "dsn", "file:ws_tokens.db?cache=shared", "Database DSN")
	flag.StringVar(&serverAddr, "addr", ":8080", "Server listen address")
	flag.Parse()

	logrus.SetFormatter(&logrus.JSONFormatter{})
	logrus.SetOutput(os.Stdout)
	logrus.SetLevel(logrus.InfoLevel)

	if err := initDB(); err != nil {
		log.Fatalf("DB init failed: %v", err)
	}

	initMetrics()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/publish", publishHandler)
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    serverAddr,
		Handler: mux,
	}

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
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.Fatalf("Server failed: %v", err)
	}
}
