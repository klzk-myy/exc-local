// Aeron wiring for the FIX gateway process. Unlike the REST gateway's
// SPSC shm rings (one writer per shard, orders.service owns the ring),
// aeron:ipc?alias=orders_in is multi-publisher-safe: every gateway
// process owns its own publication onto the same stream. That is what
// lets FIX run as a separate binary without partitioning shm rings.
//
//	orders_in  (stream 1001) — venue publications: order commands
//	orders_out (stream 1002) — engine events: fills/cancels (OutFeed)
package fix

import (
	"context"
	"fmt"
	"sync/atomic"

	"exchange/internal/ipc"
	aeronclient "exchange/internal/ipc/aeron"
	excerrors "exchange/pkg/errors"
)

// Canonical aeron:ipc stream identifiers (spec §9.2 / doc.go map).
const (
	OrdersInURI     = "aeron:ipc?alias=orders_in"
	OrdersInStream  = int32(1001)
	OrdersOutURI    = "aeron:ipc?alias=orders_out"
	OrdersOutStream = int32(1002)
)

// AeronSubmitter implements orders.Submitter over an aeron:ipc
// publication. Offer failures surface the canonical service codes —
// back-pressure → ENGINE_OVERLOAD, disconnected → SERVICE_DEGRADED —
// so Submit's L2 abort path rejects the order coherently.
type AeronSubmitter struct {
	pub *aeronclient.Publication
}

// NewAeronSubmitter wraps an established publication.
func NewAeronSubmitter(pub *aeronclient.Publication) *AeronSubmitter {
	return &AeronSubmitter{pub: pub}
}

// Send publishes one FlatBuffers event payload. shard is ignored — the
// engine derives routing from the command's symbol (it does for the shm
// ring only to pick the ring; on aeron the envelope carries it).
func (s *AeronSubmitter) Send(_ context.Context, _ uint16, payload []byte) error {
	pos := s.pub.Offer(payload)
	switch {
	case pos > 0:
		return nil
	case pos == aeronclient.OfferBackPressured:
		return excerrors.New("ENGINE_OVERLOAD", "aeron offer back-pressured")
	default:
		return excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("aeron offer failed (%d)", pos))
	}
}

// Channel reports unsupported — FIX order entry goes through Send only;
// the shm ring is the REST gateway's binding.
func (s *AeronSubmitter) Channel(uint16) (*ipc.Channel, error) {
	return nil, fmt.Errorf("fix: shm channel unsupported on aeron submitter")
}

// AeronFeedSource adapts *aeron.Subscription to FragmentSource — the
// proven settlement.AeronSource pattern: the subscription's handler
// swaps to the caller's deliver for the duration of one Poll.
type AeronFeedSource struct {
	Sub     *aeronclient.Subscription
	deliver atomic.Pointer[func([]byte)]
}

// NewAeronFeedSource wraps sub; pass AeronFeedSource.Handler() as the
// FragmentHandler at AddSubscription time.
func NewAeronFeedSource(sub *aeronclient.Subscription) *AeronFeedSource {
	return &AeronFeedSource{Sub: sub}
}

// Handler is the FragmentHandler to register at AddSubscription time.
// Fragments arriving outside Pump are dropped — Poll only runs inside it.
func (s *AeronFeedSource) Handler() aeronclient.FragmentHandler {
	return func(buf []byte) {
		defer func() { _ = recover() }() // never panic across the cgo boundary
		if d := s.deliver.Load(); d != nil {
			(*d)(buf)
		}
	}
}

// Pump implements FragmentSource.
func (s *AeronFeedSource) Pump(limit int, deliver func(payload []byte)) int {
	if s.Sub == nil {
		return -1
	}
	s.deliver.Store(&deliver)
	defer s.deliver.Store(nil)
	return s.Sub.Poll(limit)
}

// OpenAeron connects the process to the media driver (dir per config,
// default aeron-dir per AeronDriverConfig) — shared with the shm
// fallback's conventions.
func OpenAeron(dir string, timeoutMs uint64) (*aeronclient.Client, error) {
	return aeronclient.Connect(dir, timeoutMs)
}
