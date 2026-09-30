// notify.go — minimal sd_notify client (Task 9.3.28, spec §19.13.3 tier 3).
//
// exchange-watchdogd runs under Type=notify with WatchdogSec=500ms; this
// implements exactly the datagram protocol systemd expects on
// $NOTIFY_SOCKET without pulling in a systemd binding. The socket may be
// a filesystem path or an abstract name ("@foo" / "\0foo"); a datagram is
// a newline-separated list of KEY=value assignments.
//
// Fail-closed: a notify failure is returned to the caller — the supervisor
// treats a missed pet as an error worth logging (systemd will SIGABRT us
// at the watchdog boundary anyway; the log line is the evidence).
package watchdog

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Notifier sends sd_notify state datagrams. The zero value (Socket == "")
// is a disabled notifier: every method is a no-op returning nil so the
// same code path runs under systemd and on a dev terminal.
type Notifier struct {
	Socket string // raw $NOTIFY_SOCKET value
}

// NotifySocketEnv is the environment variable systemd sets.
const NotifySocketEnv = "NOTIFY_SOCKET"

// NewNotifier builds a Notifier from $NOTIFY_SOCKET.
func NewNotifier() *Notifier {
	return &Notifier{Socket: os.Getenv(NotifySocketEnv)}
}

// Enabled reports whether a notify socket was configured.
func (n *Notifier) Enabled() bool { return n != nil && n.Socket != "" }

// Notify sends one state datagram (e.g. "WATCHDOG=1", "STATUS=...").
// Multi-field payloads are joined with "\n" by the caller.
func (n *Notifier) Notify(state string) error {
	if !n.Enabled() {
		return nil
	}
	name := n.Socket
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:] // abstract socket
	} else if strings.HasPrefix(name, "\x00") {
		// already abstract form
	} else if !strings.HasPrefix(name, "/") {
		return fmt.Errorf("sd_notify: NOTIFY_SOCKET %q is neither a path nor an abstract name", n.Socket)
	}
	addr := &net.UnixAddr{Name: name, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return fmt.Errorf("sd_notify: dial %q: %w", n.Socket, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("sd_notify: write %q: %w", n.Socket, err)
	}
	return nil
}

// Ready announces READY=1 (plus optional STATUS text).
func (n *Notifier) Ready(status string) error {
	msg := "READY=1"
	if status != "" {
		msg += "\nSTATUS=" + status
	}
	return n.Notify(msg)
}

// Watchdog pets the supervising systemd instance (WATCHDOG=1).
func (n *Notifier) Watchdog() error { return n.Notify("WATCHDOG=1") }

// Status updates the unit's human-readable STATUS field.
func (n *Notifier) Status(s string) error { return n.Notify("STATUS=" + s) }

// Stopping announces STOPPING=1 during shutdown.
func (n *Notifier) Stopping() error { return n.Notify("STOPPING=1") }
