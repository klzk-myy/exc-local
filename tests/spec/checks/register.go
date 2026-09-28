package checks

import spec "exchange-testspec/spec"

// RegisterAll registers every document-checkpoint implementation owned by
// this package. Golden corpus cases register separately (package golden);
// checkpoint IDs they satisfy are bound here.
func RegisterAll(r *spec.Registry) {
	registerPhase01(r)
	registerPhase015(r)
	registerPhase02(r)
}
