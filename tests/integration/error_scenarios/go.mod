// exchange-error-scenarios is the Phase-08 Task 8.3.5 end-to-end error
// scenario & resilience suite (spec §2.7, §8.7, §24 #307).
//
// It lives OUTSIDE services/ (same trick as tests/soak and
// tests/chaos/tool) so harness dependencies never pollute the service
// module; the `replace` below resolves the service module, and the
// exchange/ module-path prefix satisfies Go's internal-visibility rule
// for exchange/internal/... imports.
module exchange/tests/integration/error_scenarios

go 1.26.8

require (
	exchange v0.0.0
	github.com/jackc/pgx/v5 v5.9.2
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/fxamacker/cbor/v2 v2.8.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/go-webauthn/webauthn v0.12.3 // indirect
	github.com/go-webauthn/x v0.1.20 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.2 // indirect
	github.com/google/flatbuffers v1.12.1 // indirect
	github.com/google/go-tpm v0.9.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/redis/go-redis/v9 v9.7.3 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/spf13/viper v1.21.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace exchange => ../../../services
