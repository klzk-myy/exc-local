// Package marketdata implements the Phase-06 public market-data WebSocket
// surface (spec §10) mounted at /ws/v1/marketdata:
//
//   - Task 6.3.1 — goroutine-per-connection WS server with the unified
//     §10.5 `action` grammar: subscribe/unsubscribe over the typed-channel
//     namespace (book@/bbo@/aggTrades@/depth@/kline@/openInterest@/
//     referencePrice@/liquidations@ + private:* auth-gated), 30s server
//     ping / 60s pong deadline, and the per-session class budgets
//     (20 L2-class channels, 5 L3-class channels, 200 total).
//   - Task 6.3.2 — L2 book distribution: DeltaSource → 100ms/100-event
//     conflator → fanout, per-symbol `md:seq:{symbol}` cursor mirrored to
//     Redis, top-20 levels per side, CRC32 depth checksum, and the
//     first_seq/last_seq/prev_last_seq contiguity envelope (§10.9).
//   - Task 6.3.9 — session resume: per-channel bounded ring buffer
//     (10,000 messages or 60s, whichever is smaller) replayed from
//     last_seq+1; out-of-range cursors get an explicit resync directive
//     plus a snapshot frame when a SnapshotSource is wired.
//   - Task 6.3.5 — private order stream on /ws/v1/orders: JWT/API-key
//     auth via auth.Issuer / auth.SignatureVerifier, read-scope gate,
//     PublishPrivate fans out only to the bound AccountID, per-account
//     replay rings and per-(channel,account) seq domains
//     (md:seq:private:{channel}:{acct}) — account A can never observe
//     account B's frames, live or replayed.
//   - Task 6.3.10 — in-band request-response dispatcher: `request`
//     method routing (time/ping + registry for Wave-2) and `order.*`
//     trading actions through the ws.Dispatcher seam with the shared 60s
//     request_id dedup window and 500ms CORE_TIMEOUT.
//   - Task 6.3.15 — parameterized depth@{symbol}:{levels}:{cadence}
//     multiplexing (spec §24 #265): the conflator slices its internal
//     20-level state into subscribed {5,10,20}×{100,250,1000} variants,
//     each on its own cadence with its own prev_last_seq chain and a
//     CRC32 over the emitted slice; bare depth@{symbol} stays the 20:100
//     default.
//   - Task 6.3.17 — referencePrice@{symbol}: RefPriceStream consumes the
//     Phase-19.5 Price Oracle seam (ReferencePriceSource +
//     ExecutionRuleSource), surfaces price, provenance, collars and
//     expiry, and fails closed past the 5s staleness gate — stale frames
//     carry no usable price fields.
//   - Task 6.3.22 — durability & entitlements: md:seq high-water mirrors
//     (public + private domains), the bounded durable gap journal
//     (md:gaps:{key}, restart boundaries / horizon misses / invalid
//     cursors / input saturation), and the EntitlementChecker seam
//     enforcing symbol-level access with ENTITLEMENT_REQUIRED.
//   - Task 6.3.24 — resynchronization: client-initiated {"action":
//     "resync"} by channel or symbol, resync directive → snapshot →
//     ordered live continuation with prev_last_seq continuity, CRC32
//     slice verification on depth variants, and private-ring-aware
//     replay.
//   - Task 7.3.9 (consumer side) — lpBook@{lpID}/{symbol} per-LP book
//     distribution: LPQuoteSource → LPPriceFilter applies the persisted
//     lp_instrument_configs markup/skew, gates on enabled + LP ACTIVE +
//     per-instrument staleness_timeout_ms (5s default), withdraws dead
//     books as stale frames, and survives config edits via interval
//     refresh (lp_pricing.go).
//
// Why a dedicated server instead of mounting internal/ws.Server: the ws
// package (built in Phase-05 for the gateway's /ws/v1 interactive
// surface) keeps its connection loop, subscribe handler and resume stub
// entirely package-private — there is no exported hook for per-class
// subscription budgets, ring-buffer resume replay, or a `request` method
// router, and internal/ws is owned by a concurrent workstream. This
// package therefore implements the same §10.5 wire contract
// (frame shapes, error envelope field order, close codes) while reusing
// every exported ws seam — ws.Session, ws.PrivateChannels, ws.Dispatcher,
// ws.DedupStore, ws.WSConnCap, ws.PayloadHash/DedupKey and the registered
// RFC 6455 close codes — so the two surfaces stay protocol-identical.
//
// Wave-2 producer seams (trades/ticker/BBO/aggTrades/liquidations/OI/
// stats/referencePrice/kline@): producers call Server.Publish(channel,
// seq, data) with a channel-scoped sequence; snapshot fallback is wired
// per channel type via Server.SetSnapshotSource; engine feeds implement
// DeltaSource (IPC ring adapter + JetStream byte-source adapter provided).
package marketdata
