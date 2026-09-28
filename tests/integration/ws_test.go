// WebSocket leg — /ws/v1 handshake evidence for criteria 187/253.
// A real gorilla/websocket dial + authenticate frame against the live
// gateway (JWT path uses the stack's minted HS256 keyring).
package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"exchange-integration/itest"

	"github.com/gorilla/websocket"
)

// wsAuthProbe dials ws://<gw>/ws/v1, authenticates with a minted JWT,
// then issues order.status — returns (ok, detail).
func wsAuthProbe(t *testing.T, s *stack) (bool, string) {
	t.Helper()
	url := "ws" + s.gw.Addr[len("http"):] + "/ws/v1"
	hdr := http.Header{"X-Forwarded-For": []string{testIP(t)}}
	c, _, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		return false, fmt.Sprintf("dial %s: %v", url, err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))

	tok, err := itest.MintJWT(stk.jwtKey, strconv.FormatInt(s.fx.UserID, 10), s.fx.AccountID, nil)
	if err != nil {
		return false, "mint jwt: " + err.Error()
	}
	// §10.5 item 1 — protocol_version is mandatory on authenticate.
	auth := map[string]any{
		"action":           "authenticate",
		"request_id":       "itest-ws-auth",
		"protocol_version": 1,
		"token":            tok,
	}
	if err := c.WriteJSON(auth); err != nil {
		return false, "write auth frame: " + err.Error()
	}

	// Read until an authenticate response or deadline.
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			return false, "read: " + err.Error()
		}
		var f struct {
			Action    string `json:"action"`
			Status    string `json:"status"`
			Error     string `json:"error"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(msg, &f) != nil {
			continue
		}
		if f.Action == "authenticate" {
			if f.Status == "ACK" || f.Error == "" {
				return true, "ws authenticate ACK"
			}
			return false, fmt.Sprintf("ws authenticate → %s", f.Error)
		}
	}
}
