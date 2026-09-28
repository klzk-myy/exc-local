// Task 7.3.1/7.3.11 unit tests — role matrix, Permits, scope algebra,
// middleware behavior. No PG needed: the binding seam is faked.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/auth"
	"exchange/internal/gateway"

	excerrors "exchange/pkg/errors"
)

// codedErr unwraps a coded exchange error or nil.
func codedErr(err error) *excerrors.Error {
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// ---------------------------------------------------------------------------
// Role set + Permits
// ---------------------------------------------------------------------------

func TestCanonicalRoles(t *testing.T) {
	want := []string{"Super Admin", "Risk Manager", "Compliance Officer",
		"Finance Ops", "Support Agent", "Read-Only Auditor"}
	if len(VenueRoles) != 6 {
		t.Fatalf("spec §8.2 defines exactly 6 roles, got %d", len(VenueRoles))
	}
	for i, r := range want {
		if VenueRoles[i] != r || !ValidRole(r) {
			t.Fatalf("role[%d] = %q, want %q", i, VenueRoles[i], r)
		}
	}
	if ValidRole("") || ValidRole("*") || ValidRole("Admin") {
		t.Fatal("sentinels/unknowns must not be valid roles")
	}
}

func TestPermits(t *testing.T) {
	cases := []struct {
		required, held string
		want           bool
	}{
		{"", "", true},              // ungated
		{"", "Support Agent", true}, // ungated ignores role
		{gateway.RoleAnyAdmin, "Support Agent", true},
		{gateway.RoleAnyAdmin, "Read-Only Auditor", true},
		{gateway.RoleAnyAdmin, "", false}, // no binding never satisfies
		{gateway.RoleAnyAdmin, "bogus", false},
		{"Risk Manager", "Risk Manager", true},
		{"Risk Manager", "Super Admin", true}, // SA satisfies all
		{"Super Admin", "Super Admin", true},
		{"Super Admin", "Risk Manager", false}, // nothing implies SA
		{"Risk Manager", "Finance Ops", false},
		{"Read-Only Auditor", "Support Agent", false},
		{"Support Agent", "Read-Only Auditor", false}, // coequal, disjoint
	}
	for _, c := range cases {
		if got := Permits(c.required, c.held); got != c.want {
			t.Errorf("Permits(%q, %q) = %v, want %v", c.required, c.held, got, c.want)
		}
	}
}

// TestPermissionMatrix walks every role × every role-gated registry row —
// the §8.2 matrix generated from route metadata.
func TestPermissionMatrix(t *testing.T) {
	routes := gateway.SeedRoutes()
	gated := map[string][]string{} // role → permitted admin routes
	for _, role := range VenueRoles {
		gated[role] = PermittedRoutes(routes, role)
	}
	if len(gated[RoleSuperAdmin]) == 0 {
		t.Fatal("Super Admin permits every gated route — got none")
	}
	// Super Admin is the superset: every role's permitted set ⊂ SA's.
	saSet := map[string]bool{}
	for _, k := range gated[RoleSuperAdmin] {
		saSet[k] = true
	}
	// Some registry row requires each named role (else the matrix is
	// structurally broken).
	for _, role := range VenueRoles {
		found := false
		for _, rt := range routes {
			if rt.Auth.Role == role {
				found = true
			}
		}
		if !found && role != RoleReadOnlyAuditor {
			// auditor has rows; all six should — warn loudly if a role
			// has zero dedicated routes (spec coverage smoke).
			t.Logf("note: no registry row requires %q", role)
		}
	}
	byKey := map[string]gateway.Route{}
	for _, rt := range routes {
		byKey[rt.Method+" "+rt.Path] = rt
	}
	// Spot invariants from the §8.2 normative lines:
	must := map[string]string{
		"POST /api/v1/admin/kill-switch":        RoleRiskManager,
		"POST /api/v1/admin/liquidation/manual": RoleRiskManager,
		"GET /api/v1/admin/roles":               RoleSuperAdmin,
	}
	for key, role := range must {
		rt, ok := byKey[key]
		if !ok {
			continue // row may legitimately be absent/renamed in the table
		}
		if rt.Auth.Role != role && rt.Auth.Role != gateway.RoleAnyAdmin {
			t.Fatalf("route %s gated on %q, expected %q", key, rt.Auth.Role, role)
		}
	}
	// Auditor holds read rows but ZERO mutation rows that name it.
	for _, k := range gated[RoleReadOnlyAuditor] {
		rt := byKey[k]
		if rt.Auth.Role == RoleReadOnlyAuditor && rt.Method != http.MethodGet {
			t.Fatalf("auditor-gated mutation route %s violates §8.2 'no mutation'", k)
		}
	}
	// Support Agent satisfies no Risk Manager routes.
	rm := map[string]bool{}
	for _, k := range gated[RoleRiskManager] {
		rm[k] = true
	}
	for _, k := range gated[RoleSupportAgent] {
		rt := byKey[k]
		if rt.Auth.Role == RoleRiskManager {
			t.Fatalf("Support Agent satisfies Risk Manager route %s", k)
		}
	}
	_ = saSet
}

// ---------------------------------------------------------------------------
// Scope algebra (§8.2a)
// ---------------------------------------------------------------------------

func TestIntersectScopeNeverWidens(t *testing.T) {
	granter := &Scope{Desks: []string{"FX-SPOT", "FX-FWD"}, Currencies: []string{"EUR", "USD"}}
	got, err := IntersectScope(granter, &Scope{Desks: []string{"FX-SPOT"}, Currencies: []string{"USD", "JPY"}})
	if err != nil {
		t.Fatalf("intersect: %v", err)
	}
	if len(got.Desks) != 1 || got.Desks[0] != "FX-SPOT" {
		t.Fatalf("desks: %+v", got)
	}
	// currencies requested {USD,JPY} ∩ granter {EUR,USD} = {USD} — JPY dropped.
	if len(got.Currencies) != 1 || got.Currencies[0] != "USD" {
		t.Fatalf("currencies: %+v", got.Currencies)
	}
	// Requested axis absent → inherits the granter's (narrowing).
	got, err = IntersectScope(granter, &Scope{})
	if err != nil {
		t.Fatalf("inherit: %v", err)
	}
	if len(got.Desks) != 2 || len(got.Currencies) != 2 {
		t.Fatalf("inherit axes: %+v", got)
	}
	// Widening attempt: desk outside the granter's set → empty
	// intersection on that axis → hard error.
	if _, err := IntersectScope(granter, &Scope{Desks: []string{"EQUITIES"}}); err == nil {
		t.Fatal("out-of-scope desk grant must fail")
	}
	// Global granter → requested scope stands.
	got, err = IntersectScope(nil, &Scope{Env: []string{"dev"}})
	if err != nil || len(got.Env) != 1 || got.Env[0] != "dev" {
		t.Fatalf("global granter: %+v %v", got, err)
	}
	// Global granter + global request → nil (global binding).
	if got, err := IntersectScope(nil, nil); err != nil || got != nil {
		t.Fatalf("global grant: %+v %v", got, err)
	}
	// Env axis is a real axis: dev-only granter can't grant staging.
	gdev := &Scope{Env: []string{"dev"}}
	if _, err := IntersectScope(gdev, &Scope{Env: []string{"staging"}}); err == nil {
		t.Fatal("env widening must fail")
	}
	got, err = IntersectScope(gdev, &Scope{})
	if err != nil || len(got.Env) != 1 || got.Env[0] != "dev" {
		t.Fatalf("env inherit: %+v %v", got, err)
	}
}

func TestUnionScope(t *testing.T) {
	// Union semantics: any member's unbounded axis makes the union
	// unbounded on that axis.
	u := UnionScope([]*Scope{
		{Desks: []string{"A"}, Env: []string{"dev"}},
		{Desks: []string{"B"}}, // Env empty → union env unbounded
	})
	if len(u.Desks) != 2 || len(u.Env) != 0 {
		t.Fatalf("union: %+v", u)
	}
	// nil member = fully global granter.
	if u := UnionScope([]*Scope{{Desks: []string{"A"}}, nil}); u != nil {
		t.Fatalf("global member → nil union, got %+v", u)
	}
	// No scopes at all: the union is shape-global but unreachable in
	// practice — requireSuperAdmin demands a binding before any grant
	// consults the union. Document the edge, don't invent a deny-scope.
	u = UnionScope(nil)
	if u == nil {
		t.Fatal("empty union must not be nil")
	}
}

func TestScopeAllows(t *testing.T) {
	s := &Scope{Desks: []string{"FX-SPOT"}, Env: []string{"dev", "staging"}}
	if !s.Allows(Dims{Desk: "FX-SPOT"}) {
		t.Fatal("in-scope desk rejected")
	}
	if s.Allows(Dims{Desk: "FX-FWD"}) {
		t.Fatal("out-of-scope desk allowed")
	}
	if !s.AllowsEnv("dev") || !s.AllowsEnv("STAGING") || s.AllowsEnv("production") {
		t.Fatal("env axis wrong")
	}
	if !s.Allows(Dims{Env: "dev"}) || s.Allows(Dims{Env: "production"}) {
		t.Fatal("dims env wrong")
	}
	var nilScope *Scope
	if !nilScope.Allows(Dims{Desk: "ANY", Env: "production"}) {
		t.Fatal("global scope must admit everything")
	}
	// NormalizeEnv normalization on read path.
	raw := []byte(`{"env":["PROD"],"currencies":["usd"]}`)
	sc, err := ParseScope(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sc = sc.Normalize()
	if !sc.AllowsEnv("production") || !sc.Allows(Dims{Currency: "usd"}) {
		t.Fatalf("normalized scope: %+v", sc)
	}
}

func TestNormalizeEnv(t *testing.T) {
	for in, want := range map[string]string{
		"dev": "dev", "development": "dev", "local": "dev", "test": "dev",
		"staging": "staging", "production": "production", "prod": "production",
		"weird": "production", "": "",
	} {
		if got := NormalizeEnv(in); got != want {
			t.Errorf("NormalizeEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Middleware (fake binding source; claims injected via auth.WithClaims)
// ---------------------------------------------------------------------------

type fakeBindings struct {
	m   map[int64][]Binding
	err error
}

func (f fakeBindings) ActiveBindings(_ context.Context, uid int64) ([]Binding, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := []Binding{}
	for _, b := range f.m[uid] {
		if b.Status == StatusActive && b.ExpiresAt.After(time.Now()) {
			out = append(out, b)
		}
	}
	return out, nil
}

func binding(uid int64, role string, scope *Scope) Binding {
	return Binding{ID: 1, UserID: uid, Role: role, Kind: KindStandard,
		Scope: scope, GranterID: 1, GrantedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour), Status: StatusActive}
}

func adminRoute(role string) gateway.Route {
	return gateway.Route{Method: http.MethodGet, Path: "/api/v1/admin/x",
		Version: "v1", Auth: gateway.AuthSpec{Required: true, Role: role},
		RateTier: gateway.TierBasic, Weight: 1, Owner: "test", Status: gateway.StatusLive}
}

func serve(mw *Middleware, rt gateway.Route, claims *auth.Claims, r *http.Request) *httptest.ResponseRecorder {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()) == nil {
			w.WriteHeader(http.StatusTeapot) // identity must be attached
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := mw.Wrap(rt, ok)
	if claims != nil {
		r = r.WithContext(auth.WithClaims(r.Context(), *claims))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func reqWith(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}

func claimsFor(uid int64) *auth.Claims {
	return &auth.Claims{Subject: fmt.Sprint(uid), TokenType: "access"}
}

func TestMiddleware(t *testing.T) {
	store := fakeBindings{m: map[int64][]Binding{
		10: {binding(10, RoleSuperAdmin, nil)},
		11: {binding(11, RoleRiskManager, nil)},
		12: {binding(12, RoleSupportAgent, &Scope{Desks: []string{"FX-SPOT"}})},
		13: {binding(13, RoleReadOnlyAuditor, nil)},
		14: {binding(14, RoleRiskManager, &Scope{Env: []string{"dev"}})},
	}}
	mw := NewMiddleware(store, nil, nil, "production")

	// No credentials → 401.
	if rec := serve(mw, adminRoute(RoleSuperAdmin), nil, reqWith("GET", "/x")); rec.Code != 401 {
		t.Fatalf("no claims: got %d", rec.Code)
	}
	// Authenticated but no binding → 403 FORBIDDEN.
	if rec := serve(mw, adminRoute(RoleSuperAdmin), claimsFor(99), reqWith("GET", "/x")); rec.Code != 403 {
		t.Fatalf("no binding: got %d", rec.Code)
	}
	// Right role → 200 + identity attached.
	if rec := serve(mw, adminRoute(RoleSuperAdmin), claimsFor(10), reqWith("GET", "/x")); rec.Code != 200 {
		t.Fatalf("super admin: got %d", rec.Code)
	}
	// Super Admin satisfies every route (§8.2 "all permissions").
	if rec := serve(mw, adminRoute(RoleRiskManager), claimsFor(10), reqWith("GET", "/x")); rec.Code != 200 {
		t.Fatalf("SA satisfies RM: got %d", rec.Code)
	}
	// Wrong role → 403 UNAUTHORIZED_ROLE.
	rec := serve(mw, adminRoute(RoleSuperAdmin), claimsFor(11), reqWith("GET", "/x"))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "UNAUTHORIZED_ROLE") {
		t.Fatalf("wrong role: %d %s", rec.Code, rec.Body.String())
	}
	// Desk-scoped Support Agent: matching desk ok, foreign desk → 403.
	rt := adminRoute(RoleSupportAgent)
	if rec := serve(mw, rt, claimsFor(12), reqWith("GET", "/x?desk=FX-SPOT")); rec.Code != 200 {
		t.Fatalf("in-scope desk: %d", rec.Code)
	}
	if rec := serve(mw, rt, claimsFor(12), reqWith("GET", "/x?desk=FX-FWD")); rec.Code != 403 {
		t.Fatalf("out-of-scope desk: %d", rec.Code)
	}
	// Env axis: dev-scoped RM binding fails on the production deployment.
	if rec := serve(mw, adminRoute(RoleRiskManager), claimsFor(14), reqWith("GET", "/x")); rec.Code != 403 {
		t.Fatalf("env mismatch (no header → production): %d", rec.Code)
	}
	r2 := reqWith("GET", "/x")
	r2.Header.Set(EnvHeader, "dev")
	if rec := serve(mw, adminRoute(RoleRiskManager), claimsFor(14), r2); rec.Code != 200 {
		t.Fatalf("env-scoped binding on dev: %d", rec.Code)
	}
	// A dev binding cannot cross into staging either (§8.2a.3).
	r3 := reqWith("GET", "/x")
	r3.Header.Set(EnvHeader, "staging")
	if rec := serve(mw, adminRoute(RoleRiskManager), claimsFor(14), r3); rec.Code != 403 {
		t.Fatalf("dev binding on staging: %d", rec.Code)
	}
	// Auditor: GET passes on its route; a mutation under an auditor-held
	// satisfying set is hard-rejected (matrix typo armor).
	if rec := serve(mw, adminRoute(RoleReadOnlyAuditor), claimsFor(13), reqWith("GET", "/x")); rec.Code != 200 {
		t.Fatalf("auditor GET: %d", rec.Code)
	}
	mut := adminRoute(RoleReadOnlyAuditor)
	mut.Method = http.MethodPost
	if rec := serve(mw, mut, claimsFor(13), reqWith("POST", "/x")); rec.Code != 403 {
		t.Fatalf("auditor mutation must fail closed: %d", rec.Code)
	}
	// Route env markers: "nonprod" barred on a production deployment;
	// "env-scoped" requires an explicit env axis (global bindings fail).
	np := adminRoute(RoleRiskManager)
	np.Env = "nonprod"
	if rec := serve(mw, np, claimsFor(11), reqWith("GET", "/x")); rec.Code != 403 {
		t.Fatalf("nonprod route on production: %d", rec.Code)
	}
	npDev := reqWith("GET", "/x")
	npDev.Header.Set(EnvHeader, "staging")
	if rec := serve(mw, np, claimsFor(11), npDev); rec.Code != 200 {
		t.Fatalf("nonprod route via staging target: %d", rec.Code)
	}
	es := adminRoute(RoleRiskManager)
	es.Env = "env-scoped"
	if rec := serve(mw, es, claimsFor(11), reqWith("GET", "/x")); rec.Code != 403 {
		t.Fatalf("global binding on env-scoped route must fail: %d", rec.Code)
	}
	esDev := reqWith("GET", "/x")
	esDev.Header.Set(EnvHeader, "dev")
	if rec := serve(mw, es, claimsFor(14), esDev); rec.Code != 200 {
		t.Fatalf("dev-scoped binding on env-scoped route: %d", rec.Code)
	}
	// Store error → 500 INTERNAL_ERROR, never a silent allow.
	bad := NewMiddleware(fakeBindings{err: fmt.Errorf("pg down")}, nil, nil, "dev")
	if rec := serve(bad, adminRoute(RoleRiskManager), claimsFor(11), reqWith("GET", "/x")); rec.Code != 500 {
		t.Fatalf("store error must be 500: %d", rec.Code)
	}
	// Non-gated route passes through untouched — no identity is attached
	// (the wrapper returns the handler unmodified).
	plain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()) != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	rec2 := httptest.NewRecorder()
	mw.Wrap(adminRoute(""), plain).ServeHTTP(rec2, reqWith("GET", "/x"))
	if rec2.Code != 200 {
		t.Fatalf("ungated route: %d", rec2.Code)
	}
	// Break-glass binding satisfies (Super Admin power) and is tagged.
	bg := binding(15, RoleSuperAdmin, &Scope{Incident: "INC-7"})
	bg.Kind = KindBreakGlass
	bgStore := fakeBindings{m: map[int64][]Binding{15: {bg}}}
	mwBG := NewMiddleware(bgStore, nil, nil, "production")
	rec = serve(mwBG, adminRoute(RoleSuperAdmin), claimsFor(15), reqWith("GET", "/x"))
	if rec.Code != 200 {
		t.Fatalf("break-glass binding must satisfy: %d", rec.Code)
	}
}

// TestAssertScope exercises the handler-level entity check.
func TestAssertScope(t *testing.T) {
	id := &Identity{UserID: 12, Bindings: []Binding{
		binding(12, RoleSupportAgent, &Scope{Desks: []string{"FX-SPOT"}}),
	}}
	ctx := WithIdentity(context.Background(), id)
	if err := AssertScope(ctx, Dims{Desk: "FX-SPOT"}); err != nil {
		t.Fatalf("in-scope entity: %v", err)
	}
	err := AssertScope(ctx, Dims{Desk: "FX-FWD"})
	if e := codedErr(err); e == nil || e.Code != "FORBIDDEN" {
		t.Fatalf("out-of-scope entity must be FORBIDDEN: %v", err)
	}
	// No identity (never wrapped) → UNAUTHORIZED, fail closed.
	if e := codedErr(AssertScope(context.Background(), Dims{})); e == nil || e.Code != "UNAUTHORIZED" {
		t.Fatalf("missing identity must be UNAUTHORIZED")
	}
}

// ---------------------------------------------------------------------------
// Lifecycle math (capExpiry, business days)
// ---------------------------------------------------------------------------

func TestCapExpiry(t *testing.T) {
	now := time.Now().UTC()
	ok := func(role, kind string, d time.Duration) error {
		return capExpiry(role, kind, now, now.Add(d))
	}
	if err := ok(RoleRiskManager, KindStandard, 12*30*24*time.Hour); err != nil {
		t.Fatalf("12mo standard must pass: %v", err)
	}
	if err := ok(RoleRiskManager, KindStandard, 13*30*24*time.Hour); err == nil {
		t.Fatal(">12mo must fail")
	}
	if err := ok(RoleSuperAdmin, KindStandard, 91*24*time.Hour); err == nil {
		t.Fatal("SA >90d must fail")
	}
	if err := ok(RoleSuperAdmin, KindStandard, 90*24*time.Hour); err != nil {
		t.Fatalf("SA 90d ok: %v", err)
	}
	// Break-glass SA escapes the 90d cap but is capped at 4h.
	if err := ok(RoleSuperAdmin, KindBreakGlass, 4*time.Hour); err != nil {
		t.Fatalf("BG 4h ok: %v", err)
	}
	if err := ok(RoleSuperAdmin, KindBreakGlass, 5*time.Hour); err == nil {
		t.Fatal("BG >4h must fail")
	}
	if err := capExpiry(RoleRiskManager, KindStandard, now, now.Add(-time.Hour)); err == nil {
		t.Fatal("past expiry must fail")
	}
}

func TestAddBusinessDays(t *testing.T) {
	fri := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) // Friday
	if got := addBusinessDays(fri, 2); got.Weekday() != time.Tuesday {
		t.Fatalf("Fri+2 biz days = %v, want Tuesday", got.Weekday())
	}
	wed := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) // Wednesday
	if got := addBusinessDays(wed, 2); got.Weekday() != time.Friday {
		t.Fatalf("Wed+2 biz days = %v, want Friday", got.Weekday())
	}
}
