// Task 1.3.5 — IPC channel abstraction (shared-memory transport).
//
// A Channel is a pair of SPSC rings under /dev/shm:
//
//	{base}_{shard}_in    gateway -> core   (OrderNew / OrderCancel)
//	{base}_{shard}_out   core -> gateway   (TradeFill / BookSnapshot)
//
// EndpointGateway = Go services side (produces _in, consumes _out);
// EndpointCore = the C++ matching engine (mirrored in
// core/src/ipc/SharedMemChannel.cpp). The C++ side is the image creator.
//
// Preferred transport is Aeron `aeron:ipc` (internal/ipc/aeron); this package
// is the mandated fallback (Task 1.3.5 §2).

package ipc

import "fmt"

const DefaultShmBase = "exchange_ipc"

// Endpoint selects which side of the channel this process is.
type Endpoint int

const (
	EndpointCore Endpoint = iota
	EndpointGateway
)

// InName / OutName return the shm object names ("/dev/shm/<name>").
func InName(base string, shard uint16) string  { return fmt.Sprintf("%s_%d_in", base, shard) }
func OutName(base string, shard uint16) string { return fmt.Sprintf("%s_%d_out", base, shard) }

// Channel binds the inbound and outbound rings of one shard.
type Channel struct {
	in  *Ring // {base}_{shard}_in  (gw -> core)
	out *Ring // {base}_{shard}_out (core -> gw)
	ep  Endpoint
}

// OpenChannel maps both rings. `create` should be true on exactly one side
// (convention: the C++ core creates; the Go gateway attaches).
func OpenChannel(base string, shard uint16, ep Endpoint, create bool,
	capacity, slotPayload uint32) (*Channel, error) {

	c := &Channel{ep: ep}
	inRole := RoleConsumer
	outRole := RoleProducer
	if ep == EndpointGateway {
		inRole = RoleProducer
		outRole = RoleConsumer
	}
	var err error
	if c.in, err = OpenRing(InName(base, shard), inRole, create, capacity, slotPayload); err != nil {
		return nil, err
	}
	if c.out, err = OpenRing(OutName(base, shard), outRole, create, capacity, slotPayload); err != nil {
		c.in.Close()
		return nil, err
	}
	return c, nil
}

// Send publishes one framed message toward the peer. false = ring full
// (backpressure — Drops() increments; map to ENGINE_OVERLOAD upstream).
func (c *Channel) Send(p []byte) bool {
	if c.ep == EndpointCore {
		return c.out.TryWrite(p)
	}
	return c.in.TryWrite(p)
}

// Peek returns the next inbound payload aliasing ring memory (zero-copy,
// valid until Consume). nil when drained.
func (c *Channel) Peek() []byte {
	if c.ep == EndpointCore {
		return c.in.Peek()
	}
	return c.out.Peek()
}

// Consume commits the Peek'd slot.
func (c *Channel) Consume() {
	if c.ep == EndpointCore {
		c.in.Consume()
		return
	}
	c.out.Consume()
}

// Poll copies the next inbound message into buf. 0 drained, -1 too small.
func (c *Channel) Poll(buf []byte) int {
	if c.ep == EndpointCore {
		return c.in.Poll(buf)
	}
	return c.out.Poll(buf)
}

// ProducerAlive probes the far-end producer's stamped pid (kill(pid,0)).
func (c *Channel) ProducerAlive() bool {
	if c.ep == EndpointCore {
		return c.in.ProducerAlive()
	}
	return c.out.ProducerAlive()
}

// Occupancy = pending unread inbound messages. Drops = refused outbound
// writes while the ring was full.
func (c *Channel) Occupancy() uint64 {
	if c.ep == EndpointCore {
		return c.in.Occupancy()
	}
	return c.out.Occupancy()
}
func (c *Channel) Drops() uint64 {
	if c.ep == EndpointCore {
		return c.out.Drops()
	}
	return c.in.Drops()
}

func (c *Channel) Close() error {
	e1 := c.in.Close()
	e2 := c.out.Close()
	if e1 != nil {
		return e1
	}
	return e2
}
