// exchange-integration is the Phase-08 Task 8.3.1 end-to-end integration
// suite. It lives OUTSIDE services/ so its harness dependencies never
// pollute the service module (same convention as tests/spec).
//
// NOTE on visibility: `exchange/internal/...` packages are internal to the
// services module and CANNOT be imported here. Tests exercise them by:
//   (a) spawning the real service binaries (services/cmd/*),
//   (b) delegating to the services' own integration tests via
//       `go test -run` subprocesses inside services/,
//   (c) driving the C++ core via ctest / gtest / the matching_engine
//       binary.
// `exchange/pkg/...` packages are importable directly.
module exchange-integration

go 1.26.8

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/redis/go-redis/v9 v9.7.3
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v1.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace exchange => ../../services

replace github.com/jackc/pgservicefile => github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761
