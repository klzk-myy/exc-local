// exchange-soak is the Phase-02.5 engine soak load generator (Task 2.5.3.1).
// It lives OUTSIDE services/ so soak-harness dependencies never pollute the
// service module; it consumes the service module through the `replace`
// directive below.
//
// NOTE on visibility: the module path carries the `exchange/` prefix so that
// `exchange/internal/...` packages (Go internal-visibility rule: importers
// must share the parent path prefix) resolve from this module. Without the
// prefix, `exchange/internal/ipc` would be unimportable from tests/.
module exchange/tests/soak

go 1.26.8

require (
	exchange v0.0.0
	github.com/google/flatbuffers v1.12.1
)

require golang.org/x/sys v0.48.0 // indirect

replace exchange => ../../services
