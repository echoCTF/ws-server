package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Black-box scenario runner for wsserver. Talks only HTTP/WS, so the same
// binary works unmodified against both the pre-refactor and post-refactor
// server builds. "core" scenarios are expected to behave identically on
// both; "ratelimit-default" and "ratelimit-explicit" only apply to the new
// build's opt-in rate limiting and are run separately.

type result struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

var (
	addr string
	mode string
)

func main() {
	flag.StringVar(&addr, "addr", "127.0.0.1:8081", "server address")
	flag.StringVar(&mode, "mode", "core", "core | ratelimit-default | ratelimit-explicit")
	flag.Parse()

	var results []result
	switch mode {
	case "core":
		results = runCore()
	case "ratelimit-default":
		results = runRateLimitDefault()
	case "ratelimit-explicit":
		results = runRateLimitExplicit()
	default:
		fmt.Fprintln(os.Stderr, "unknown -mode:", mode)
		os.Exit(2)
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println(string(out))

	failed := 0
	for _, r := range results {
		if !r.Pass {
			failed++
		}
	}
	fmt.Fprintf(os.Stderr, "\n%d/%d passed\n", len(results)-failed, len(results))
	if failed > 0 {
		os.Exit(1)
	}
}

func add(results []result, name string, pass bool, detail string) []result {
	return append(results, result{Name: name, Pass: pass, Detail: detail})
}

// dialWS dials /ws?token=... and returns the connection (nil if the dial
// itself failed at the HTTP level), the handshake response, and any error.
func dialWS(token string) (*websocket.Conn, *http.Response, error) {
	return dialWSWithHeader(token, "", "")
}

// dialWSWithHeader is dialWS plus one extra request header, for scenarios
// that need to control what wsserver sees on the handshake (e.g. an
// oversized Cookie).
func dialWSWithHeader(token, headerName, headerValue string) (*websocket.Conn, *http.Response, error) {
	u := fmt.Sprintf("ws://%s/ws", addr)
	if token != "__omit__" {
		u += "?token=" + token
	}
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	var hdr http.Header
	if headerName != "" {
		hdr = http.Header{}
		hdr.Set(headerName, headerValue)
	}
	return d.Dial(u, hdr)
}

// expectClose reads until the connection closes and returns the close code
// and reason gorilla reports, or ("", -1) if it didn't observe a close
// frame before some other error/timeout.
func expectClose(conn *websocket.Conn, timeout time.Duration) (code int, reason string) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue // control/ping frames etc, keep reading
		}
		if ce, ok := err.(*websocket.CloseError); ok {
			return ce.Code, ce.Text
		}
		return -1, err.Error()
	}
}

func httpPost(path string, headers map[string]string, body string) (*http.Response, string) {
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s%s", addr, path), bytes.NewBufferString(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func runCore() []result {
	var res []result

	// 1. Missing token: expect close code 1008 "missing token", header
	// X-WS-Reject: missing_token, no X-Player-Uid (nothing was resolved),
	// and no "token" field is asserted here since we can't see server logs
	// from the client, that's diffed separately from the captured log
	// files.
	{
		conn, httpResp, err := dialWS("__omit__")
		pass := err == nil && httpResp != nil && httpResp.Header.Get("X-WS-Reject") == "missing_token" &&
			httpResp.Header.Get("X-Player-Uid") == ""
		detail := fmt.Sprintf("dial err=%v X-WS-Reject=%q X-Player-Uid=%q", err, headerOrNil(httpResp), playerUIDOrNil(httpResp))
		if pass {
			code, reason := expectClose(conn, 3*time.Second)
			pass = code == websocket.ClosePolicyViolation && reason == "missing token"
			detail = fmt.Sprintf("X-WS-Reject=%s close=(%d,%q)", httpResp.Header.Get("X-WS-Reject"), code, reason)
			conn.Close()
		}
		res = add(res, "ws_missing_token", pass, detail)
	}

	// 2. Invalid token: expect close code 1008 "invalid token", no
	// X-Player-Uid (the token never resolved to a player).
	{
		conn, httpResp, err := dialWS("this-token-does-not-exist")
		pass := err == nil && httpResp != nil && httpResp.Header.Get("X-WS-Reject") == "invalid_token" &&
			httpResp.Header.Get("X-Player-Uid") == ""
		detail := fmt.Sprintf("dial err=%v X-WS-Reject=%q X-Player-Uid=%q", err, headerOrNil(httpResp), playerUIDOrNil(httpResp))
		if pass {
			code, reason := expectClose(conn, 3*time.Second)
			pass = code == websocket.ClosePolicyViolation && reason == "invalid token"
			detail = fmt.Sprintf("X-WS-Reject=%s close=(%d,%q)", httpResp.Header.Get("X-WS-Reject"), code, reason)
			conn.Close()
		}
		res = add(res, "ws_invalid_token", pass, detail)
	}

	// 3. Valid token: expect a clean upgrade, no immediate close, and
	// X-Player-Uid set to the token's resolved player_id (1, for
	// player-token-1 per the fixture).
	{
		conn, httpResp, err := dialWS("player-token-1")
		pass := err == nil && httpResp != nil && httpResp.Header.Get("X-WS-Reject") == "" &&
			httpResp.Header.Get("X-Player-Uid") == "1"
		detail := fmt.Sprintf("dial err=%v status=%v X-Player-Uid=%q", err, statusOrNil(httpResp), playerUIDOrNil(httpResp))
		if pass {
			conn.Close()
		}
		res = add(res, "ws_valid_token_connects", pass, detail)
	}

	// 4. Expired-but-present row: expires_at is in the past but the row
	// still exists, expect it still validates (validateToken never checks
	// expires_at, see main.go).
	{
		conn, httpResp, err := dialWS("expired-player-token")
		pass := err == nil && httpResp != nil && httpResp.Header.Get("X-WS-Reject") == ""
		detail := fmt.Sprintf("dial err=%v status=%v", err, statusOrNil(httpResp))
		if pass {
			conn.Close()
		}
		res = add(res, "ws_expired_row_still_validates", pass, detail)
	}

	// 5. Client sends a data frame: expect server closes with 1008
	// "client messages not accepted".
	{
		conn, _, err := dialWS("player-token-1")
		pass := err == nil
		detail := fmt.Sprintf("dial err=%v", err)
		if pass {
			_ = conn.WriteMessage(websocket.TextMessage, []byte("hello"))
			code, reason := expectClose(conn, 3*time.Second)
			pass = code == websocket.ClosePolicyViolation && reason == "client messages not accepted"
			detail = fmt.Sprintf("close=(%d,%q)", code, reason)
			conn.Close()
		}
		res = add(res, "ws_client_data_frame_rejected", pass, detail)
	}

	// 5b. Oversized Cookie header: rejected with 431 before any upgrade at
	// all, no X-WS-Reject header involved since this never gets that far.
	// Uses a value far bigger than any sane -max-header-value default so
	// this doesn't depend on what the server was actually configured with.
	{
		huge := strings.Repeat("a", 9000)
		_, httpResp, err := dialWSWithHeader("player-token-1", "Cookie", huge)
		pass := err != nil && httpResp != nil && httpResp.StatusCode == http.StatusRequestHeaderFieldsTooLarge
		detail := fmt.Sprintf("err=%v status=%v", err, statusOrNil(httpResp))
		res = add(res, "ws_oversized_cookie_rejected_431", pass, detail)
	}

	// 6. Too many connections: max-conns defaults to 10 on both builds.
	// Open 10 for player-token-2 (exclusive to this scenario), then the
	// 11th must be rejected with 1013 "too many connections".
	{
		var conns []*websocket.Conn
		ok := true
		var detail string
		for i := 0; i < 10; i++ {
			c, _, err := dialWS("player-token-2")
			if err != nil {
				ok = false
				detail = fmt.Sprintf("connection %d failed: %v", i+1, err)
				break
			}
			conns = append(conns, c)
		}
		if ok {
			c11, _, err := dialWS("player-token-2")
			if err != nil {
				ok = false
				detail = fmt.Sprintf("11th dial error: %v", err)
			} else {
				code, reason := expectClose(c11, 3*time.Second)
				ok = code == websocket.CloseTryAgainLater && reason == "too many connections"
				detail = fmt.Sprintf("11th close=(%d,%q)", code, reason)
				c11.Close()
			}
		}
		for _, c := range conns {
			c.Close()
		}
		res = add(res, "ws_too_many_connections", ok, detail)
	}

	// 7. Publish to an online player: connect player-token-3, publish to
	// player_id=3, expect the live connection receives it.
	{
		conn, _, err := dialWS("player-token-3")
		pass := err == nil
		detail := fmt.Sprintf("dial err=%v", err)
		if pass {
			time.Sleep(100 * time.Millisecond) // let registerConnection land
			resp, body := httpPost("/publish",
				map[string]string{"Authorization": "Bearer server-token-1", "Content-Type": "application/json"},
				`{"player_id":"3","event":"notify","payload":{"x":1}}`)
			pass = resp != nil && resp.StatusCode == 200
			detail = fmt.Sprintf("publish status=%v body=%q", statusOrNil(resp), body)
			if pass {
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var msg map[string]interface{}
				err = conn.ReadJSON(&msg)
				pass = err == nil && msg["event"] == "notify"
				detail += fmt.Sprintf(" received=%v err=%v", msg, err)
			}
			conn.Close()
		}
		res = add(res, "publish_delivers_to_online_player", pass, detail)
	}

	// 8. Publish to an offline player queues it, then connecting within
	// offline-ttl flushes it.
	{
		resp, body := httpPost("/publish",
			map[string]string{"Authorization": "Bearer server-token-1", "Content-Type": "application/json"},
			`{"player_id":"6","event":"queued_notify","payload":{"y":2}}`)
		pass := resp != nil && resp.StatusCode == 200
		detail := fmt.Sprintf("publish status=%v body=%q", statusOrNil(resp), body)
		if pass {
			conn, _, err := dialWS("player-token-6")
			pass = err == nil
			if pass {
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var msg map[string]interface{}
				err = conn.ReadJSON(&msg)
				pass = err == nil && msg["event"] == "queued_notify"
				detail += fmt.Sprintf(" flushed=%v err=%v", msg, err)
				conn.Close()
			}
		}
		res = add(res, "publish_queues_then_flushes_offline", pass, detail)
	}

	// 9. Publish with no Authorization header: 401.
	{
		resp, _ := httpPost("/publish", nil, `{"player_id":"1","event":"x","payload":{}}`)
		pass := resp != nil && resp.StatusCode == 401
		res = add(res, "publish_missing_auth_401", pass, fmt.Sprintf("status=%v", statusOrNil(resp)))
	}

	// 10. Publish with an invalid server token: 403.
	{
		resp, _ := httpPost("/publish",
			map[string]string{"Authorization": "Bearer not-a-real-token"},
			`{"player_id":"1","event":"x","payload":{}}`)
		pass := resp != nil && resp.StatusCode == 403
		res = add(res, "publish_bad_server_token_403", pass, fmt.Sprintf("status=%v", statusOrNil(resp)))
	}

	// 11. Publish with malformed JSON body: 400.
	{
		resp, _ := httpPost("/publish",
			map[string]string{"Authorization": "Bearer server-token-1", "Content-Type": "application/json"},
			`{not json`)
		pass := resp != nil && resp.StatusCode == 400
		res = add(res, "publish_bad_body_400", pass, fmt.Sprintf("status=%v", statusOrNil(resp)))
	}

	// 12. Broadcast with no player_id delivers to every connected client.
	{
		c4, _, err4 := dialWS("player-token-4")
		c5, _, err5 := dialWS("player-token-5")
		pass := err4 == nil && err5 == nil
		detail := fmt.Sprintf("dial errs: %v %v", err4, err5)
		if pass {
			time.Sleep(100 * time.Millisecond)
			resp, body := httpPost("/broadcast",
				map[string]string{"Authorization": "Bearer server-token-1", "Content-Type": "application/json"},
				`{"event":"reload","payload":{}}`)
			pass = resp != nil && resp.StatusCode == 200
			detail = fmt.Sprintf("broadcast status=%v body=%q", statusOrNil(resp), body)
			if pass {
				_ = c4.SetReadDeadline(time.Now().Add(3 * time.Second))
				_ = c5.SetReadDeadline(time.Now().Add(3 * time.Second))
				var m4, m5 map[string]interface{}
				e4 := c4.ReadJSON(&m4)
				e5 := c5.ReadJSON(&m5)
				pass = e4 == nil && e5 == nil && m4["event"] == "reload" && m5["event"] == "reload"
				detail += fmt.Sprintf(" m4=%v e4=%v m5=%v e5=%v", m4, e4, m5, e5)
			}
			c4.Close()
			c5.Close()
		}
		res = add(res, "broadcast_all_reaches_every_connection", pass, detail)
	}

	// 13. Broadcast to a specific, never-connected player: 200 OK,
	// message simply discarded (nothing to receive, nothing to assert
	// beyond the status code, this is the "no queueing on broadcast"
	// contract from the README).
	{
		resp, body := httpPost("/broadcast",
			map[string]string{"Authorization": "Bearer server-token-1", "Content-Type": "application/json"},
			`{"player_id":"7","event":"x","payload":{}}`)
		pass := resp != nil && resp.StatusCode == 200
		res = add(res, "broadcast_offline_player_200_no_queue", pass, fmt.Sprintf("status=%v body=%q", statusOrNil(resp), body))
	}

	// 14. /metrics is reachable and exposes the documented series names.
	{
		resp, err := http.Get(fmt.Sprintf("http://%s/metrics", addr))
		pass := err == nil && resp.StatusCode == 200
		var body string
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
			resp.Body.Close()
		}
		pass = pass && strings.Contains(body, "ws_active_connections") &&
			strings.Contains(body, "ws_messages_published_total") &&
			strings.Contains(body, "ws_messages_delivered_total")
		res = add(res, "metrics_endpoint_exposes_expected_series", pass, fmt.Sprintf("err=%v len(body)=%d", err, len(body)))
	}

	return res
}

// runRateLimitDefault only applies to the new build: with no rate-limit
// flags passed, hammering /ws with garbage tokens from one IP must never
// produce a 429.
func runRateLimitDefault() []result {
	var res []result
	got429 := false
	for i := 0; i < 60; i++ {
		resp, _, err := dialWithHTTPStatus("bogus-token-" + fmt.Sprint(i))
		if err == nil && resp == 429 {
			got429 = true
			break
		}
	}
	res = add(res, "no_429_when_ratelimit_flags_unset", !got429, fmt.Sprintf("got429=%v after 60 rapid attempts", got429))
	return res
}

// runRateLimitExplicit only applies to the new build: with the rate-limit
// flags explicitly set low, a burst must eventually produce a 429.
func runRateLimitExplicit() []result {
	var res []result
	got429 := false
	var lastStatus int
	for i := 0; i < 30; i++ {
		resp, _, err := dialWithHTTPStatus("bogus-token-" + fmt.Sprint(i))
		if err == nil {
			lastStatus = resp
			if resp == 429 {
				got429 = true
				break
			}
		}
	}
	res = add(res, "429_eventually_when_ratelimit_flags_set", got429, fmt.Sprintf("got429=%v lastStatus=%d", got429, lastStatus))
	return res
}

// dialWithHTTPStatus attempts a raw dial and reports the HTTP status code
// when the dial is rejected at the HTTP level (e.g. 429 before upgrade).
func dialWithHTTPStatus(token string) (status int, code int, err error) {
	conn, resp, derr := dialWS(token)
	if derr != nil {
		if resp != nil {
			return resp.StatusCode, -1, nil
		}
		return 0, -1, derr
	}
	c, _ := expectClose(conn, 2*time.Second)
	conn.Close()
	return 101, c, nil
}

func headerOrNil(r *http.Response) string {
	if r == nil {
		return "<nil resp>"
	}
	return r.Header.Get("X-WS-Reject")
}

func playerUIDOrNil(r *http.Response) string {
	if r == nil {
		return "<nil resp>"
	}
	return r.Header.Get("X-Player-Uid")
}

func statusOrNil(r *http.Response) interface{} {
	if r == nil {
		return nil
	}
	return r.StatusCode
}
