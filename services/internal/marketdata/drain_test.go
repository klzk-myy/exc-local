package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Task 9.3.23 item 3: on shutdown the server emits system.reconnect and
// closes 1001; new upgrades refuse during the drain.
func TestDrainAdvisoryAndClose(t *testing.T) {
	srv, c := dial(t, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Drain(ctx, "maintenance", time.Second); err != nil && ctx.Err() == nil {
		t.Fatalf("drain: %v", err)
	}

	// Client sees the advisory then the close.
	var sawReconnect bool
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		mt, b, err := c.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.TextMessage && strings.Contains(string(b), `"system.reconnect"`) {
			sawReconnect = true
			if !strings.Contains(string(b), `"maintenance"`) {
				t.Fatalf("advisory missing reason: %s", b)
			}
		}
		if mt == websocket.CloseMessage {
			break
		}
	}
	if !sawReconnect {
		t.Fatal("no system.reconnect advisory received")
	}
}

func TestDrainRejectsNewUpgrades(t *testing.T) {
	srv, _ := dial(t, Config{})
	srv.draining.Store(true)

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	defer httpSrv.Close()
	resp, err := http.Get(httpSrv.URL + "/ws/v1/marketdata")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade during drain → %d, want 503", resp.StatusCode)
	}
}

func TestDrainIdempotent(t *testing.T) {
	srv := NewServer(Config{})
	ctx := context.Background()
	if err := srv.Drain(ctx, "maintenance", 10*time.Millisecond); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if err := srv.Drain(ctx, "maintenance", 10*time.Millisecond); err != nil {
		t.Fatalf("second drain should be a no-op: %v", err)
	}
	if !srv.Draining() {
		t.Fatal("Draining() false after Drain")
	}
}
