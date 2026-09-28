// Task 2.3.2 — IcebergManager: visible-slice bookkeeping for ICEBERG orders
// (spec §3.2 #6, §3.7.4). Implementation is header-inline: every method is a
// hot-path O(1) probe/index op and the component is MatchingEngine-internal.

#include "matching/IcebergManager.hpp"

namespace exch {
// All logic lives in the header (see file docstring).
}  // namespace exch
