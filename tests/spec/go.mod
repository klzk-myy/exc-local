// exchange-testspec is the spec validation harness module (Phase-01.5 Task
// 1.5.3.2). It lives OUTSIDE services/ so CI-harness dependencies never
// pollute the service module; it consumes the service module through the
// `replace` directive below.
//
// NOTE on visibility: `exchange/internal/...` packages are internal to the
// services module and CANNOT be imported here. Checkpoint implementations
// that exercise internal packages delegate via `go test -run` subprocesses
// against named tests (see spec/exec.go); `exchange/pkg/...` packages are
// imported directly.
module exchange-testspec

go 1.26.8

require (
	exchange v0.0.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/nats-io/nats.go v1.45.0
	github.com/redis/go-redis/v9 v9.7.3
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
	github.com/nats-io/nkeys v0.4.11 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace exchange => ../../services
