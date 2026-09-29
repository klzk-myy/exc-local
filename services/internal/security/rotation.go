// Package security implements the Phase-13.5 secrets subsystem:
//
//   - Task 13.5.3.5 (spec §24 #111): secret rotation policy — a typed
//     inventory (SecretClass → RotationPolicy), a SecretSource seam with a
//     real Vault HTTP-API client shape (VaultSource) and an explicitly
//     DEV-labeled file/env adapter (DevSource), a rotation scheduler that
//     tracks per-secret deadlines and drives rotation hooks, and a
//     kid-keyed JWT keyring that performs zero-downtime dual-key rotation
//     on top of auth.Issuer's existing keyring (auth/jwt.go owns the
//     verify mechanics; this package owns the rotation policy).
//   - Task 13.5.3.6 (spec §24 #213): the Go-side half of the bare-metal
//     secrets lifecycle — Vault dynamic DB credentials (1h TTL lease,
//     auto-renew) and a generic credential Swapper that rebuilds client
//     pools without process restart.
//   - Task 13.5.3.7 (spec §24 #314): drills_test.go fault-injects this
//     machinery (rotation-under-load, expired-credential replay, Vault
//     partition fail-closed).
//
// Fail-closed contract (spec §2.7): when a deployment requires a real
// secret store (EXC_SECRETS_REQUIRED=production, or the deployment env is
// production — callers pass required=), any required secret that cannot
// be resolved aborts the load with CONFIG_LOAD_FAILED. There is no
// fallback to defaults, to dev material, or to "keep going without the
// secret" — a service that cannot fetch its credentials does not boot.
//
// Error codes emitted here (CONFIG_LOAD_FAILED, SECRET_ROTATION_FAILED)
// are cited by the Phase-13.5 plan (§13.5.3.7) and queued for the §23
// registry pass owned by Phase-05 Task 5.3.21.
package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"exchange/internal/auth"
	"exchange/internal/observability"
	excerrors "exchange/pkg/errors"
)

// Codes emitted by this package. CONFIG_LOAD_FAILED is the boot-time
// fail-closed code the Phase-13.5 plan names (§13.5.3.7 secret-store
// partition drill); SECRET_ROTATION_FAILED marks a failed rotation
// attempt — the old material stays live and the secret goes OVERDUE.
const (
	CodeConfigLoadFailed     = "CONFIG_LOAD_FAILED"
	CodeSecretRotationFailed = "SECRET_ROTATION_FAILED"
	CodeSecretConfigInvalid  = "SECRET_CONFIG_INVALID"
	defaultRotationAge       = 90 * 24 * time.Hour // spec §24 #111: 90-day rotation
	defaultAlertWindow       = 14 * 24 * time.Hour // P2 within 14 days of expiry
	jwtOverlap               = 24 * time.Hour      // §13.5.3.5 item 5: JWT kid + 24h overlap
	tlsRenewBefore           = 30 * 24 * time.Hour // item 6: renew 30d pre-expiry
	dynamicLeaseTTL          = time.Hour           // item 3 (13.5.3.6): 1h TTL dynamic DB creds
	vaultHTTPTimeout         = 10 * time.Second
	maxSecretResponseBytes   = 1 << 20 // 1 MiB — KV payloads are small; bound the read
	leaseRenewAtFraction     = 2       // renew at TTL/2
	kvV2DataSegments         = "data"  // {mount}/data/{path}
)

// ---------------------------------------------------------------------------
// Secret inventory — Task 13.5.3.5 item 2.
// ---------------------------------------------------------------------------

// SecretClass is one row of the secret inventory taxonomy. Every class of
// secret material the platform holds is enumerated here; the deploy-side
// inventory (deploy/security/secret-inventory.md) maps each class to its
// storage location, owner and rotation mechanism.
type SecretClass string

const (
	ClassJWTSigningKey  SecretClass = "jwt_signing_key"  // HS256/RS256/EdDSA kid keyring
	ClassAPIKeyMaterial SecretClass = "api_key_material" // SecretBox data key + client HMAC secrets
	ClassDBCredential   SecretClass = "db_credential"    // Postgres (PgBouncer) logins
	ClassRedisPassword  SecretClass = "redis_password"   // coordination + cache AUTH
	ClassAeronToken     SecretClass = "aeron_auth_token" // IPC channel admission token
	ClassTLSCertificate SecretClass = "tls_certificate"  // ingress + FIX mTLS
	ClassBankingAPIKey  SecretClass = "banking_api_key"  // SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2
)

// RotationPolicy is the per-class rotation contract.
type RotationPolicy struct {
	// MaxAge is the hard rotation deadline: a secret whose RotatedAt is
	// older than MaxAge is OVERDUE. Default 90 days (spec §24 #111);
	// configurable per class via Registry.SetPolicy.
	MaxAge time.Duration
	// Overlap is the dual-credential acceptance window during which both
	// the outgoing and incoming material verify (zero-downtime rotation).
	// JWT: 24h. Stateless credentials rotated by reload use 0.
	Overlap time.Duration
	// RenewBefore is the renewal/alert lead: the scheduler reports
	// DUE_SOON once the deadline is within RenewBefore... see AlertWindow
	// on the Scheduler for the P2 paging lead (14d). RenewBefore is the
	// provisioning lead a class needs (TLS: 30d for ACME/cert-manager).
	RenewBefore time.Duration
	// Dynamic marks Vault dynamic-lease credentials (database/creds/*) —
	// they expire by lease TTL and rotate by re-issuance, not by MaxAge
	// bookkeeping alone. Lease TTL contract: 1h (Task 13.5.3.6 item 3).
	Dynamic bool
}

// effective fills zero fields with the class defaults.
func (p RotationPolicy) effective() RotationPolicy {
	if p.MaxAge <= 0 {
		p.MaxAge = defaultRotationAge
	}
	if p.RenewBefore <= 0 {
		p.RenewBefore = defaultAlertWindow
	}
	return p
}

// Registry maps each SecretClass to its RotationPolicy. Construct with
// DefaultRegistry (the spec-pinned table) and override via SetPolicy —
// the 90-day figure is the ceiling, never silently extended.
type Registry struct {
	mu       sync.RWMutex
	policies map[SecretClass]RotationPolicy
}

// DefaultRegistry returns the spec §24 #111 rotation table.
func DefaultRegistry() *Registry {
	r := &Registry{policies: map[SecretClass]RotationPolicy{}}
	r.policies[ClassJWTSigningKey] = RotationPolicy{
		MaxAge: defaultRotationAge, Overlap: jwtOverlap,
		RenewBefore: defaultAlertWindow,
	}
	r.policies[ClassAPIKeyMaterial] = RotationPolicy{
		MaxAge: defaultRotationAge, Overlap: 72 * time.Hour,
		RenewBefore: defaultAlertWindow,
	}
	r.policies[ClassDBCredential] = RotationPolicy{
		MaxAge: defaultRotationAge, RenewBefore: defaultAlertWindow,
		Dynamic: true, // Vault database/creds/* lease, 1h TTL
	}
	r.policies[ClassRedisPassword] = RotationPolicy{
		MaxAge: defaultRotationAge, RenewBefore: defaultAlertWindow,
		Dynamic: true,
	}
	r.policies[ClassAeronToken] = RotationPolicy{
		MaxAge: defaultRotationAge, Overlap: time.Hour,
		RenewBefore: defaultAlertWindow,
	}
	r.policies[ClassTLSCertificate] = RotationPolicy{
		MaxAge: 365 * 24 * time.Hour, // cert lifetime bound, CA-issued
		// cert-manager/ACME renews 30 days pre-expiry; the "rotation" is a
		// reissue, overlap is the cert chain's own validity window.
		RenewBefore: tlsRenewBefore,
	}
	r.policies[ClassBankingAPIKey] = RotationPolicy{
		MaxAge: defaultRotationAge, RenewBefore: defaultAlertWindow,
	}
	return r
}

// Policy resolves the effective policy for class — registered value with
// defaults filled, or the global default for unregistered classes.
func (r *Registry) Policy(class SecretClass) RotationPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.policies[class]; ok {
		return p.effective()
	}
	return RotationPolicy{}.effective()
}

// SetPolicy overrides a class policy. MaxAge is capped at the 90-day
// ceiling: a weaker policy is a defect, not a configuration (§2.7).
func (r *Registry) SetPolicy(class SecretClass, p RotationPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.MaxAge > defaultRotationAge && class != ClassTLSCertificate {
		return excerrors.New(CodeSecretConfigInvalid,
			"rotation max_age exceeds the 90-day ceiling for "+string(class))
	}
	r.policies[class] = p.effective()
	return nil
}

// ---------------------------------------------------------------------------
// Secret references and values
// ---------------------------------------------------------------------------

// SecretRef identifies one logical secret: what it is, where it lives in
// the source, and whether boot may proceed without it.
type SecretRef struct {
	Name     string      // canonical inventory name, e.g. "jwt-hs256-key"
	Class    SecretClass //
	Path     string      // logical path inside the source (KV: {mount}/data/{path})
	Key      string      // single property to extract ("" = whole payload)
	Required bool        // required secrets failing to load → CONFIG_LOAD_FAILED
}

// SecretValue is one resolved payload. Fields are never logged — the
// String/GoString surface deliberately renders metadata only.
type SecretValue struct {
	Ref       SecretRef
	Fields    map[string]string
	Version   int
	CreatedAt time.Time
}

func (v *SecretValue) String() string {
	if v == nil {
		return "<nil secret>"
	}
	return fmt.Sprintf("secret{name:%s class:%s version:%d fields:%d}",
		v.Ref.Name, v.Ref.Class, v.Version, len(v.Fields))
}

// Get returns one field — the common case for a Ref.Key extract.
func (v *SecretValue) Get(key string) (string, bool) {
	if v == nil {
		return "", false
	}
	s, ok := v.Fields[key]
	return s, ok
}

// ---------------------------------------------------------------------------
// SecretSource — the store seam
// ---------------------------------------------------------------------------

// SecretSource resolves static secrets. Production is VaultSource (the
// Vault HTTP API shape — no Vault binary is vendored); DevSource is the
// explicitly-labeled development adapter and is refused whenever the
// deployment requires a real store.
type SecretSource interface {
	// Name identifies the backend for logs/diagnostics — never a secret.
	Name() string
	// Read fetches the secret addressed by ref.
	Read(ctx context.Context, ref SecretRef) (*SecretValue, error)
}

// DynamicCredential is a leased credential (Vault database/creds/* or the
// Redis equivalent): short-lived, auto-renewed, revoked on shutdown.
type DynamicCredential struct {
	Username  string
	Password  string // never logged
	LeaseID   string
	TTL       time.Duration
	Renewable bool
	IssuedAt  time.Time
}

// LeaseSource issues/renews/revokes dynamic credentials. VaultSource
// implements it against database/creds/{role} + sys/leases/*.
type LeaseSource interface {
	Issue(ctx context.Context, role string) (*DynamicCredential, error)
	Renew(ctx context.Context, leaseID string, increment time.Duration) (time.Duration, error)
	Revoke(ctx context.Context, leaseID string) error
}

// ---------------------------------------------------------------------------
// VaultSource — real Vault HTTP API client shape.
//
// The client speaks only the endpoints the platform uses:
//   GET  /v1/{mount}/data/{path}      — KV v2 read
//   GET  /v1/{mount}/creds/{role}     — dynamic DB credential issue
//   PUT  /v1/sys/leases/renew         — lease renewal
//   PUT  /v1/sys/leases/revoke        — lease revoke
//
// Authentication: X-Vault-Token. On bare metal the token is re-read from
// TokenFile on every request, so vault-agent token renewals are picked up
// without a process restart (Task 13.5.3.6 item 1 contract).
// ---------------------------------------------------------------------------

// VaultConfig configures the Vault HTTP client.
type VaultConfig struct {
	Addr      string // e.g. "https://vault.exchange.internal:8200" — https required off-loopback
	Token     string // direct token (K8s SA flow resolves to this)
	TokenFile string // vault-agent sink path — preferred on bare metal
	Namespace string // optional X-Vault-Namespace (Vault Enterprise)
	Timeout   time.Duration
	Client    *http.Client // injectable (mTLS transport in production)
	MountKV   string       // KV v2 mount, default "secret"
	MountDB   string       // database secrets engine mount, default "database"
}

// VaultSource is the production SecretSource + LeaseSource.
type VaultSource struct {
	cfg VaultConfig
	hc  *http.Client
}

// NewVaultSource validates the config and returns the client. The Addr
// must be an absolute http(s) URL; non-loopback plaintext http:// is
// rejected — a token over cleartext is a leak (fail-closed).
func NewVaultSource(cfg VaultConfig) (*VaultSource, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.Addr))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, excerrors.New(CodeSecretConfigInvalid,
			"vault addr must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, excerrors.New(CodeSecretConfigInvalid,
			"vault addr scheme must be http or https")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Host) {
		return nil, excerrors.New(CodeSecretConfigInvalid,
			"vault addr over plaintext http is only permitted on loopback")
	}
	if strings.TrimSpace(cfg.Token) == "" && strings.TrimSpace(cfg.TokenFile) == "" {
		return nil, excerrors.New(CodeSecretConfigInvalid,
			"vault auth requires EXC_VAULT_TOKEN or a vault-agent token file")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = vaultHTTPTimeout
	}
	if cfg.MountKV == "" {
		cfg.MountKV = "secret"
	}
	if cfg.MountDB == "" {
		cfg.MountDB = "database"
	}
	hc := cfg.Client
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &VaultSource{cfg: cfg, hc: hc}, nil
}

func isLoopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Name implements SecretSource.
func (v *VaultSource) Name() string { return "vault-http" }

// token resolves the auth token: the agent sink file wins when configured
// (per-request re-read = seamless vault-agent token renewal).
func (v *VaultSource) token() (string, error) {
	if v.cfg.TokenFile != "" {
		b, err := os.ReadFile(v.cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("vault token file %s: %w", v.cfg.TokenFile, err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", fmt.Errorf("vault token file %s is empty", v.cfg.TokenFile)
		}
		return t, nil
	}
	return v.cfg.Token, nil
}

// do issues one authed Vault API request and returns the decoded JSON
// envelope. Vault's error body ({errors:[...]}) is surfaced verbatim —
// it contains operator-facing messages, never secret material.
func (v *VaultSource) do(ctx context.Context, method, path string, body io.Reader) (map[string]any, error) {
	tok, err := v.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(v.cfg.Addr, "/")+"/v1/"+path, body)
	if err != nil {
		return nil, fmt.Errorf("vault request build: %w", err)
	}
	req.Header.Set("X-Vault-Token", tok)
	if v.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.cfg.Namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSecretResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("vault %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("vault %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		// Write endpoints (sys/leases/revoke et al.) return 204 with an
		// empty body — a valid response, not a malformed one.
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("vault %s %s: malformed response: %w", method, path, err)
	}
	return doc, nil
}

// kvV2Doc is the KV v2 read envelope.
type kvV2Doc struct {
	Data struct {
		Data     map[string]string `json:"data"`
		Metadata struct {
			Version     int       `json:"version"`
			CreatedTime time.Time `json:"created_time"`
		} `json:"metadata"`
	} `json:"data"`
}

// Read fetches {mount}/data/{ref.Path}. KV v2 only — the versioned mount
// is the store of record (docs/ops/secrets-inventory.md §3 step 2).
func (v *VaultSource) Read(ctx context.Context, ref SecretRef) (*SecretValue, error) {
	doc, err := v.do(ctx, http.MethodGet, v.cfg.MountKV+"/"+kvV2DataSegments+"/"+ref.Path, nil)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(doc)
	var kv kvV2Doc
	if err := json.Unmarshal(raw, &kv); err != nil {
		return nil, fmt.Errorf("vault kv decode %s: %w", ref.Path, err)
	}
	if len(kv.Data.Data) == 0 {
		return nil, fmt.Errorf("vault kv %s: empty payload", ref.Path)
	}
	return &SecretValue{
		Ref:       ref,
		Fields:    kv.Data.Data,
		Version:   kv.Data.Metadata.Version,
		CreatedAt: kv.Data.Metadata.CreatedTime,
	}, nil
}

// dynCredDoc is the database/creds/{role} lease envelope.
type dynCredDoc struct {
	LeaseID       string `json:"lease_id"`
	LeaseDuration int64  `json:"lease_duration"` // seconds
	Renewable     bool   `json:"renewable"`
	Data          struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"data"`
}

// Issue fetches a dynamic credential: GET {dbmount}/creds/{role}.
// The 1h TTL is set server-side by the Vault role definition; anything
// longer than 2h is rejected as a contract violation (a forgotten
// `ttl=` is how "temporary" credentials become permanent).
func (v *VaultSource) Issue(ctx context.Context, role string) (*DynamicCredential, error) {
	doc, err := v.do(ctx, http.MethodGet, v.cfg.MountDB+"/creds/"+role, nil)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(doc)
	var dc dynCredDoc
	if err := json.Unmarshal(raw, &dc); err != nil {
		return nil, fmt.Errorf("vault creds decode role %s: %w", role, err)
	}
	if dc.Data.Username == "" || dc.Data.Password == "" {
		return nil, fmt.Errorf("vault creds role %s: empty username/password", role)
	}
	cred := &DynamicCredential{
		Username:  dc.Data.Username,
		Password:  dc.Data.Password,
		LeaseID:   dc.LeaseID,
		TTL:       time.Duration(dc.LeaseDuration) * time.Second,
		Renewable: dc.Renewable,
		IssuedAt:  time.Now(),
	}
	if cred.TTL <= 0 {
		cred.TTL = dynamicLeaseTTL // contract default, still bounded
	}
	if cred.TTL > 2*time.Hour {
		return nil, fmt.Errorf("vault creds role %s: TTL %s exceeds the 2h contract bound", role, cred.TTL)
	}
	return cred, nil
}

// Renew extends a lease: PUT sys/leases/renew. Returns the new TTL.
func (v *VaultSource) Renew(ctx context.Context, leaseID string, increment time.Duration) (time.Duration, error) {
	body, _ := json.Marshal(map[string]any{
		"lease_id":  leaseID,
		"increment": fmt.Sprintf("%ds", int64(increment.Seconds())),
	})
	doc, err := v.do(ctx, http.MethodPut, "sys/leases/renew", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	if d, ok := doc["lease_duration"].(float64); ok {
		return time.Duration(d) * time.Second, nil
	}
	return increment, nil
}

// Revoke releases a lease: PUT sys/leases/revoke.
func (v *VaultSource) Revoke(ctx context.Context, leaseID string) error {
	body, _ := json.Marshal(map[string]any{"lease_id": leaseID})
	_, err := v.do(ctx, http.MethodPut, "sys/leases/revoke", strings.NewReader(string(body)))
	return err
}

// ---------------------------------------------------------------------------
// DevSource — DEV-ONLY adapter.
//
// Reads secret material from a JSON file or process environment for
// development and CI drills. It is not a secrets store: no access
// control, no audit, no versioning. It refuses construction when the
// deployment requires production-grade secrets (required=true) — the
// fail-closed path drills exercise — and every value it returns is
// tagged Source "dev" so downstream auditing can prove dev material
// never reached a production consumer.
// ---------------------------------------------------------------------------

// DevSource is the explicitly DEV-labeled SecretSource. Do not use in
// production; Loader enforces the refusal when required=true.
type DevSource struct {
	file   string              // JSON file: {"<path>": {"<key>": "<value>"}}
	getenv func(string) string // env adapter (default os.Getenv)
	dev    bool                // always true — the marker
}

// NewDevFileSource reads secrets from a JSON file mapping
// path → {key: value}. Missing file = construction error (fail-closed:
// a dev adapter must not silently fabricate material either).
func NewDevFileSource(path string) (*DevSource, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dev secrets file %s: %w", path, err)
	}
	var probe map[string]map[string]string
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("dev secrets file %s: not a {path:{key:value}} JSON map: %w", path, err)
	}
	return &DevSource{file: path, getenv: os.Getenv, dev: true}, nil
}

// NewDevEnvSource reads EXC_DEV_SECRET_<NAME> variables.
// getenv is injectable for tests; nil uses os.Getenv.
func NewDevEnvSource(getenv func(string) string) *DevSource {
	if getenv == nil {
		getenv = os.Getenv
	}
	return &DevSource{getenv: getenv, dev: true}
}

// Name implements SecretSource — deliberately self-incriminating.
func (d *DevSource) Name() string { return "DEV-ONLY insecure adapter" }

// IsDev reports the adapter class — callers assert on this to prove a
// production boot never resolved through the dev seam.
func (d *DevSource) IsDev() bool { return d.dev }

// Read resolves ref.Path (file adapter) or EXC_DEV_SECRET_<NAME>
// (env adapter, NAME uppercased with -/_ → _). A missing secret is an
// error — the dev adapter fails closed too, just without audit.
func (d *DevSource) Read(_ context.Context, ref SecretRef) (*SecretValue, error) {
	if d.file != "" {
		b, err := os.ReadFile(d.file)
		if err != nil {
			return nil, fmt.Errorf("dev secrets file %s: %w", d.file, err)
		}
		var all map[string]map[string]string
		if err := json.Unmarshal(b, &all); err != nil {
			return nil, fmt.Errorf("dev secrets file %s: %w", d.file, err)
		}
		fields, ok := all[ref.Path]
		if !ok || len(fields) == 0 {
			return nil, fmt.Errorf("dev secrets file %s: no entry for path %q", d.file, ref.Path)
		}
		return &SecretValue{Ref: ref, Fields: fields, Version: 1, CreatedAt: time.Now()}, nil
	}
	envName := "EXC_DEV_SECRET_" +
		strings.NewReplacer("-", "_", "/", "_", ".", "_").
			Replace(strings.ToUpper(ref.Name))
	if v := d.getenv(envName); v != "" {
		return &SecretValue{Ref: ref,
			Fields:    map[string]string{envName: v},
			Version:   1,
			CreatedAt: time.Now()}, nil
	}
	return nil, fmt.Errorf("dev env secret %s not set", envName)
}

// ---------------------------------------------------------------------------
// Loader — the boot-time resolution contract
// ---------------------------------------------------------------------------

// Bundle is the result of one LoadSecrets pass.
type Bundle struct {
	Secrets map[string]*SecretValue // keyed by Ref.Name
	Missing []string                // optional refs that did not resolve
	Source  string                  // backend name
	Dev     bool                    // true iff the source is the dev adapter
}

// LoadSecrets resolves every ref. When required=true (production-grade
// secrets mandated — EXC_SECRETS_REQUIRED=production or a production
// deployment env), the outcome is all-or-nothing and fail-closed:
//
//   - a *DevSource is rejected outright (no dev material behind a
//     production contract);
//   - any ref failure — including a partitioned/unreachable source —
//     returns nil and a CONFIG_LOAD_FAILED-coded error. No partial
//     bundle, no default substitution, no keep-going.
//
// When required=false (dev/test), Required refs still fail closed with
// the same code; only non-required refs may land in Missing.
func LoadSecrets(ctx context.Context, src SecretSource, refs []SecretRef, required bool) (*Bundle, error) {
	if src == nil {
		return nil, excerrors.New(CodeConfigLoadFailed, "no secret source configured")
	}
	b := &Bundle{Secrets: map[string]*SecretValue{}, Source: src.Name()}
	if ds, ok := src.(*DevSource); ok {
		b.Dev = ds.IsDev()
		if required {
			return nil, excerrors.New(CodeConfigLoadFailed,
				"development secret adapter is forbidden when production secrets are required")
		}
	}
	for _, ref := range refs {
		v, err := src.Read(ctx, ref)
		if err != nil {
			if ref.Required || required {
				return nil, excerrors.Wrap(CodeConfigLoadFailed,
					"required secret "+ref.Name+" unavailable from "+src.Name(), err)
			}
			b.Missing = append(b.Missing, ref.Name)
			continue
		}
		b.Secrets[ref.Name] = v
	}
	sort.Strings(b.Missing)
	return b, nil
}

// SourceFromEnv builds the configured source from process env, and
// reports whether production-grade secrets are required:
//
//	EXC_SECRETS_SOURCE      vault | dev-file | dev-env   (default: vault when required, dev-env otherwise is WRONG — see below)
//	EXC_SECRETS_REQUIRED    production | development    (explicit override)
//	EXC_VAULT_ADDR          Vault base URL
//	EXC_VAULT_TOKEN         direct token
//	EXC_VAULT_TOKEN_FILE    vault-agent sink (preferred bare metal)
//	EXC_VAULT_NAMESPACE     optional
//	EXC_DEV_SECRETS_FILE    dev-file adapter JSON
//
// `deployEnv` is the deployment environment label (config.Environment).
// Required resolves to: EXC_SECRETS_REQUIRED explicit value, else
// production semantics for any env label outside the known non-prod set
// (mirrors config.IsProduction — unknown labels fail closed).
// There is NO default that silently picks a weaker store: when required
// and the source config is incomplete, construction fails.
func SourceFromEnv(deployEnv string) (SecretSource, bool, error) {
	required := secretsRequired(deployEnv)
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EXC_SECRETS_SOURCE"))) {
	case "", "vault":
		return vaultFromEnv(required)
	case "dev-file":
		if required {
			return nil, true, excerrors.New(CodeConfigLoadFailed,
				"EXC_SECRETS_SOURCE=dev-file with production secrets required")
		}
		f := os.Getenv("EXC_DEV_SECRETS_FILE")
		if f == "" {
			return nil, false, excerrors.New(CodeSecretConfigInvalid,
				"EXC_SECRETS_SOURCE=dev-file needs EXC_DEV_SECRETS_FILE")
		}
		src, err := NewDevFileSource(f)
		return src, false, err
	case "dev-env":
		if required {
			return nil, true, excerrors.New(CodeConfigLoadFailed,
				"EXC_SECRETS_SOURCE=dev-env with production secrets required")
		}
		return NewDevEnvSource(nil), false, nil
	default:
		return nil, required, excerrors.New(CodeSecretConfigInvalid,
			"unknown EXC_SECRETS_SOURCE")
	}
}

func vaultFromEnv(required bool) (SecretSource, bool, error) {
	cfg := VaultConfig{
		Addr:      os.Getenv("EXC_VAULT_ADDR"),
		Token:     os.Getenv("EXC_VAULT_TOKEN"),
		TokenFile: os.Getenv("EXC_VAULT_TOKEN_FILE"),
		Namespace: os.Getenv("EXC_VAULT_NAMESPACE"),
	}
	src, err := NewVaultSource(cfg)
	if err != nil {
		if !required {
			// Dev/test convenience: no Vault configured → dev-env adapter.
			// This is the ONLY implicit fallback and it is unreachable in
			// production (required=true short-circuits above).
			return NewDevEnvSource(nil), false, nil
		}
		return nil, true, excerrors.Wrap(CodeConfigLoadFailed,
			"vault secret source misconfigured", err)
	}
	return src, required, nil
}

// secretsRequired mirrors config.IsProduction's keyword set (kept local —
// importing internal/config would drag viper into this leaf package; the
// canonical copy lives at services/internal/config/config.go:64).
func secretsRequired(deployEnv string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EXC_SECRETS_REQUIRED"))) {
	case "production", "required", "1", "true":
		return true
	case "development", "dev", "0", "false":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(deployEnv)) {
	case "development", "dev", "staging", "stage", "test", "testing",
		"sandbox", "local", "ci":
		return false
	}
	return true // unknown/empty labels fail closed to production
}

// ---------------------------------------------------------------------------
// Scheduler — rotation deadlines, status, hooks
// ---------------------------------------------------------------------------

// RotateFunc performs the rotation of one record: fetch/issue new
// material at the source, distribute it to consumers, then update
// rec.Version/RotatedAt. Returning an error leaves the old material live
// and marks the record's LastError — rotation failures never destroy the
// working credential.
type RotateFunc func(ctx context.Context, src SecretSource, rec *SecretRecord) error

// SecretRecord is the scheduler's tracking state for one secret.
type SecretRecord struct {
	Ref       SecretRef
	Policy    RotationPolicy
	Version   int
	RotatedAt time.Time // last completed rotation (or first load)
	LastError string    // operator-facing failure text — never secret data
}

// RotationState is the per-secret health classification.
type RotationState string

const (
	StateOK        RotationState = "ok"        // deadline comfortably ahead
	StateDueSoon   RotationState = "due_soon"  // within the RenewBefore/alert window
	StateOverdue   RotationState = "overdue"   // past MaxAge — rotate now
	StateUnrotated RotationState = "unrotated" // never loaded/rotated
)

// SecretStatus is the snapshot exported to metrics and ops surfaces.
type SecretStatus struct {
	Name                string
	Class               SecretClass
	State               RotationState
	SecondsUntilExpiry  float64 // RotatedAt+MaxAge−now (negative = overdue)
	SecondsUntilRenewal float64 // RenewBefore lead crossing (negative = renew now)
	Version             int
	LastError           string
}

// Scheduler tracks every managed secret and runs rotation hooks on a
// fixed cadence (daily is plenty — the alert window is 14 days).
type Scheduler struct {
	reg         *Registry
	src         SecretSource
	now         func() time.Time
	alertWindow time.Duration
	mu          sync.RWMutex
	recs        map[string]*SecretRecord
	hooks       map[SecretClass]RotateFunc
}

// NewScheduler binds a registry + source. alertWindow<=0 uses the 14-day
// spec bound (Task 13.5.3.5 item 8: P2 within 14 days of expiry).
func NewScheduler(reg *Registry, src SecretSource, alertWindow time.Duration) *Scheduler {
	if reg == nil {
		reg = DefaultRegistry()
	}
	if alertWindow <= 0 {
		alertWindow = defaultAlertWindow
	}
	return &Scheduler{
		reg: reg, src: src, alertWindow: alertWindow,
		now: time.Now, recs: map[string]*SecretRecord{},
		hooks: map[SecretClass]RotateFunc{},
	}
}

// Track registers a secret under its class policy. rotatedAt is the last
// known rotation (Vault metadata.created_time is the honest source);
// zero means "never rotated" — tracked but flagged Unrotated.
func (s *Scheduler) Track(ref SecretRef, version int, rotatedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[ref.Name] = &SecretRecord{
		Ref: ref, Policy: s.reg.Policy(ref.Class),
		Version: version, RotatedAt: rotatedAt,
	}
}

// Untrack removes a secret (delisted class, retired credential).
func (s *Scheduler) Untrack(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, name)
}

// SetHook installs the rotation procedure for a class. A class without a
// hook is manual: the scheduler still reports state (alerts fire) but
// RotateDue skips it — a missing procedure is surfaced, not silently
// successful (docs/ops/secrets-inventory.md §3).
func (s *Scheduler) SetHook(class SecretClass, fn RotateFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks[class] = fn
}

// Assess computes current status for every tracked secret.
func (s *Scheduler) Assess() []SecretStatus {
	now := s.now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SecretStatus, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, s.statusOf(r, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Scheduler) statusOf(r *SecretRecord, now time.Time) SecretStatus {
	st := SecretStatus{Name: r.Ref.Name, Class: r.Ref.Class,
		Version: r.Version, LastError: r.LastError}
	if r.RotatedAt.IsZero() {
		st.State = StateUnrotated
		return st
	}
	deadline := r.RotatedAt.Add(r.Policy.MaxAge)
	renewAt := deadline.Add(-r.Policy.RenewBefore)
	st.SecondsUntilExpiry = deadline.Sub(now).Seconds()
	st.SecondsUntilRenewal = renewAt.Sub(now).Seconds()
	switch {
	case !now.Before(deadline):
		st.State = StateOverdue
	case !now.Before(deadline.Add(-s.alertWindow)) || !now.Before(renewAt):
		st.State = StateDueSoon
	default:
		st.State = StateOK
	}
	return st
}

// RotateDue runs hooks for every record at or past its renewal point.
// Dynamic-class secrets renew by lease (RotateDue is for the *rotation*
// bookkeeping — the LeaseSource renew loop handles the credential
// itself). Records without hooks are reported, not rotated.
func (s *Scheduler) RotateDue(ctx context.Context) []SecretStatus {
	now := s.now()
	var rotated []SecretStatus
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.RotatedAt.IsZero() {
			continue // unrotated secrets need provisioning, not rotation
		}
		renewAt := r.RotatedAt.Add(r.Policy.MaxAge - r.Policy.RenewBefore)
		if now.Before(renewAt) {
			continue
		}
		hook, ok := s.hooks[r.Ref.Class]
		if !ok {
			continue
		}
		if err := hook(ctx, s.src, r); err != nil {
			r.LastError = err.Error()
		} else {
			r.LastError = ""
			r.RotatedAt = now
		}
		rotated = append(rotated, s.statusOf(r, now))
	}
	return rotated
}

// MarkRotated records an externally-completed rotation (operator ran the
// class runbook — e.g. rotate-secrets.sh — rather than an in-process hook).
func (s *Scheduler) MarkRotated(name string, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[name]; ok {
		r.RotatedAt = s.now()
		r.Version = version
		r.LastError = ""
	}
}

// RegisterMetrics exports the per-secret deadline gauges consumed by
// deploy/prometheus/alerts.yml (SecretRotationDueSoon /
// SecretRotationOverdue). Pull-based: samples are computed at scrape
// time, so a wedged scheduler can never report stale "ok" values.
func (s *Scheduler) RegisterMetrics(reg *observability.Registry) {
	reg.VecFunc("secret_rotation_seconds_until_expiry",
		"Seconds until the secret's rotation deadline (RotatedAt+MaxAge); "+
			"negative = overdue. P2 alert fires below 14 days.",
		"gauge", func() []observability.PullSample {
			var out []observability.PullSample
			for _, st := range s.Assess() {
				out = append(out, observability.PullSample{
					Labels: []string{"secret", st.Name, "class", string(st.Class)},
					Value:  st.SecondsUntilExpiry,
				})
			}
			return out
		})
	reg.VecFunc("secret_rotation_state",
		"Rotation state per secret: 0=ok 1=due_soon 2=overdue 3=unrotated.",
		"gauge", func() []observability.PullSample {
			var out []observability.PullSample
			for _, st := range s.Assess() {
				var v float64
				switch st.State {
				case StateDueSoon:
					v = 1
				case StateOverdue:
					v = 2
				case StateUnrotated:
					v = 3
				}
				out = append(out, observability.PullSample{
					Labels: []string{"secret", st.Name, "class", string(st.Class)},
					Value:  v,
				})
			}
			return out
		})
}

// Run executes the rotation cadence until ctx cancels. Interval <=0
// uses 6h — frequent enough that an overdue secret pages within the same
// shift, cheap enough to run forever.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.RotateDue(ctx)
		}
	}
}

// ---------------------------------------------------------------------------
// JWTKeyring — zero-downtime kid rotation over auth.Issuer (item 4-5).
//
// auth.Issuer is constructed immutable: Parse/Issue only read its key
// map, so a published Issuer is race-free under concurrent load. Rotation
// therefore swaps whole Issuer snapshots via atomic.Pointer rather than
// mutating the keyring mid-flight — requests parsed under the old
// snapshot keep verifying through the 24h overlap because the new
// snapshot still carries the retiring kid.
// ---------------------------------------------------------------------------

// JWTKeyVersion is one keyring entry with its acceptance expiry.
type JWTKeyVersion struct {
	KID string
	Alg string // "HS256" | "RS256" | "EdDSA"

	// Exactly one material set, matching Alg.
	HMACSecret []byte
	RSAPriv    *rsa.PrivateKey
	RSAPub     *rsa.PublicKey
	EdPriv     ed25519.PrivateKey
	EdPub      ed25519.PublicKey

	NotAfter time.Time // verification cutoff (rotation time + overlap for predecessors)
}

// JWTKeyring manages kid-keyed rotation. The active (signing) key is the
// newest version; predecessors remain verifiable until NotAfter.
type JWTKeyring struct {
	issuer   string
	audience string
	ttl      time.Duration
	overlap  time.Duration
	now      func() time.Time

	mu   sync.Mutex // serializes rotation; reads go through the atomic
	vers []JWTKeyVersion
	cur  atomic.Pointer[auth.Issuer]
}

// NewJWTKeyring builds an empty ring. overlap<=0 uses the 24h spec value.
// The ring is unusable until the first AddKey — Issuer() returns nil
// before then (fail-closed: no key material, no tokens).
func NewJWTKeyring(issuer, audience string, ttl, overlap time.Duration) *JWTKeyring {
	if overlap <= 0 {
		overlap = jwtOverlap
	}
	return &JWTKeyring{issuer: issuer, audience: audience, ttl: ttl,
		overlap: overlap, now: time.Now}
}

// Issuer returns the current signing/verification snapshot — nil until
// the first key lands. Callers treat nil as unauthenticated-everything.
func (k *JWTKeyring) Issuer() *auth.Issuer { return k.cur.Load() }

// Rotate installs a new signing key. The outgoing key's acceptance window
// is clamped to now+overlap (24h spec bound); keys already past their
// cutoff are pruned from the new snapshot so a retired kid is rejected
// the moment its window closes — never extended by accident.
func (k *JWTKeyring) Rotate(v JWTKeyVersion) error {
	if v.KID == "" {
		return excerrors.New(CodeSecretConfigInvalid, "jwt key version requires a kid")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	kept := make([]JWTKeyVersion, 0, len(k.vers)+1)
	for _, old := range k.vers {
		if old.KID == v.KID {
			continue // same-kid replacement: retire the old material now
		}
		// Clamp the predecessor's cutoff: min(existing NotAfter, now+overlap).
		cutoff := now.Add(k.overlap)
		if !old.NotAfter.IsZero() && old.NotAfter.Before(cutoff) {
			cutoff = old.NotAfter
		}
		old.NotAfter = cutoff
		if old.NotAfter.After(now) {
			kept = append(kept, old)
		}
	}
	if v.NotAfter.IsZero() {
		v.NotAfter = now.Add(defaultRotationAge) // signing key ages out at MaxAge
	}
	kept = append(kept, v)
	if err := k.rebuildLocked(kept, now); err != nil {
		return err
	}
	k.vers = kept
	return nil
}

// rebuildLocked constructs a fresh immutable Issuer holding exactly the
// supplied live versions, then atomically publishes it. The newest
// version signs; every version verifies until its NotAfter.
func (k *JWTKeyring) rebuildLocked(vers []JWTKeyVersion, now time.Time) error {
	if len(vers) == 0 {
		return excerrors.New(CodeSecretConfigInvalid, "jwt keyring cannot be empty")
	}
	iss := auth.NewIssuer(k.issuer, k.audience, k.ttl)
	for i, v := range vers {
		active := i == len(vers)-1
		var err error
		switch v.Alg {
		case "HS256":
			err = iss.AddHMACKey(v.KID, v.HMACSecret, active)
		case "RS256":
			err = iss.AddRSAKey(v.KID, v.RSAPriv, v.RSAPub, active)
		case "EdDSA":
			err = iss.AddEd25519Key(v.KID, v.EdPriv, v.EdPub, active)
		default:
			err = excerrors.New(CodeSecretConfigInvalid,
				"unsupported jwt alg "+v.Alg)
		}
		if err != nil {
			return excerrors.Wrap(CodeSecretRotationFailed,
				"jwt keyring rebuild kid "+v.KID, err)
		}
	}
	k.cur.Store(iss)
	return nil
}

// Prune drops versions whose acceptance window has closed. The active
// (newest) version is always retained — pruning can never remove the
// signing key under the ring's feet.
func (k *JWTKeyring) Prune() {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if len(k.vers) == 0 {
		return
	}
	kept := make([]JWTKeyVersion, 0, len(k.vers))
	for _, v := range k.vers {
		if v.NotAfter.After(now) {
			kept = append(kept, v)
		}
	}
	// Always keep at least the newest version — pruning can never
	// remove the signing key under the ring's feet.
	if len(kept) == 0 {
		kept = append(kept, k.vers[len(k.vers)-1])
	}
	if len(kept) != len(k.vers) {
		if err := k.rebuildLocked(kept, now); err == nil {
			k.vers = kept
		}
	}
}

// ActiveKID returns the kid currently used for signing.
func (k *JWTKeyring) ActiveKID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.vers) == 0 {
		return ""
	}
	return k.vers[len(k.vers)-1].KID
}

// Versions reports the kid→NotAfter map (observability/debug; no key
// material is exposed).
func (k *JWTKeyring) Versions() map[string]time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]time.Time, len(k.vers))
	for _, v := range k.vers {
		out[v.KID] = v.NotAfter
	}
	return out
}

// JWTMaterialFromFields decodes a Vault KV payload into a key version:
//
//	kid                  — required
//	alg                  — HS256 | RS256 | EdDSA (default HS256)
//	hs256_b64            — base64 HMAC secret (>=32 bytes decoded)
//	rsa_private_pem      — PKCS8/PKCS1 PEM private key (signing key)
//	rsa_public_pem       — PKIX PEM public key (verify-only overlap entry)
//	ed25519_private_b64  — base64 ed25519.PrivateKey
//	ed25519_public_b64   — base64 ed25519.PublicKey
//	not_after            — RFC3339 verify cutoff (optional)
func JWTMaterialFromFields(f map[string]string) (JWTKeyVersion, error) {
	v := JWTKeyVersion{KID: f["kid"], Alg: f["alg"]}
	if v.KID == "" {
		return v, excerrors.New(CodeSecretConfigInvalid, "jwt material: missing kid")
	}
	if v.Alg == "" {
		v.Alg = "HS256"
	}
	var err error
	switch v.Alg {
	case "HS256":
		v.HMACSecret, err = base64.StdEncoding.DecodeString(f["hs256_b64"])
		if err != nil || len(v.HMACSecret) < 32 {
			return v, excerrors.New(CodeSecretConfigInvalid,
				"jwt material: hs256_b64 must decode to >=32 bytes")
		}
	case "RS256":
		if pem := f["rsa_private_pem"]; pem != "" {
			v.RSAPriv, err = parseRSAPrivate(pem)
			if err != nil {
				return v, err
			}
		}
		if pem := f["rsa_public_pem"]; pem != "" {
			v.RSAPub, err = parseRSAPublic(pem)
			if err != nil {
				return v, err
			}
		}
		if v.RSAPub == nil && v.RSAPriv == nil {
			return v, excerrors.New(CodeSecretConfigInvalid,
				"jwt material: RS256 needs rsa_private_pem or rsa_public_pem")
		}
	case "EdDSA":
		if s := f["ed25519_private_b64"]; s != "" {
			b, derr := base64.StdEncoding.DecodeString(s)
			if derr != nil || len(b) != ed25519.PrivateKeySize {
				return v, excerrors.New(CodeSecretConfigInvalid,
					"jwt material: ed25519_private_b64 must decode to 64 bytes")
			}
			v.EdPriv = ed25519.PrivateKey(b)
		}
		if s := f["ed25519_public_b64"]; s != "" {
			b, derr := base64.StdEncoding.DecodeString(s)
			if derr != nil || len(b) != ed25519.PublicKeySize {
				return v, excerrors.New(CodeSecretConfigInvalid,
					"jwt material: ed25519_public_b64 must decode to 32 bytes")
			}
			v.EdPub = ed25519.PublicKey(b)
		}
		if v.EdPub == nil && v.EdPriv == nil {
			return v, excerrors.New(CodeSecretConfigInvalid,
				"jwt material: EdDSA needs ed25519 private or public material")
		}
	default:
		return v, excerrors.New(CodeSecretConfigInvalid,
			"jwt material: unsupported alg "+v.Alg)
	}
	if ts := f["not_after"]; ts != "" {
		t, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			return v, excerrors.Wrap(CodeSecretConfigInvalid,
				"jwt material: bad not_after", perr)
		}
		v.NotAfter = t
	}
	return v, nil
}

func parseRSAPrivate(pemStr string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, excerrors.New(CodeSecretConfigInvalid, "rsa_private_pem: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, excerrors.Wrap(CodeSecretConfigInvalid, "rsa_private_pem decode", err)
	}
	rsaK, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, excerrors.New(CodeSecretConfigInvalid, "rsa_private_pem: not RSA")
	}
	return rsaK, nil
}

func parseRSAPublic(pemStr string) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, excerrors.New(CodeSecretConfigInvalid, "rsa_public_pem: no PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, excerrors.Wrap(CodeSecretConfigInvalid, "rsa_public_pem decode", err)
	}
	rsaK, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, excerrors.New(CodeSecretConfigInvalid, "rsa_public_pem: not RSA")
	}
	return rsaK, nil
}

// ---------------------------------------------------------------------------
// Swapper — dynamic-credential pool reload without restart (13.5.3.5
// item 7 / 13.5.3.6 item 3). T is a client handle (*pgxpool.Pool,
// *redis.Client, ...). Build constructs a fresh handle from the leased
// credential; Rotate atomically swaps it in and closes the predecessor
// only after the swap — in-flight holders finish on the old handle
// (owner's Close semantic), new readers get the new credential.
// ---------------------------------------------------------------------------

// Swapper hot-swaps a client handle on credential rotation.
type Swapper[T any] struct {
	// Build constructs a ready-to-use handle from the credential. It must
	// validate connectivity (Ping) — a failed build aborts the swap and
	// the old handle stays live.
	Build func(ctx context.Context, c *DynamicCredential) (*T, error)
	// Close releases a retired handle. nil = let GC/the owner handle it.
	Close func(*T)

	cur atomic.Pointer[T]
}

// Get returns the current handle — nil until the first Rotate.
func (s *Swapper[T]) Get() *T { return s.cur.Load() }

// Rotate builds the replacement handle and swaps it in. Build failure
// leaves the current handle untouched (fail-closed: never swap in an
// unverified credential).
func (s *Swapper[T]) Rotate(ctx context.Context, c *DynamicCredential) error {
	if s.Build == nil {
		return excerrors.New(CodeSecretConfigInvalid, "swapper has no Build")
	}
	next, err := s.Build(ctx, c)
	if err != nil {
		return excerrors.Wrap(CodeSecretRotationFailed, "credential rebuild", err)
	}
	old := s.cur.Swap(next)
	if old != nil && old != next && s.Close != nil {
		s.Close(old)
	}
	return nil
}

// LeaseRenewal drives the 1h-TTL dynamic-credential lifecycle against a
// LeaseSource: issue → swap → renew at TTL/2 → on renewal failure,
// re-issue → swap. Renewal failures mark the returned error path; when
// the lease is non-renewable the loop re-issues at TTL/2 instead. Any
// failure to obtain *some* live credential surfaces as an error — the
// caller's supervisor decides whether to degrade; this loop never
// silently keeps a dead credential.
func (s *Swapper[T]) LeaseRenewal(ctx context.Context, ls LeaseSource, role string, onError func(error)) error {
	if ls == nil {
		return excerrors.New(CodeConfigLoadFailed, "no lease source configured")
	}
	cred, err := ls.Issue(ctx, role)
	if err != nil {
		return excerrors.Wrap(CodeConfigLoadFailed,
			"dynamic credential issue role "+role, err)
	}
	if err := s.Rotate(ctx, cred); err != nil {
		return err
	}
	for {
		wait := cred.TTL / leaseRenewAtFraction
		if wait <= 0 {
			wait = dynamicLeaseTTL / leaseRenewAtFraction
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			// Revoke on shutdown — leased creds must not outlive the service.
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = ls.Revoke(rctx, cred.LeaseID)
			cancel()
			return ctx.Err()
		case <-t.C:
		}
		if cred.Renewable {
			if _, err := ls.Renew(ctx, cred.LeaseID, cred.TTL); err == nil {
				continue
			} else if onError != nil {
				onError(err)
			}
		}
		next, err := ls.Issue(ctx, role)
		if err != nil {
			if onError != nil {
				onError(err)
			}
			// Keep the currently-swapped credential and retry on the
			// next tick — the old lease may still be live; if it dies
			// before a re-issue lands, consumers see auth failures and
			// the P1 path takes over. A credential is never fabricated.
			continue
		}
		if err := s.Rotate(ctx, next); err != nil {
			if onError != nil {
				onError(err)
			}
			continue
		}
		cred = next
	}
}
