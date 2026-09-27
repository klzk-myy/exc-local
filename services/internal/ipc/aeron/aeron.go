// Package aeron is the Go endpoint of the Aeron IPC transport (Task 1.3.5):
// a thin cgo wrapper over the vendored Aeron C client
// (core/third_party/aeron/lib/libaeron_static.a, headers include/c).
//
// The Go gateway mirrors the C++ AeronChannel: it publishes on
// `aeron:ipc?alias=orders_in` (stream 1001) and subscribes
// `aeron:ipc?alias=orders_out` (stream 1002). Requires a media driver on the
// host (aeronmd); connect fails fast when the CnC file never appears.
//
// Zero-copy: subscription callbacks receive a slice aliasing the
// driver-mapped log buffer — valid only for the duration of the call.
// Offer() hands the message bytes straight to the publication log (single
// copy into the ring, no intermediate framing buffer).
//
// Concurrency: a Client's publications are not goroutine-safe for Offer
// (aeron_publication_offer is thread-safe internally, but keep one
// Publication per goroutine for cache-line hygiene); Poll must be called
// from a single goroutine per Subscription.

package aeron

/*
#cgo CFLAGS: -I${SRCDIR}/../../../../core/third_party/aeron/include/c
#cgo LDFLAGS: ${SRCDIR}/../../../../core/third_party/aeron/lib/libaeron_static.a -lpthread -ldl -lm -lrt

#include <aeronc.h>
#include <stdlib.h>
#include <string.h>

// Delivered fragments hop back into Go; clientd is a cgo.Handle passed as a
// plain integer (uintptr_t) so vet's unsafeptr check stays quiet. The
// //export'd symbol takes a plain void* (cgo can't emit const uint8_t*).
extern void excGoFragment(void *clientd, void *buffer, size_t length, aeron_header_t *header);

static void exc_fragment_trampoline(void *clientd, const uint8_t *buffer, size_t length, aeron_header_t *header) {
	excGoFragment(clientd, (void *)buffer, length, header);
}

static int exc_poll(aeron_subscription_t *sub, uintptr_t clientd_handle, size_t limit) {
	return aeron_subscription_poll(sub, exc_fragment_trampoline, (void *)clientd_handle, limit);
}

static int64_t exc_offer(aeron_publication_t *pub, const uint8_t *buf, size_t len) {
	return aeron_publication_offer(pub, buf, len, NULL, NULL);
}

static void exc_noop_image(void *clientd, aeron_subscription_t *s, aeron_image_t *i) {
	(void)clientd; (void)s; (void)i;
}

static int exc_add_subscription(aeron_async_add_subscription_t **async,
                                aeron_t *client, const char *uri, int32_t stream_id) {
	return aeron_async_add_subscription(async, client, uri, stream_id,
	                                    exc_noop_image, NULL, exc_noop_image, NULL);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime/cgo"
	"time"
	"unsafe"
)

// Offer results mirrored from aeron_publication_offer.
const (
	OfferNotConnected   int64 = -1
	OfferBackPressured  int64 = -2
	OfferAdminAction    int64 = -3
	OfferClosed         int64 = -4
	OfferMaxPosExceeded int64 = -5
)

var (
	ErrConnectTimeout = errors.New("aeron: timed out waiting for media driver")
	ErrClosed         = errors.New("aeron: client closed")
)

// Client owns one aeron_t (with its internal conductor thread). The context
// must outlive the client — aeron_init stores the context pointer, not a copy.
type Client struct {
	c   *C.aeron_t
	ctx *C.aeron_context_t
}

// Connect attaches to the media driver whose CnC file lives in `dir`
// ("" => $AERON_DIR or the client default /dev/shm/aeron-<user>).
func Connect(dir string, driverTimeoutMs uint64) (*Client, error) {
	var ctx *C.aeron_context_t
	if C.aeron_context_init(&ctx) < 0 {
		return nil, fmt.Errorf("aeron: context_init: %s", lastErr())
	}

	if dir != "" {
		cdir := C.CString(dir)
		defer C.free(unsafe.Pointer(cdir))
		C.aeron_context_set_dir(ctx, cdir)
	}
	if driverTimeoutMs > 0 {
		C.aeron_context_set_driver_timeout_ms(ctx, C.uint64_t(driverTimeoutMs))
	}
	cname := C.CString("exc-go")
	defer C.free(unsafe.Pointer(cname))
	C.aeron_context_set_client_name(ctx, cname) // context strdup()s it
	C.aeron_context_set_use_conductor_agent_invoker(ctx, false)

	var client *C.aeron_t
	if C.aeron_init(&client, ctx) < 0 {
		msg := lastErr()
		C.aeron_context_close(ctx)
		return nil, fmt.Errorf("aeron: init: %s", msg)
	}
	if C.aeron_start(client) < 0 {
		msg := lastErr()
		C.aeron_close(client)
		C.aeron_context_close(ctx)
		return nil, fmt.Errorf("aeron: start: %s", msg)
	}
	return &Client{c: client, ctx: ctx}, nil
}

// Close releases the client (subscriptions/publications must not be used
// afterwards), then the context it references.
func (c *Client) Close() {
	if c.c != nil {
		C.aeron_close(c.c)
		c.c = nil
	}
	if c.ctx != nil {
		C.aeron_context_close(c.ctx)
		c.ctx = nil
	}
}

// IsClosed reports whether the driver dropped the client (driver death).
func (c *Client) IsClosed() bool {
	if c.c == nil {
		return true
	}
	return bool(C.aeron_is_closed(c.c))
}

// ClientID is the driver-assigned correlation id.
func (c *Client) ClientID() int64 { return int64(C.aeron_client_id(c.c)) }

// Publication is a handle on one `aeron:...` channel+stream.
type Publication struct {
	p *C.aeron_publication_t
}

// AddPublication registers a publication and blocks until the driver
// acknowledges it or `timeout` elapses.
func (c *Client) AddPublication(uri string, streamID int32, timeout time.Duration) (*Publication, error) {
	if c.c == nil {
		return nil, ErrClosed
	}
	curi := C.CString(uri)
	defer C.free(unsafe.Pointer(curi))
	var async *C.aeron_async_add_publication_t
	if C.aeron_async_add_publication(&async, c.c, curi, C.int32_t(streamID)) < 0 {
		return nil, fmt.Errorf("aeron: add_publication: %s", lastErr())
	}
	deadline := time.Now().Add(timeout)
	var pub *C.aeron_publication_t
	for {
		r := C.aeron_async_add_publication_poll(&pub, async)
		if r == 1 {
			return &Publication{p: pub}, nil
		}
		if r < 0 {
			return nil, fmt.Errorf("aeron: add_publication_poll: %s", lastErr())
		}
		if time.Now().After(deadline) {
			return nil, ErrConnectTimeout
		}
		time.Sleep(time.Millisecond)
	}
}

// Offer publishes buf. Returns the new stream position (>0) or a negative
// Offer* code — caller applies backpressure policy on OfferBackPressured /
// OfferNotConnected.
func (p *Publication) Offer(buf []byte) int64 {
	if len(buf) == 0 {
		return int64(C.exc_offer(p.p, nil, 0))
	}
	return int64(C.exc_offer(p.p, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))))
}

// IsConnected reports whether a subscriber image is attached.
func (p *Publication) IsConnected() bool {
	return bool(C.aeron_publication_is_connected(p.p))
}

// FragmentHandler receives one assembled message per call. buf aliases
// driver memory — do not retain it.
type FragmentHandler func(buf []byte)

// Subscription is a handle on one inbound channel+stream.
type Subscription struct {
	s       *C.aeron_subscription_t
	handler FragmentHandler
	h       cgo.Handle
}

// AddSubscription registers a subscription delivering fragments to `handler`.
func (c *Client) AddSubscription(uri string, streamID int32, handler FragmentHandler,
	timeout time.Duration) (*Subscription, error) {
	if c.c == nil {
		return nil, ErrClosed
	}
	curi := C.CString(uri)
	defer C.free(unsafe.Pointer(curi))
	var async *C.aeron_async_add_subscription_t
	if C.exc_add_subscription(&async, c.c, curi, C.int32_t(streamID)) < 0 {
		return nil, fmt.Errorf("aeron: add_subscription: %s", lastErr())
	}
	deadline := time.Now().Add(timeout)
	var sub *C.aeron_subscription_t
	for {
		var s *C.aeron_subscription_t
		r := C.aeron_async_add_subscription_poll(&s, async)
		if r == 1 {
			sub = s
			break
		}
		if r < 0 {
			return nil, fmt.Errorf("aeron: add_subscription_poll: %s", lastErr())
		}
		if time.Now().After(deadline) {
			return nil, ErrConnectTimeout
		}
		time.Sleep(time.Millisecond)
	}
	s := &Subscription{s: sub, handler: handler}
	s.h = cgo.NewHandle(s)
	return s, nil
}

// Poll delivers up to `limit` fragments via the registered handler.
// Returns fragments consumed, or -1 on error.
func (s *Subscription) Poll(limit int) int {
	if s.s == nil {
		return -1
	}
	return int(C.exc_poll(s.s, C.uintptr_t(s.h), C.size_t(limit)))
}

// IsConnected reports whether at least one publisher image is attached.
func (s *Subscription) IsConnected() bool {
	return bool(C.aeron_subscription_is_connected(s.s))
}

// Close releases the Go handle; the driver resource is reaped with the client.
func (s *Subscription) Close() {
	if s.h != 0 {
		s.h.Delete()
		s.h = 0
	}
	s.s = nil
}

//export excGoFragment
func excGoFragment(clientd unsafe.Pointer, buffer unsafe.Pointer, length C.size_t, header *C.aeron_header_t) {
	s := cgo.Handle(uintptr(clientd)).Value().(*Subscription)
	b := unsafe.Slice((*byte)(buffer), int(length))
	s.handler(b)
}

func lastErr() string {
	m := C.aeron_errmsg()
	if m == nil {
		return "unknown"
	}
	return C.GoString(m)
}
