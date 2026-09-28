// Binding data scopes (spec §8.2a, §19.16.4) — Task 7.3.11.
//
// A binding's scope is a JSON object with optional array axes
// {desks, regions, currencies, env}; NULL/absent axes are unbounded.
// Grant-time semantics: the granted scope is intersected with the
// granter's own scope — narrowing never widens. Request-time semantics:
// every populated axis must admit the request's dimension or the read
// rejects FORBIDDEN.
package admin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Scope is one binding's data scope. A nil *Scope is global.
type Scope struct {
	Desks      []string `json:"desks,omitempty"`
	Regions    []string `json:"regions,omitempty"`
	Currencies []string `json:"currencies,omitempty"`
	Env        []string `json:"env,omitempty"`      // §8.2a.3/§19.16.4 axis
	Incident   string   `json:"incident,omitempty"` // break-glass confinement tag
}

// ParseScope decodes the scope column (object already shape-checked by
// the migration CHECK; we still validate defensively on read).
func ParseScope(raw []byte) (*Scope, error) {
	var s Scope
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("scope decode: %w", err)
	}
	return &s, nil
}

// IsGlobal reports whether the scope imposes no bounds.
func (s *Scope) IsGlobal() bool {
	return s == nil || (len(s.Desks) == 0 && len(s.Regions) == 0 &&
		len(s.Currencies) == 0 && len(s.Env) == 0 && s.Incident == "")
}

// Marshal returns the JSONB encoding (NULL when global).
func (s *Scope) Marshal() ([]byte, error) {
	if s == nil || s.IsGlobal() {
		return nil, nil
	}
	return json.Marshal(s)
}

func contains(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func normList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Normalize trims/sorts axes and upper-cases currencies + env values so
// comparisons are deterministic.
func (s *Scope) Normalize() *Scope {
	if s == nil {
		return nil
	}
	out := &Scope{
		Desks:    normList(s.Desks),
		Regions:  normList(s.Regions),
		Incident: strings.TrimSpace(s.Incident),
	}
	for _, v := range s.Currencies {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v != "" {
			out.Currencies = append(out.Currencies, v)
		}
	}
	for _, v := range s.Env {
		if e := NormalizeEnv(v); e != "" {
			out.Env = append(out.Env, e)
		}
	}
	sort.Strings(out.Currencies)
	sort.Strings(out.Env)
	return out
}

// AllowsEnv reports whether the scope admits the target environment.
// Empty env axis = all environments (§8.2a.3: only an explicit axis
// confines).
func (s *Scope) AllowsEnv(env string) bool {
	if s == nil || len(s.Env) == 0 {
		return true
	}
	return contains(s.Env, NormalizeEnv(env))
}

// Dims is a request-time scope probe: each non-empty field is a
// dimension the caller claims to touch.
type Dims struct {
	Desk     string
	Region   string
	Currency string
	Env      string
}

// Allows reports whether every populated axis of s admits dims. Empty
// dims fields are unconstraining (the caller touched nothing on that
// axis); empty scope axes admit everything on theirs.
func (s *Scope) Allows(d Dims) bool {
	if s == nil {
		return true
	}
	if d.Desk != "" && len(s.Desks) > 0 && !contains(s.Desks, d.Desk) {
		return false
	}
	if d.Region != "" && len(s.Regions) > 0 && !contains(s.Regions, d.Region) {
		return false
	}
	if d.Currency != "" && len(s.Currencies) > 0 &&
		!contains(s.Currencies, strings.ToUpper(d.Currency)) {
		return false
	}
	if d.Env != "" && !s.AllowsEnv(d.Env) {
		return false
	}
	return true
}

// IntersectScope computes the grant-time intersection (spec §8.2a.1):
// the granted scope may narrow, never widen, the granter's scope.
//
// Per axis:
//   - granter axis empty (unbounded) → requested axis stands;
//   - requested axis empty → inherits the granter's axis (a request for
//     "everything" yields what the granter can see, not the universe);
//   - both populated → set intersection; an empty intersection is a
//     hard error — a binding scoped to nothing is a grant bug, not a
//     usable row.
//
// Union-granter semantics: when the granter holds several bindings, the
// granter side is the per-axis union (an empty axis anywhere on the
// granter side means unbounded on that axis).
func IntersectScope(granter *Scope, requested *Scope) (*Scope, error) {
	if granter == nil || granter.IsGlobal() {
		// Global granter: the requested scope stands verbatim.
		return requested.Normalize(), nil
	}
	g := granter.Normalize()
	var req Scope
	if requested != nil {
		req = *requested.Normalize()
	}
	axis := func(name string, g, r []string) ([]string, error) {
		switch {
		case len(g) == 0:
			return r, nil
		case len(r) == 0:
			return g, nil
		}
		var out []string
		for _, v := range r {
			if contains(g, v) {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf(
				"scope axis %s: requested %v has no overlap with granter scope %v",
				name, r, g)
		}
		return out, nil
	}
	var err error
	out := &Scope{Incident: req.Incident}
	if out.Desks, err = axis("desks", g.Desks, req.Desks); err != nil {
		return nil, err
	}
	if out.Regions, err = axis("regions", g.Regions, req.Regions); err != nil {
		return nil, err
	}
	if out.Currencies, err = axis("currencies", g.Currencies, req.Currencies); err != nil {
		return nil, err
	}
	if out.Env, err = axis("env", g.Env, req.Env); err != nil {
		return nil, err
	}
	if out.IsGlobal() {
		return nil, nil
	}
	return out, nil
}

// UnionScope merges several scopes per axis for the granter side of a
// grant intersection. An empty axis on any member scope makes the union
// unbounded on that axis (nil member = fully global).
func UnionScope(scopes []*Scope) *Scope {
	if len(scopes) == 0 {
		return &Scope{} // no bindings ⇒ no scope at all (empty, bounded)
	}
	out := &Scope{}
	global := map[string]bool{} // axis name → unbounded
	seen := func(dst *[]string, axis string, vals []string) {
		if global[axis] {
			return
		}
		if len(vals) == 0 {
			global[axis] = true
			*dst = nil
			return
		}
		for _, v := range vals {
			if !contains(*dst, v) {
				*dst = append(*dst, v)
			}
		}
	}
	for _, s := range scopes {
		if s == nil {
			return nil // a global binding makes the granter global
		}
		seen(&out.Desks, "desks", s.Desks)
		seen(&out.Regions, "regions", s.Regions)
		seen(&out.Currencies, "currencies", s.Currencies)
		seen(&out.Env, "env", s.Env)
		if s.Incident != "" && !contains([]string{out.Incident}, s.Incident) {
			out.Incident = s.Incident // carry the latest tag only
		}
	}
	return out.Normalize()
}

// NormalizeEnv maps a deployment/environment label onto the spec §19.16
// vocabulary (dev | staging | production). Unknown/empty labels map to
// "production" — fail-closed, an unrecognized env is treated as the
// most restricted.
func NormalizeEnv(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "dev", "development", "local", "test", "testing", "ci", "sandbox":
		return "dev"
	case "staging", "stage":
		return "staging"
	case "production", "prod":
		return "production"
	case "":
		return ""
	}
	return "production"
}
