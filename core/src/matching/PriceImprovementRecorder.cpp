// Task 2.3.22 — PriceImprovementRecorder translation unit.
// The component is fully inline in matching/PriceImprovementRecorder.hpp
// (pure int64 math + POD counters; nothing worth a call boundary on the
// matching hot path). This TU exists so the library carries the component
// symbolically and future cold-path members (report formatting, snapshot
// encode) have a home without touching the header's hot surface.

#include "matching/PriceImprovementRecorder.hpp"

namespace exch {

static_assert(sizeof(ImprovementStamp) == 56,
              "fill-record extension grew — watch POD drift (spec §3.1)");

}  // namespace exch
