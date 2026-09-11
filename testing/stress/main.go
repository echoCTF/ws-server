package main

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8097", "server address")
	workers := flag.Int("workers", 40, "concurrent workers")
	duration := flag.Duration("duration", 4*time.Second, "how long to hammer the server")
	flag.Parse()

	tokens := []string{
		"player-token-1", "player-token-2", "player-token-3",
		"player-token-4", "player-token-5", "player-token-6", "player-token-7",
		"bogus-1", "bogus-2", "",
	}

	stop := time.Now().Add(*duration)
	var wg sync.WaitGroup

	// Concurrent /ws connect/disconnect churn across valid and invalid tokens.
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(id)))
			for time.Now().Before(stop) {
				tok := tokens[r.Intn(len(tokens))]
				u := fmt.Sprintf("ws://%s/ws", *addr)
				if tok != "" {
					u += "?token=" + tok
				}
				d := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
				c, _, err := d.Dial(u, nil)
				if err != nil {
					continue
				}
				if r.Intn(4) == 0 {
					_ = c.WriteMessage(websocket.TextMessage, []byte("x")) // trigger client_data_frame path
				}
				_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				_, _, _ = c.ReadMessage()
				c.Close()
			}
		}(w)
	}

	// Concurrent /publish and /broadcast traffic against the same players.
	for w := 0; w < *workers/2; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(1000 + id)))
			for time.Now().Before(stop) {
				if r.Intn(2) == 0 {
					body := fmt.Sprintf(`{"player_id":"%d","event":"e","payload":{}}`, r.Intn(7)+1)
					req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/publish", *addr), bytes.NewBufferString(body))
					req.Header.Set("Authorization", "Bearer server-token-1")
					req.Header.Set("Content-Type", "application/json")
					resp, err := http.DefaultClient.Do(req)
					if err == nil {
						resp.Body.Close()
					}
				} else {
					req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/broadcast", *addr), bytes.NewBufferString(`{"event":"e","payload":{}}`))
					req.Header.Set("Authorization", "Bearer server-token-1")
					req.Header.Set("Content-Type", "application/json")
					resp, err := http.DefaultClient.Do(req)
					if err == nil {
						resp.Body.Close()
					}
				}
			}
		}(w)
	}

	wg.Wait()
	fmt.Println("stress run complete")
}
