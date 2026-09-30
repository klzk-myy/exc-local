package checks

import spec "exchange-testspec/spec"

// RegisterAll registers every document-checkpoint implementation owned by
// this package. Golden corpus cases register separately (package golden);
// checkpoint IDs they satisfy are bound here.
func RegisterAll(r *spec.Registry) {
	registerPhase01(r)
	registerPhase015(r)
	registerPhase02(r)
	registerPhase025(r)
	registerPhase03(r)
	registerPhase04(r)
	registerPhase045(r)
	registerPhase05(r)
	registerPhase06(r)
	registerPhase07(r)
	registerPhase08(r)
	registerPhase085(r)
	registerPhase09(r)
	registerPhase10(r)
	registerPhase11(r)
	registerPhase12(r)
	registerPhase13(r)
	registerPhase135(r)
	registerPhase14(r)
	registerPhase15(r)
	registerPhase16(r)
	registerPhase17(r)
	registerPhase18(r)
	registerPhase19(r)
	registerPhase195(r)
	registerPhase20(r)
	registerPhase21(r)
	registerPhase22(r)
}
