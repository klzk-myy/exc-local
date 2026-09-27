// Task 1.3.5 — Aeron `aeron:ipc` transport over the vendored C++ client
// (third_party/aeron/lib/libaeron_client.a). The conductor runs via an
// AgentInvoker pumped inside send()/poll() — no extra thread on the hot path.
//
// Fail-closed: any Aeron exception or missing media driver leaves the channel
// closed (open()==false, send()==false, poll()==0).

#include "ipc/AeronChannel.hpp"

#if EXCH_WITH_AERON
#include <Aeron.h>
#include <ExclusivePublication.h>
#include <FragmentAssembler.h>
#include <Subscription.h>
#include <chrono>
#include <thread>
#endif

#include <cstdlib>
#include <cstring>
#include <time.h>

namespace exch {

namespace {
int64_t mono_ns() noexcept {
    timespec ts {};
    ::clock_gettime(CLOCK_MONOTONIC, &ts);
    return static_cast<int64_t>(ts.tv_sec) * 1'000'000'000LL + ts.tv_nsec;
}
}  // namespace

struct AeronChannel::Impl {
#if EXCH_WITH_AERON
    std::shared_ptr<aeron::Aeron> aeron;
    std::shared_ptr<aeron::Subscription> sub;
    std::shared_ptr<aeron::ExclusivePublication> pub;

    bool connect(const AeronChannelConfig& cfg, bool& open_flag) noexcept {
        try {
            aeron::Context ctx;
            if (!cfg.aeron_dir.empty()) {
                ctx.aeronDir(cfg.aeron_dir);
            } else if (const char* d = std::getenv("AERON_DIR")) {
                ctx.aeronDir(d);
            }
            ctx.clientName("exch-core");
            // Bound the CnC-file wait so a missing driver fails fast.
            ctx.mediaDriverTimeout(static_cast<long>(cfg.connect_timeout_ms));
            ctx.useConductorAgentInvoker(true);  // we pump the conductor
            aeron = aeron::Aeron::connect(ctx);
            auto& invoker = aeron->conductorAgentInvoker();
            invoker.start();

            const int64_t deadline_ms =
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    std::chrono::steady_clock::now().time_since_epoch()).count()
                + cfg.connect_timeout_ms;

            const int64_t sub_reg =
                aeron->addSubscription(cfg.in_uri, cfg.in_stream_id);
            const int64_t pub_reg =
                aeron->addExclusivePublication(cfg.out_uri, cfg.out_stream_id);

            while (!sub || !pub) {
                invoker.invoke();
                if (!sub) sub = aeron->findSubscription(sub_reg);
                if (!pub) pub = aeron->findExclusivePublication(pub_reg);
                if (std::chrono::duration_cast<std::chrono::milliseconds>(
                        std::chrono::steady_clock::now().time_since_epoch())
                        .count() > deadline_ms) {
                    return false;
                }
                std::this_thread::sleep_for(std::chrono::milliseconds(1));
            }
            open_flag = true;
            return true;
        } catch (...) {
            open_flag = false;
            return false;
        }
    }

    void do_work() noexcept {
        try {
            if (aeron) aeron->conductorAgentInvoker().invoke();
        } catch (...) {
            // Conductor errors (e.g. driver timeout) are surfaced through
            // is_open()/send() failure paths; never propagate out of noexcept.
        }
    }
#endif
};

AeronChannel::AeronChannel(std::string_view in_uri, std::string_view out_uri) {
    cfg_.in_uri = in_uri;
    cfg_.out_uri = out_uri;
}

AeronChannel::AeronChannel(AeronChannelConfig cfg) : cfg_(std::move(cfg)) {}

AeronChannel::~AeronChannel() { close(); }

bool AeronChannel::open() noexcept {
#if EXCH_WITH_AERON
    close();
    impl_ = std::make_unique<Impl>();
    if (!impl_->connect(cfg_, open_)) {
        impl_.reset();
        return false;
    }
    return true;
#else
    return false;
#endif
}

void AeronChannel::close() noexcept {
#if EXCH_WITH_AERON
    if (impl_) {
        try {
            impl_->sub.reset();
            impl_->pub.reset();
            impl_->aeron.reset();
        } catch (...) {
        }
    }
#endif
    impl_.reset();
    open_ = false;
}

bool AeronChannel::send(const void* data, uint32_t len) noexcept {
#if EXCH_WITH_AERON
    if (!open_ || !impl_ || !impl_->pub)
        return false;
    try {
        impl_->do_work();
        aeron::AtomicBuffer buf;
        buf.wrap(const_cast<uint8_t*>(static_cast<const uint8_t*>(data)), len);
        const int64_t deadline = mono_ns() + cfg_.offer_retry_ns;
        for (;;) {
            const int64_t pos = impl_->pub->offer(buf, 0, static_cast<int32_t>(len));
            if (pos > 0)
                return true;
            if (pos == aeron::MAX_POSITION_EXCEEDED ||
                pos == aeron::PUBLICATION_CLOSED)
                return false;
            // NOT_CONNECTED / BACK_PRESSURED / ADMIN_ACTION: bounded retry.
            if (mono_ns() >= deadline)
                return false;
        }
    } catch (...) {
        return false;
    }
#else
    (void)data; (void)len;
    return false;
#endif
}

int32_t AeronChannel::poll(void* buf, uint32_t buf_cap) noexcept {
    struct Ctx {
        uint8_t* buf; uint32_t cap; int32_t len;
    } ctx { static_cast<uint8_t*>(buf), buf_cap, -1 };
    const int frags = poll_each(
        [](void* c, const uint8_t* data, uint32_t len) {
            auto* p = static_cast<Ctx*>(c);
            if (len <= p->cap) {
                std::memcpy(p->buf, data, len);
                p->len = static_cast<int32_t>(len);
            } else {
                p->len = -2;  // caller buffer too small
            }
        },
        &ctx, 1);
    if (frags <= 0)
        return static_cast<int32_t>(frags);
    return ctx.len;
}

int AeronChannel::poll_each(fragment_sink_t handler, void* ctx,
                            int fragment_limit) noexcept {
#if EXCH_WITH_AERON
    if (!open_ || !impl_ || !impl_->sub || handler == nullptr)
        return 0;
    try {
        impl_->do_work();
        aeron::FragmentAssembler assembler(
            [handler, ctx](const aeron::AtomicBuffer& buffer,
                           aeron::util::index_t offset,
                           aeron::util::index_t length,
                           const aeron::Header&) {
                handler(ctx, buffer.buffer() + offset,
                        static_cast<uint32_t>(length));
            });
        return impl_->sub->poll(assembler.handler(), fragment_limit);
    } catch (...) {
        open_ = false;  // conductor/driver failure — fail closed
        return -1;
    }
#else
    (void)handler; (void)ctx; (void)fragment_limit;
    return 0;
#endif
}

bool AeronChannel::publisher_connected() const noexcept {
#if EXCH_WITH_AERON
    try {
        return impl_ && impl_->pub && impl_->pub->isConnected();
    } catch (...) {
        return false;
    }
#else
    return false;
#endif
}

bool AeronChannel::subscriber_connected() const noexcept {
#if EXCH_WITH_AERON
    try {
        return impl_ && impl_->sub && impl_->sub->isConnected();
    } catch (...) {
        return false;
    }
#else
    return false;
#endif
}

}  // namespace exch
