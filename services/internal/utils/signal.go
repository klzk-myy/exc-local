// Package utils holds small shared helpers.
package utils

import (
	"context"
	"os/signal"
	"syscall"
)

// SignalContext returns a context that is cancelled on SIGINT or SIGTERM,
// plus a stop func that restores default signal handling. Every service
// uses it as its graceful-shutdown trigger.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
