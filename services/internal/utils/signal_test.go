package utils

import (
	"syscall"
	"testing"
	"time"
)

func TestSignalContextCancelsOnSIGTERM(t *testing.T) {
	ctx, stop := SignalContext()
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("self SIGTERM: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ctx not cancelled by SIGTERM")
	}
}

// Note: the stop()→default-restore path is deliberately untested —
// restoring default SIGTERM handling and re-signalling would terminate
// the test binary.
