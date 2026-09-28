// Task 2.3.26 — Discretionary Offset & Fill-and-Store (FAS) band
// evaluation (spec §6.11, §24 #405).
//
// The component is header-inline (same convention as StopOrderTrigger.hpp):
// eval()/validate() are pure integer arithmetic on registers — there is no
// state and no out-of-line code worth a call boundary on the intake path.
// This translation unit anchors the header in the exch_core build and keeps
// the registered CMake source list stable for the engine-integration wave.

#include "matching/DiscretionaryExecutor.hpp"

namespace exch {

// Code-string identity: the header's constexpr array is odr-safe inline;
// this anchor pins a single address so engine code comparing pointers
// against kRejectOffsetInvalid observes one identity.
static_assert(sizeof(DiscretionaryExecutor::kRejectOffsetInvalid) ==
              sizeof("DISCRETIONARY_OFFSET_INVALID"));

}  // namespace exch
