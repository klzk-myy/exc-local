// Task 2.3.2 — StopOrderTrigger: conditional-trigger queues for STOP and
// STOP_LIMIT orders (spec §3.2 #7, §3.7.5). Header-inline implementation —
// the component is MatchingEngine-internal and all ops are O(1)-amortized
// probes or intrusive list edits.

#include "matching/StopOrderTrigger.hpp"

namespace exch {
// All logic lives in the header (see file docstring).
}  // namespace exch
