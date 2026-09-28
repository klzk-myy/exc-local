// Package errs is the canonical error-code registry (Phase-05 Task 5.3.21).
//
// It embeds the spec §23 table (149 codes) as data and enforces the task's
// invariants:
//   - every emitted code must be registered — Emit/NewError on an
//     unregistered code degrades to INTERNAL_ERROR and records a violation
//     that ValidateEmissions surfaces at startup and in CI;
//   - every code is owner-resolvable — Lookup exposes the owning
//     "Phase-NN Task N.N.N" citation (or the reserved/deprecated markers)
//     so CI can assert zero ownerless codes;
//   - the registry is exposed for CI checks via All()/SpecTable() and is
//     served to admins by the gateway's GET /api/v1/errors meta endpoint.
//
// Severity stays in pkg/errors (SeverityFor) — one classification source;
// this package owns code→HTTP status and owner discoverability only.
package errs

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	pkgerrors "exchange/pkg/errors"
)

// Code is the generic fallback code emitted for unclassified failures and
// for attempts to emit an unregistered code (fail-closed, spec §2.7.1).
const CodeInternalError = "INTERNAL_ERROR"

// CodeNotImplemented is emitted by registered routes whose owning phase has
// not landed yet (registration-completeness invariant, spec §8.4 item 4).
const CodeNotImplemented = "NOT_IMPLEMENTED"

// Registry maps error code → definition and records emission violations.
// The zero-value-unsafe fields are initialised by New; use Default for the
// process-wide registry.
type Registry struct {
	mu         sync.RWMutex
	defs       map[string]CodeDef
	emitted    map[string]struct{} // codes observed through NewError
	violations []string            // unregistered emission attempts
}

// New returns a registry pre-loaded with the embedded spec §23 table plus
// the gateway-local codes (localCodes — Spec=false until §23 grows rows).
func New() *Registry {
	r := &Registry{defs: make(map[string]CodeDef, len(specCodes)+len(localCodes)), emitted: make(map[string]struct{})}
	for _, d := range specCodes {
		r.defs[d.Code] = d
	}
	for _, d := range localCodes {
		r.defs[d.Code] = d
	}
	return r
}

// Default is the process-wide registry loaded with spec §23. Handlers emit
// through it (NewError/Emit); services validate it at startup.
var Default = New()

// Register adds d to the registry. Re-registering the same code with an
// identical definition is a no-op; a conflicting re-registration (different
// HTTP status or description) returns an error — silently rebinding a code
// to a different contract is a defect.
func (r *Registry) Register(d CodeDef) error {
	if d.Code == "" {
		return fmt.Errorf("errs: register: empty code")
	}
	if d.HTTPStatus < 100 || d.HTTPStatus > 599 {
		return fmt.Errorf("errs: register %s: invalid HTTP status %d", d.Code, d.HTTPStatus)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.defs[d.Code]; ok {
		if prev.HTTPStatus != d.HTTPStatus || prev.Description != d.Description {
			return fmt.Errorf("errs: register %s: conflicts with existing definition (status %d→%d)",
				d.Code, prev.HTTPStatus, d.HTTPStatus)
		}
		return nil // identical re-registration: idempotent
	}
	r.defs[d.Code] = d
	return nil
}

// MustRegister is Register for init-time registration; it panics on a
// conflicting definition so a bad code table fails startup loudly.
func (r *Registry) MustRegister(d CodeDef) {
	if err := r.Register(d); err != nil {
		panic(err)
	}
}

// Lookup returns the definition for code.
func (r *Registry) Lookup(code string) (CodeDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[code]
	return d, ok
}

// HTTPStatus resolves code to its spec §23 HTTP status. Unknown codes map
// to 500: an unclassified failure is never a client fault (fail-closed).
func (r *Registry) HTTPStatus(code string) int {
	if d, ok := r.Lookup(code); ok {
		return d.HTTPStatus
	}
	return 500
}

// Severity resolves the spec §2.7.2 tier for code, delegating to the single
// severity source in pkg/errors.
func (r *Registry) Severity(code string) pkgerrors.Severity {
	return pkgerrors.SeverityFor(code)
}

// All returns every registered definition sorted by code — the payload for
// GET /api/v1/errors and the CI registry dump.
func (r *Registry) All() []CodeDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CodeDef, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Len returns the total registered-code count (spec + local).
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.defs)
}

// SpecTable returns a copy of the embedded spec §23 rows for CI checks that
// diff code ↔ spec (e.g. count must equal the §23 row total).
func SpecTable() []CodeDef {
	out := make([]CodeDef, len(specCodes))
	copy(out, specCodes)
	return out
}

// NewError builds a coded error through the emission gate: a registered code
// produces *pkgerrors.Error{Code: code}; an unregistered code records a
// violation and returns an INTERNAL_ERROR-coded error (fail-closed — the
// caller's message is preserved for logs but the client-visible code must
// come from the registry).
func (r *Registry) NewError(code, message string) *pkgerrors.Error {
	r.mu.Lock()
	r.emitted[code] = struct{}{}
	if _, ok := r.defs[code]; !ok {
		r.violations = append(r.violations, code)
		code = CodeInternalError
	}
	r.mu.Unlock()
	return pkgerrors.New(code, message)
}

// NewErrorf is NewError with a formatted message.
func (r *Registry) NewErrorf(code, format string, args ...any) *pkgerrors.Error {
	return r.NewError(code, fmt.Sprintf(format, args...))
}

// Violations returns the distinct codes emitted through NewError that were
// not registered — the input to the startup/CI emission gate.
func (r *Registry) Violations() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := make(map[string]struct{}, len(r.violations))
	var out []string
	for _, c := range r.violations {
		if _, dup := set[c]; !dup {
			set[c] = struct{}{}
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// Emitted returns the sorted set of codes observed through NewError —
// emitted-coverage data for the CI registry check.
func (r *Registry) Emitted() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.emitted))
	for c := range r.emitted {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CheckRegistered returns an error listing any of the given codes absent
// from the registry. Services declare the codes they can emit at startup and
// fail fast if a code was never registered (Task 5.3.21 fail-startup gate).
func (r *Registry) CheckRegistered(codes ...string) error {
	var missing []string
	for _, c := range codes {
		if _, ok := r.Lookup(c); !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("errs: unregistered error codes declared: %s", strings.Join(missing, ", "))
	}
	return nil
}

// ValidateEmissions fails if any code was emitted through NewError without
// being registered. Call it after startup wiring (and in tests) so an
// unregistered emission fails the process before it serves traffic.
func (r *Registry) ValidateEmissions() error {
	if v := r.Violations(); len(v) > 0 {
		return fmt.Errorf("errs: %d unregistered code(s) emitted: %s", len(v), strings.Join(v, ", "))
	}
	return nil
}

// UnresolvedOwners lists registered codes that carry no owning "Phase-NN
// Task" citation in their description and are not reserved/deprecated.
// These are owner-resolvable only via the alternate §23 branch (the code
// token appearing in a phase plan) — CI greps the phase docs for them; the
// list must shrink to zero or be fully covered by that scan.
func (r *Registry) UnresolvedOwners() []CodeDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []CodeDef
	for _, d := range r.defs {
		if d.Owner == "" && !d.Reserved && d.SupersededBy == "" {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
