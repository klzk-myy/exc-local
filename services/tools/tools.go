//go:build tools

// Package tools pins module dependencies whose consumers land in later
// phases, so go.mod declares them now per Task 1.3.2:
//   - gorilla/websocket: market-data WS fan-out (Phase-06)
//   - quickfixgo/quickfix: FIX gateway sessions (Phase-18)
//   - spf13/cobra: service CLI entrypoints (shared flags, Phase-05+)
package tools

import (
	_ "github.com/gorilla/websocket"
	_ "github.com/quickfixgo/quickfix"
	_ "github.com/spf13/cobra"
)
