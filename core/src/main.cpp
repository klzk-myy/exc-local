// matching_engine — one process per shard (DESIGN.md: dedicated bare metal,
// single matching thread per shard). Task 1.3.1 scaffold: parse -shard <n>,
// wire core components, report readiness.

#include <cinttypes>
#include <cstdio>
#include <cstdlib>
#include <cstring>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "degradation/ModeManager.hpp"
#include "election/LeaderElection.hpp"
#include "health/HealthChecker.hpp"
#include "matching/MatchingEngine.hpp"
#include "recovery/RecoveryManager.hpp"
#include "risk/PreTradeChecker.hpp"
#include "utils/MemoryPool.hpp"
#include "utils/TimeUtils.hpp"

namespace {

void usage(const char* argv0) {
    std::fprintf(stderr, "usage: %s [-shard <n>]\n", argv0);
}

bool parse_shard(const char* s, uint32_t* out) {
    if (s == nullptr || *s == '\0' || *s == '-') return false;
    char* end = nullptr;
    const unsigned long v = std::strtoul(s, &end, 10);
    if (end == s || *end != '\0' || v > 0xFFFFul) return false;
    *out = static_cast<uint32_t>(v);
    return true;
}

}  // namespace

int main(int argc, char** argv) {
    uint32_t shard = 0;
    for (int i = 1; i < argc; ++i) {
        if (std::strcmp(argv[i], "-shard") == 0) {
            ++i;
            if (i >= argc || !parse_shard(argv[i], &shard)) {
                usage(argv[0]);
                return 2;
            }
        } else if (std::strcmp(argv[i], "-h") == 0 ||
                   std::strcmp(argv[i], "--help") == 0) {
            usage(argv[0]);
            return 0;
        } else {
            std::fprintf(stderr, "unknown argument: %s\n", argv[i]);
            usage(argv[0]);
            return 2;
        }
    }

    // Pre-allocated state: no heap traffic after this point (spec §3.6).
    exch::MemoryPool<exch::Order> orders(exch::kOrderPoolCapacity);
    exch::OrderBook book;
    exch::MatchingEngine engine(shard, book, orders);
    exch::PreTradeChecker risk;
    exch::ModeManager modes;
    exch::HealthChecker health(modes);
    exch::LeaderElection election(shard);
    exch::RecoveryManager recovery(shard);
    (void)engine; (void)risk; (void)health; (void)election; (void)recovery;

    std::printf("shard %" PRIu32 " initialized at %" PRIu64 " ns\n", shard,
                exch::now_ns());
    return 0;
}
