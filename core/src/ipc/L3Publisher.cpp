// PHASE-17 TASK-17.3.1 — L3 order-level publisher. See header for the
// journal-first / pseudonym / hot-path contract.

#include "ipc/L3Publisher.hpp"

#include <cstdlib>

namespace exch {

L3Publisher::L3Publisher(IpcChannel* out, std::size_t builder_capacity,
                         std::size_t symbol_cap) noexcept
    : out_(out), sym_cap_(symbol_cap)
#if EXCH_L3_FLATBUFFERS
    // All hot-path state is allocated up front (spec §3.6): the builder
    // reserves its arena once; publish() is allocation-free thereafter.
    , builder_(builder_capacity)
#else
    // No wire path without generated headers — capacity is unused.
#endif
{
    (void)builder_capacity;
    if (sym_cap_ != 0) {
        auto* mem = static_cast<SymSlot*>(
            std::calloc(sym_cap_, sizeof(SymSlot)));
        if (mem == nullptr) {
            sym_cap_ = 0;          // saturated from the start -> all drops
        } else {
            for (std::size_t i = 0; i < sym_cap_; ++i)
                mem[i].instrument_id = kEmptyIid;
            sym_ = mem;
        }
    }
}

L3Publisher::~L3Publisher() { std::free(sym_); }

uint64_t L3Publisher::hash_account(uint64_t account_id) noexcept {
    // FNV-1a-64 over salt || account_id little-endian — matches the
    // fingerprint family used by wal_audit (FNV-1a, no external deps).
    uint64_t h = 14695981039346656037ull;
    const auto mix = [&h](uint8_t b) noexcept {
        h ^= b;
        h *= 1099511628211ull;
    };
    for (const char* p = kAccountHashSalt; *p != '\0'; ++p)
        mix(static_cast<uint8_t>(*p));
    for (unsigned i = 0; i < 8; ++i)
        mix(static_cast<uint8_t>((account_id >> (i * 8)) & 0xffu));
    return h;
}

uint64_t L3Publisher::symbol_seq(uint32_t iid) const noexcept {
    if (sym_ == nullptr || sym_cap_ == 0) return 0;
    std::size_t i = static_cast<std::size_t>(iid) % sym_cap_;
    for (std::size_t probe = 0; probe < sym_cap_; ++probe, i = (i + 1) % sym_cap_) {
        if (sym_[i].instrument_id == kEmptyIid) return 0;
        if (sym_[i].instrument_id == iid) return sym_[i].seq;
    }
    return 0;
}

uint64_t L3Publisher::next_seq(uint32_t iid) noexcept {
    if (sym_ == nullptr || sym_cap_ == 0) return UINT64_MAX;
    std::size_t i = static_cast<std::size_t>(iid) % sym_cap_;
    std::size_t free_i = sym_cap_;
    for (std::size_t probe = 0; probe < sym_cap_;
         ++probe, i = (i + 1) % sym_cap_) {
        if (sym_[i].instrument_id == iid) return sym_[i].seq++;
        if (sym_[i].instrument_id == kEmptyIid && free_i == sym_cap_)
            free_i = i;            // remember first free slot, keep probing
    }
    if (free_i == sym_cap_) return UINT64_MAX;   // table full
    // Wire contract: per-instrument seqs are 1-BASED — the Go decode gate
    // (services/internal/ipc/l3.go) rejects l3_seq==0 as "field absent",
    // so the first emitted event carries seq 1.
    sym_[free_i].instrument_id = iid;
    sym_[free_i].seq = 2;                        // seq 1 assigned now
    ++sym_live_;
    return 1;
}

#if EXCH_L3_FLATBUFFERS

bool L3Publisher::publish(const L3Event& e) noexcept {
    // Consume the instrument seq up front: a send failure must leave a
    // hole so a live consumer sees a gap rather than silently missing the
    // mutation (spec §11.1 gap semantics).
    const uint64_t sseq = next_seq(e.instrument_id);
    if (sseq == UINT64_MAX) {
        ++drops_;
        ++seq_overflow_;
        return false;
    }
    if (out_ == nullptr) {         // degrade: count, never send
        ++drops_;
        return false;
    }

    builder_.Clear();
    const auto ev = exc::wire::CreateL3OrderEvent(
        builder_, e.instrument_id,
        static_cast<exc::wire::L3EventKind>(e.kind),
        e.order_id, hash_account(e.account_id),
        e.side == Side::BUY ? exc::wire::Side_Buy : exc::wire::Side_Sell,
        e.price_ticks, e.ref_price_ticks, e.qty_units, e.qty_delta,
        sseq, e.wal_seq, e.trade_id, e.fill_role, e.flags, e.cancel_reason);
    exc::wire::EventBuilder eb(builder_);
    eb.add_seq(++pub_seq_);
    eb.add_ts(e.ts_ns);
    eb.add_type_type(exc::wire::EventType_L3OrderEvent);
    eb.add_type(ev.Union());
    builder_.Finish(eb.Finish());
    const bool ok = out_->send(builder_.GetBufferPointer(),
                               builder_.GetSize());
    if (ok) ++published_; else ++drops_;
    return ok;
}

#else

bool L3Publisher::publish(const L3Event& e) noexcept {
    // Without FlatBuffers headers there is no wire path: still consume the
    // instrument seq so replay/determinism accounting stays identical.
    const uint64_t sseq = next_seq(e.instrument_id);
    ++drops_;
    if (sseq == UINT64_MAX) ++seq_overflow_;
    return false;
}

#endif

}  // namespace exch
