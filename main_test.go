package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

// resetRateLimiterState clears the package-level rate limiter maps and
// enable flags between tests, since they're shared globals.
func resetRateLimiterState() {
	ipMu.Lock()
	ipLimiters = make(map[string]*rateEntry)
	ipMu.Unlock()
	playerMu.Lock()
	playerLimiters = make(map[string]*rateEntry)
	playerMu.Unlock()
	rateLimitIPEnabled = false
	rateLimitPlayerEnabled = false
	rateLimitIP, rateBurstIP = 5, 20
	rateLimitPlayer, rateBurstPlayer = 2, 5
}

func TestAllowIP_DisabledByDefault(t *testing.T) {
	resetRateLimiterState()
	// rateLimitIPEnabled is false: every call must be allowed, regardless
	// of volume, since the flag was never set.
	for i := 0; i < 1000; i++ {
		if !allowIP("203.0.113.5") {
			t.Fatalf("allowIP rejected call %d while rateLimitIPEnabled=false", i)
		}
	}
}

func TestAllowPlayer_DisabledByDefault(t *testing.T) {
	resetRateLimiterState()
	for i := 0; i < 1000; i++ {
		if !allowPlayer("42") {
			t.Fatalf("allowPlayer rejected call %d while rateLimitPlayerEnabled=false", i)
		}
	}
}

func TestAllowIP_EnabledRespectsLimiter(t *testing.T) {
	resetRateLimiterState()
	rateLimitIPEnabled = true
	rateLimitIP, rateBurstIP = 1, 2 // burst 2, refill 1/s

	allowed := 0
	for i := 0; i < 5; i++ {
		if allowIP("203.0.113.9") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("expected exactly burst=2 immediate allows, got %d", allowed)
	}
}

func TestAllowPlayer_EnabledRespectsLimiter(t *testing.T) {
	resetRateLimiterState()
	rateLimitPlayerEnabled = true
	rateLimitPlayer, rateBurstPlayer = 1, 3

	allowed := 0
	for i := 0; i < 6; i++ {
		if allowPlayer("player-9") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("expected exactly burst=3 immediate allows, got %d", allowed)
	}
}

func TestAllowIP_EnabledDoesNotAffectOtherIPs(t *testing.T) {
	resetRateLimiterState()
	rateLimitIPEnabled = true
	rateLimitIP, rateBurstIP = 1, 1

	if !allowIP("10.0.0.1") {
		t.Fatal("first call for 10.0.0.1 should be allowed")
	}
	if allowIP("10.0.0.1") {
		t.Fatal("second immediate call for 10.0.0.1 should be throttled (burst=1)")
	}
	// A different IP has its own bucket and must not be affected.
	if !allowIP("10.0.0.2") {
		t.Fatal("a different IP must have its own independent bucket")
	}
}

// TestCheckConnectionLimit exercises the real function end-to-end through a
// live websocket upgrade, since it needs a genuine *websocket.Conn to call
// WriteMessage/Close on in the rejection path.
func TestCheckConnectionLimit(t *testing.T) {
	origMax := maxConnectionsPerPlayer
	defer func() { maxConnectionsPerPlayer = origMax }()
	maxConnectionsPerPlayer = 2

	slotA := &websocket.Conn{}
	slotB := &websocket.Conn{}

	mu.Lock()
	players = map[string]map[*websocket.Conn]*wsConnection{
		"p1": {
			slotA: {},
			slotB: {},
		},
	}
	mu.Unlock()

	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		u := websocket.Upgrader{}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		if ok := checkConnectionLimit(c, r, "127.0.0.1", "tok", "p1"); ok {
			t.Errorf("expected checkConnectionLimit to report at-capacity (2/2), got ok=true")
		}
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	d := websocket.Dialer{}
	c, _, err := d.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_, _, err = c.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected a close error, got %v", err)
	}
	if closeErr.Code != websocket.CloseTryAgainLater || closeErr.Text != "too many connections" {
		t.Errorf("close=(%d,%q), want (%d,%q)", closeErr.Code, closeErr.Text,
			websocket.CloseTryAgainLater, "too many connections")
	}

	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server-side handler did not finish in time")
	}
}

func TestCheckConnectionLimit_UnderCapacityAllowed(t *testing.T) {
	origMax := maxConnectionsPerPlayer
	defer func() { maxConnectionsPerPlayer = origMax }()
	maxConnectionsPerPlayer = 2

	mu.Lock()
	players = map[string]map[*websocket.Conn]*wsConnection{
		"p1": {&websocket.Conn{}: {}}, // 1 of 2 slots used
	}
	mu.Unlock()

	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		u := websocket.Upgrader{}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		if ok := checkConnectionLimit(c, r, "127.0.0.1", "tok", "p1"); !ok {
			t.Errorf("expected checkConnectionLimit to allow (1/2 used), got ok=false")
		}
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	d := websocket.Dialer{}
	c, _, err := d.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()

	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server-side handler did not finish in time")
	}
}

func TestPostUpgradeRejectsTableComplete(t *testing.T) {
	want := []string{"missing_token", "invalid_token", "token_check_failed"}
	for _, reason := range want {
		spec, ok := postUpgradeRejects[reason]
		if !ok {
			t.Errorf("postUpgradeRejects missing entry for %q", reason)
			continue
		}
		if spec.closeMsg == "" {
			t.Errorf("%q: empty closeMsg", reason)
		}
		if spec.logMsg == "" {
			t.Errorf("%q: empty logMsg", reason)
		}
		if spec.closeCode != websocket.ClosePolicyViolation && spec.closeCode != websocket.CloseTryAgainLater {
			t.Errorf("%q: unexpected closeCode %d", reason, spec.closeCode)
		}
	}
	// missing_token must NOT log the token (there isn't one); the other
	// two must, matching LOGGING.md's documented field table.
	if postUpgradeRejects["missing_token"].tokenInLog {
		t.Error("missing_token should not include the token field")
	}
	if !postUpgradeRejects["invalid_token"].tokenInLog {
		t.Error("invalid_token should include the token field")
	}
	if !postUpgradeRejects["token_check_failed"].tokenInLog {
		t.Error("token_check_failed should include the token field")
	}
}

func TestOversizedHeader(t *testing.T) {
	origMax := maxHeaderValueLen
	defer func() { maxHeaderValueLen = origMax }()
	maxHeaderValueLen = 10

	mk := func(cookie, origin, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/ws", nil)
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	if got := oversizedHeader(mk("short", "short", "short")); got != "" {
		t.Errorf("all under limit: got %q, want \"\"", got)
	}
	if got := oversizedHeader(mk("way-too-long-value", "short", "short")); got != "Cookie" {
		t.Errorf("oversized cookie: got %q, want \"Cookie\"", got)
	}
	if got := oversizedHeader(mk("short", "way-too-long-value", "short")); got != "Origin" {
		t.Errorf("oversized origin: got %q, want \"Origin\"", got)
	}
	if got := oversizedHeader(mk("short", "short", "way-too-long-value")); got != "X-Forwarded-For" {
		t.Errorf("oversized xff: got %q, want \"X-Forwarded-For\"", got)
	}
	// Exactly at the limit must pass (strictly-greater-than check).
	if got := oversizedHeader(mk("0123456789", "", "")); got != "" {
		t.Errorf("exactly at limit: got %q, want \"\" (limit is exclusive)", got)
	}
}

func TestPreUpgradeCheck_OversizedHeaderRejectsBeforeAnythingElse(t *testing.T) {
	origMax := maxHeaderValueLen
	defer func() { maxHeaderValueLen = origMax }()
	maxHeaderValueLen = 10

	resetRateLimiterState() // make sure a rate-limit rejection can't mask this

	r := httptest.NewRequest("GET", "/ws", nil)
	r.Header.Set("Cookie", "this-cookie-value-is-way-over-the-limit")
	w := httptest.NewRecorder()

	_, _, ok := preUpgradeCheck(w, r, "127.0.0.1", "")
	if ok {
		t.Fatal("expected ok=false for an oversized Cookie header")
	}
	if w.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestHeaderFieldsTooLarge)
	}
}

func TestClientIP_TrustXFF(t *testing.T) {
	origTrust := trustXFF
	defer func() { trustXFF = origTrust }()

	r := httptest.NewRequest("GET", "/ws", nil)
	r.RemoteAddr = "192.0.2.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	trustXFF = false
	if got := clientIP(r); got != "192.0.2.1" {
		t.Errorf("trustXFF=false: got %q, want peer address 192.0.2.1", got)
	}

	trustXFF = true
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("trustXFF=true: got %q, want leftmost XFF entry 203.0.113.7", got)
	}
}

func TestValidateToken(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?cache=shared"

	origDB := db
	defer func() { db = origDB }()

	var err error
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE ws_token (
		token TEXT PRIMARY KEY, player_id INTEGER, subject_id TEXT,
		is_server INTEGER NOT NULL, expires_at DATETIME NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ws_token (token, player_id, subject_id, is_server, expires_at)
		VALUES ('good-player', 7, 'p7', 0, '2000-01-01 00:00:00')`); err != nil { // expired on purpose
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ws_token (token, player_id, subject_id, is_server, expires_at)
		VALUES ('good-server', NULL, 's1', 1, '2099-01-01 00:00:00')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if id, err := validateToken("good-player", false); err != nil || id != "7" {
		t.Errorf("valid player token: id=%q err=%v, want id=7 err=nil", id, err)
	}
	if id, err := validateToken("good-player", true); err != ErrTokenNotFound {
		t.Errorf("player token checked as server: id=%q err=%v, want ErrTokenNotFound", id, err)
	}
	if id, err := validateToken("good-server", true); err != nil || id != "s1" {
		t.Errorf("valid server token: id=%q err=%v, want id=s1 err=nil", id, err)
	}
	if _, err := validateToken("nonexistent", false); err != ErrTokenNotFound {
		t.Errorf("unknown token: err=%v, want ErrTokenNotFound", err)
	}
	// expires_at is in the past, but validateToken must not care: it only
	// checks that the row exists. This is the documented (and, per
	// today's discussion, deliberate) behavior.
	if _, err := validateToken("good-player", false); err != nil {
		t.Errorf("expired-but-present row should still validate, got err=%v", err)
	}
}
