// exchange-load is the Phase-08 Task 8.3.2 load-test harness (tests/load/).
// It lives OUTSIDE services/ so harness dependencies never pollute the
// service module; it consumes the service module through the `replace`
// directive below (same module-path convention as tests/soak — the
// `exchange/` prefix makes `exchange/internal/...` importable).
module exchange/tests/load

go 1.26.8

require (
	exchange v0.0.0
	github.com/google/flatbuffers v1.12.1
	github.com/gorilla/websocket v1.5.3
)

require golang.org/x/sys v0.48.0 // indirect

replace exchange => ../../services
