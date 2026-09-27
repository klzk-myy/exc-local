// Shared-memory ring fault scenarios (L1 tier): invalid Aeron/shm buffer
// addresses and images must produce clean coded errors — never a panic,
// never a mapped-garbage ring that mutates state.

package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"

	"exchange/internal/ipc"
)

func scenarioShmFaults(_ context.Context, _ *env) *Checks {
	c := &Checks{}
	base := fmt.Sprintf("exch_fault_%d", os.Getpid())
	rm := func(name string) { _ = os.Remove("/dev/shm/" + name) }

	// --- invalid geometry: rejected before touching /dev/shm -----------------
	bad := []struct {
		name        string
		cap         uint32
		slotPayload uint32
	}{
		{"cap_not_pow2", 1000, 1024},
		{"cap_zero", 0, 1024},
		{"slot_not_64_multiple", 8, 100},
		{"slot_zero", 8, 0},
	}
	for _, k := range bad {
		err, panicked := panicGuard(func() error {
			r, err := ipc.OpenRing(base+"_geom", ipc.RoleProducer, true, k.cap, k.slotPayload)
			if r != nil {
				r.Close()
			}
			return err
		})
		c.okf(k.name+":clean_error", panicked == nil && err != nil,
			"err=%v panic=%v", err, panicked)
		rm(base + "_geom")
	}

	// --- truncated image: file smaller than the 320B header ------------------
	trunc := base + "_trunc"
	if err := os.WriteFile("/dev/shm/"+trunc, make([]byte, 100), 0o600); err == nil {
		err, panicked := panicGuard(func() error {
			r, err := ipc.OpenRing(trunc, ipc.RoleConsumer, false, 4096, 1024)
			if r != nil {
				r.Close()
			}
			return err
		})
		c.okf("truncated_image:clean_error", panicked == nil && err != nil,
			"err=%v panic=%v", err, panicked)
		rm(trunc)
	} else {
		c.ok("truncated_image:fixture", false, err.Error())
	}

	// --- corrupt magic / version: attach must reject --------------------------
	corrupt := base + "_corrupt"
	prod, err := ipc.OpenRing(corrupt, ipc.RoleProducer, true, 64, 64)
	switch {
	case err != nil:
		c.ok("corrupt_image:fixture", false, err.Error())
	default:
		_ = prod.Close()
		// Smash the version field (offset 260) in the persisted image.
		if f, ferr := os.OpenFile("/dev/shm/"+corrupt, os.O_RDWR, 0o600); ferr == nil {
			_, _ = f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 260)
			_ = f.Close()
		}
		err, panicked := panicGuard(func() error {
			r, err := ipc.OpenRing(corrupt, ipc.RoleConsumer, false, 64, 64)
			if r != nil {
				r.Close()
			}
			return err
		})
		c.okf("corrupt_image:badmagic_rejected",
			panicked == nil && stderrors.Is(err, ipc.ErrBadMagic),
			"err=%v panic=%v", err, panicked)
		rm(corrupt)
	}

	// --- corrupt slot len: Peek must fail closed to nil ----------------------
	slot := base + "_slot"
	prod, err = ipc.OpenRing(slot, ipc.RoleProducer, true, 8, 64)
	if err != nil {
		c.ok("corrupt_slot:fixture", false, err.Error())
	} else if cons, cerr := ipc.OpenRing(slot, ipc.RoleConsumer, false, 8, 64); cerr != nil {
		c.ok("corrupt_slot:consumer_attach", false, cerr.Error())
		_ = prod.Close()
		rm(slot)
	} else {
		if !prod.TryWrite([]byte("hello")) {
			c.ok("corrupt_slot:write_fixture", false, "TryWrite failed")
		}
		// Tamper the slot-0 len field (offset 320+4) to beyond slot payload.
		if f, ferr := os.OpenFile("/dev/shm/"+slot, os.O_RDWR, 0o600); ferr == nil {
			_, _ = f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, 324)
			_ = f.Close()
		}
		var peek []byte
		_, panicked := panicGuard(func() error {
			peek = cons.Peek()
			return nil
		})
		c.okf("corrupt_slot:peek_fail_closed", panicked == nil && peek == nil,
			"peek=%v panic=%v", peek, panicked)
		_ = cons.Close()
		_ = prod.Close()
		rm(slot)
	}

	// --- dead producer: ProducerAlive must report false (peer-death / -------
	// --- network-split fencing signal for the L1 election path) --------------
	dead := base + "_dead"
	prod, err = ipc.OpenRing(dead, ipc.RoleProducer, true, 8, 64)
	if err != nil {
		c.ok("dead_producer:fixture", false, err.Error())
	} else if cons, cerr := ipc.OpenRing(dead, ipc.RoleConsumer, false, 8, 64); cerr != nil {
		c.ok("dead_producer:attach", false, cerr.Error())
		_ = prod.Close()
		rm(dead)
	} else {
		c.okf("dead_producer:alive_while_open", cons.ProducerAlive(),
			"pid=%d", cons.ProducerPid())
		_ = prod.Close()
		// Stamp a definitely-dead pid (2^22 > default /proc/sys/kernel/pid_max)
		// so the check does not depend on the real producer exiting.
		if f, ferr := os.OpenFile("/dev/shm/"+dead, os.O_RDWR, 0o600); ferr == nil {
			var b [8]byte
			v := uint64(1 << 22)
			for i := 0; i < 8; i++ {
				b[i] = byte(v >> (8 * i))
			}
			_, _ = f.WriteAt(b[:], 192) // offPid
			_ = f.Close()
		}
		c.okf("dead_producer:dead_detected", !cons.ProducerAlive(),
			"ProducerAlive()=true for dead pid")
		_ = cons.Close()
		rm(dead)
	}

	return c
}
