// Route metadata types for the Task 5.3.7 central route registry.
//
// Every endpoint across all phases registers here — including routes whose
// owning phase has not landed (Status=Stub, served 501 until then). The
// registry is the single source for the /api/v1/routes dump, OpenAPI
// generation (Task 5.3.8), the RBAC permission matrix (spec §8.2), the
// rate-weight table (spec §8.8, Task 5.3.40) and the registration-
// completeness CI cross-check (spec §8.4 item 4).
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Status is the route lifecycle marker: Stub until the owning phase lands
// the handler, Live once a real handler is mounted.
type Status string

const (
	StatusStub Status = "stub"
	StatusLive Status = "live"
)

// Rate-tier names from spec §8.3 (5 tiers). RateTier on a Route is the
// *minimum* tier bucket the request is charged against: "public" for
// anonymous-ok endpoints, "basic" for authenticated, "exempt" for
// unbilled meta/health surfaces.
const (
	TierPublic        = "public"        // 5 req/s, keyed by IP
	TierBasic         = "basic"         // 20 req/s
	TierStandard      = "standard"      // 100 req/s
	TierProfessional  = "professional"  // 500 req/s
	TierInstitutional = "institutional" // 2000+ req/s
	TierExempt        = "exempt"        // no quota (e.g. GET /api/v1/time)
)

// Auth-method names accepted on a route (spec §8.1/§8.8 scope matrix).
const (
	AuthJWT    = "jwt"
	AuthHMAC   = "hmac"
	AuthOAuth2 = "oauth2"
)

// API-key permission scopes (spec §8.8 vocabulary — read/trade/transfer/
// admin; there is no `withdraw` scope, money-moving is `transfer`).
const (
	ScopeRead     = "read"
	ScopeTrade    = "trade"
	ScopeTransfer = "transfer"
	ScopeAdmin    = "admin"
)

// RBAC role names (spec §8.2 canonical list).
const (
	RoleSuperAdmin        = "Super Admin"
	RoleRiskManager       = "Risk Manager"
	RoleComplianceOfficer = "Compliance Officer"
	RoleFinanceOps        = "Finance Ops"
	RoleSupportAgent      = "Support Agent"
	RoleReadOnlyAuditor   = "Read-Only Auditor"
)

// AuthSpec declares the authentication/authorisation a route requires.
// Enforcement lands with the auth middleware (Task 5.3.1 / Phase-07 RBAC);
// the metadata is normative now — it drives the §8.2 permission matrix and
// the registry dump consumed by CI.
type AuthSpec struct {
	// Required: anonymous requests are rejected (UNAUTHORIZED) once the
	// auth middleware lands. Public endpoints set Required=false.
	Required bool `json:"required"`
	// Role: required RBAC role for admin routes (§8.2 names); "" = any
	// authenticated principal.
	Role string `json:"role,omitempty"`
	// Scopes: API-key permission scopes the caller must hold (§8.8).
	Scopes []string `json:"scopes,omitempty"`
	// Methods: accepted credential types; nil = all of jwt/hmac/oauth2.
	Methods []string `json:"methods,omitempty"`
}

// BodySchema is the centralized structural request-body declaration
// (Task 5.3.7 step 5 / spec §8.4 item 3). Fields maps a top-level JSON
// field name to a JSON primitive kind ("string", "number", "integer",
// "boolean", "array", "object"). The Router enforces it before the handler
// runs; business-rule validation stays in domain services.
type BodySchema struct {
	Required []string          `json:"required,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
}

// ValidateRequest checks body against the schema: must be a JSON object,
// every Required key present, every declared field matching its kind.
// Unknown fields are tolerated (forward-compatible minor versioning,
// spec §8.6). Returns a describe-the-defect error or nil.
func (s *BodySchema) ValidateRequest(body []byte) error {
	if s == nil {
		return nil
	}
	if len(body) == 0 {
		if len(s.Required) > 0 || len(s.Fields) > 0 {
			return fmt.Errorf("request body required")
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return fmt.Errorf("body is not a JSON object: %v", err)
	}
	for _, key := range s.Required {
		if _, ok := obj[key]; !ok {
			return fmt.Errorf("missing required field %q", key)
		}
	}
	for field, kind := range s.Fields {
		raw, ok := obj[field]
		if !ok {
			continue // optional absent field is fine
		}
		if err := checkJSONKind(field, kind, raw); err != nil {
			return err
		}
	}
	return nil
}

func checkJSONKind(field, kind string, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("field %q is not valid JSON: %v", field, err)
	}
	ok := true
	switch kind {
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "integer":
		f, isNum := v.(float64)
		ok = isNum && f == float64(int64(f))
	case "boolean":
		_, ok = v.(bool)
	case "array":
		_, ok = v.([]any)
	case "object":
		_, ok = v.(map[string]any)
	case "any":
		// declared but unconstrained
	default:
		return fmt.Errorf("field %q declares unknown kind %q", field, kind)
	}
	if !ok {
		return fmt.Errorf("field %q must be %s", field, kind)
	}
	return nil
}

// Route is one registered endpoint. Path uses Go 1.22+ ServeMux wildcard
// syntax ("{id}"). Method "WS" marks a WebSocket upgrade endpoint — it is
// mounted as GET and carried in the table for the completeness check.
type Route struct {
	Method      string      `json:"method"`  // GET|POST|PUT|DELETE|PATCH|WS
	Path        string      `json:"path"`    // /api/v1/orders/{id}
	Version     string      `json:"version"` // "v1"; "" for unversioned (/health/*, /developer)
	Auth        AuthSpec    `json:"auth"`
	RateTier    string      `json:"rate_tier"`     // TierPublic…TierExempt
	Weight      int         `json:"weight"`        // §8.8 request weight (0 → 1)
	Owner       string      `json:"owner"`         // "Phase-05 Task 5.3.3"
	Status      Status      `json:"status"`        // stub|live
	DualControl bool        `json:"dual_control"`  // §8.2 four-eyes operation
	Env         string      `json:"env,omitempty"` // "" any; "nonprod" = test/staging only; "env-scoped" = RBAC env-axis binding required (§8.2a)
	Description string      `json:"description"`
	Schema      *BodySchema `json:"schema,omitempty"` // centralized request validation
}

// key uniquely identifies a registered route for the mux and the table.
func (r Route) key() string { return r.Method + " " + r.Path }

// mountPattern is the net/http ServeMux pattern. WS routes mount as GET —
// the upgrade handler distinguishes them.
func (r Route) mountPattern() string {
	m := r.Method
	if m == "WS" {
		m = http.MethodGet
	}
	return m + " " + r.Path
}

// validate sanity-checks a Route before registration: fail fast on a
// malformed entry rather than letting a bad row reach the mux or the dump.
func (r Route) validate() error {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, "WS":
	default:
		return fmt.Errorf("route %s %s: invalid method %q", r.Method, r.Path, r.Method)
	}
	if r.Path == "" || r.Path[0] != '/' {
		return fmt.Errorf("route %s %s: path must be absolute", r.Method, r.Path)
	}
	switch r.RateTier {
	case TierPublic, TierBasic, TierStandard, TierProfessional, TierInstitutional, TierExempt:
	default:
		return fmt.Errorf("route %s %s: invalid rate_tier %q", r.Method, r.Path, r.RateTier)
	}
	switch r.Status {
	case StatusStub, StatusLive:
	default:
		return fmt.Errorf("route %s %s: invalid status %q", r.Method, r.Path, r.Status)
	}
	if r.Owner == "" {
		return fmt.Errorf("route %s %s: owner required (completeness invariant)", r.Method, r.Path)
	}
	return nil
}

// maxBodyBytes caps centralized-validation reads at the §5.3.29 request
// ceiling (1MB REST).
const maxBodyBytes = 1 << 20
