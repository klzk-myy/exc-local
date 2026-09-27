// PHASE-02 STUB (Task 2.3.6) — probe evaluation + transitions land there.
#include "health/HealthChecker.hpp"

#include "degradation/ModeManager.hpp"

namespace exch {

HealthChecker::HealthChecker(ModeManager& modes) noexcept : modes_(modes) {}

HealthReport HealthChecker::check_once() const noexcept {
    (void)modes_;
    return HealthReport{true, true, true, true, true, 0};
}

}  // namespace exch
