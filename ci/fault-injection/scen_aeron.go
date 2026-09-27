// Aeron fault scenario (L1 tier): a missing/bogus media-driver directory
// (invalid Aeron buffer address / unreachable driver) must surface a clean
// coded error — never a panic or a silent half-initialized client.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"exchange/internal/ipc/aeron"
)

func scenarioAeronUnreachable(_ context.Context, e *env) *Checks {
	c := &Checks{}

	// A CnC directory that cannot contain a live media driver.
	bogus := filepath.Join(e.workDir("aeron_unreachable"), "no-such-aeron-dir")
	if err := os.MkdirAll(bogus, 0o755); err != nil {
		c.ok("fixture_mkdir", false, err.Error())
		return c
	}

	var client *aeron.Client
	err, panicked := panicGuard(func() error {
		cl, err := aeron.Connect(bogus, 750) // 750ms driver timeout
		client = cl
		return err
	})
	c.okf("bad_dir:no_panic", panicked == nil, "panic=%v", panicked)
	c.okf("bad_dir:clean_error", err != nil, "err=%v", err)
	c.okf("bad_dir:no_client", client == nil, "client=%+v", client)
	if client != nil {
		client.Close()
	}

	// A directory with a garbage CnC-shaped file — not a real driver.
	garbage := filepath.Join(e.workDir("aeron_unreachable"), "garbage-cnc")
	_ = os.MkdirAll(garbage, 0o755)
	_ = os.WriteFile(filepath.Join(garbage, "cnc.dat"),
		[]byte{0xDE, 0xAD, 0xBE, 0xEF}, 0o644)
	err, panicked = panicGuard(func() error {
		cl, err := aeron.Connect(garbage, 750)
		if cl != nil {
			cl.Close()
		}
		return err
	})
	c.okf("garbage_cnc:fail_closed", panicked == nil && err != nil,
		"err=%v panic=%v", err, panicked)

	// Positive control is exercised by services' own aeron_test; here we only
	// assert the failure surface stays clean when the driver is absent.
	c.info("note", fmt.Sprintf("driver-timeout path verified; live-driver "+
		"round-trip is covered by internal/ipc/aeron tests (kept out of this "+
		"suite — no aeronmd on CI)"))

	return c
}
