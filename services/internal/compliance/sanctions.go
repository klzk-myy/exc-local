// Phase-13.5 Task 13.5.3.3 — file-backed sanctions list screener.
// Phase-21 Task 21.3.1 — production extensions: real vendor-list
// formats (OFAC SDN CSV + alt.csv alias file, EU/UN consolidated XML
// with alias containers, UK HMT/OFSI CSV), alias resolution, per-list
// versioning/provenance, PEP-flag list ingestion, refresh deltas and a
// provider-outage quarantine seam (Task 21.3.23).
//
// ListScreener loads sanctions name lists from a configured directory
// and answers the funding.SanctionsScreener / funding.WithdrawalScreener
// seams plus the Phase-21 registration/trade/PEP screening surfaces. It
// reuses the Phase-11 Jaro-Winkler matcher + legal-name normalizer
// (internal/funding) rather than re-implementing them — normalized
// exact match first, then fuzzy match at the spec-pinned >= 0.85
// threshold.
//
// List sources are classified by file name (see classifyFile): OFAC
// SDN CSV (sdn*.csv), OFAC alternate-identity CSV (*alt*.csv), EU
// consolidated XML (*eu*.xml / sanctionEntity format), UN consolidated
// XML (*un*.xml / INDIVIDUAL format), UK HMT/OFSI CSV (*hmt*|*ofsi*|
// *uk*.csv), PEP vendor lists (pep*.*), and one-name-per-line plain
// text. PEP-kind entries NEVER hard-block funding flows — a PEP hit is
// an EDD/review signal owned by Task 21.3.11, not a SANCTIONS_HIT.
//
// The deploy/security/sanctions-dev/ fixture ships clearly labelled
// fictitious entries in each vendor shape so every parser is executable
// in dev and CI; VendorRefresher (sanctions_refresh.go) owns the
// scheduled official-source pulls (daily per Task 21.3.1).
//
// Fail-closed contract: a screener with zero loaded entries — or one
// bound to a quarantined provider gate — reports itself unavailable;
// ScreenDeposit/ScreenWithdrawal/ScreenParty return a non-nil error,
// which the funding layer surfaces as SANCTIONS_SERVICE_UNAVAILABLE (or
// escalates STANDARD-tier deposits to PENDING_REVIEW when the seam is
// nil). A positive match returns hit=true; the funding layer stamps the
// SANCTIONS_HIT flag and parks the flow in PENDING_REVIEW for
// compliance disposition.
package compliance

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"exchange/internal/funding"
)

// sanctionsListExts maps a file extension to its parser. Any other
// extension is ignored — a stray README in the list directory is never
// mistaken for a list.
const (
	extOFACCSV = ".csv"
	extXML     = ".xml"
	extPlain   = ".txt"
	extList    = ".lst"
)

// EntryKind distinguishes hard-block sanctions entries from PEP-flag
// entries. PEP hits route to the Task 21.3.11 EDD workflow (compliance
// hold + dual-control clearance); they are deliberately excluded from
// the deposit/withdrawal block path.
type EntryKind string

const (
	EntryKindSanctions EntryKind = "SANCTIONS"
	EntryKindPEP       EntryKind = "PEP"
)

// List source identifiers — provenance + hit reporting (§14.3 list set).
const (
	SrcOFACSDN        = "OFAC_SDN"
	SrcOFACSDNAlt     = "OFAC_SDN_ALT"
	SrcEUConsolidated = "EU_CONSOLIDATED"
	SrcUNConsolidated = "UN_CONSOLIDATED"
	SrcUKHMT          = "UK_HMT"
	SrcLocal          = "LOCAL"
	SrcPEP            = "PEP_VENDOR"
)

// listEntry is one normalized screening entry. Aliases carry the parent
// party's list-native reference so a hit can be resolved to the primary
// listed name for audit detail.
type listEntry struct {
	norm    string    // normalized match key
	display string    // as-listed name (audit surface)
	source  string    // Src* provenance
	kind    EntryKind // SANCTIONS blocks; PEP reviews
	alias   bool      // true = a.k.a. alias of a primary entry
	ref     string    // list-native reference of the PARENT party (alias) or self (primary)
}

// MatchHit is the structured screen result returned by MatchDetail and
// the screening service — evidence for audit rows, holds and cases.
type MatchHit struct {
	Candidate string  `json:"candidate"`          // the screened party text
	Matched   string  `json:"matched"`            // normalized list entry that matched
	Display   string  `json:"display"`            // as-listed name
	List      string  `json:"list"`               // Src* provenance
	Kind      string  `json:"kind"`               // SANCTIONS | PEP
	Alias     bool    `json:"alias"`              // matched an a.k.a. entry
	AliasOf   string  `json:"alias_of,omitempty"` // primary listed name (alias hits)
	Score     float64 `json:"score"`              // 1.0 exact; Jaro-Winkler otherwise
}

// ListMeta is per-list provenance — the versioned record Task 21.3.1's
// "list versioning/provenance" requirement and the ops status surface
// report.
type ListMeta struct {
	File     string    `json:"file"`
	Source   string    `json:"source"`
	SHA256   string    `json:"sha256"`
	Entries  int       `json:"entries"`
	Version  string    `json:"version,omitempty"` // list-native version (dateGenerated etc.)
	LoadedAt time.Time `json:"loaded_at"`
}

// ListDelta records the entry-set change applied by the last successful
// Reload — the delta-rescreen trigger Task 21.3.11's monitoring sweep
// consumes (new entries re-screen the account base; removed entries
// never auto-clear flags, they feed the review queue).
type ListDelta struct {
	Added   []string `json:"added,omitempty"`   // normalized names newly listed
	Removed []string `json:"removed,omitempty"` // normalized names delisted
}

// ListScreener is an in-memory, file-backed sanctions/PEP name list.
// The loaded set is immutable between Reload calls; matching never
// blocks on I/O.
type ListScreener struct {
	dir  string
	logf func(format string, args ...any)

	mu      sync.RWMutex
	names   map[string]*listEntry // normalized names — exact-match set (both kinds)
	entries []*listEntry          // normalized names — fuzzy scan order
	lists   []ListMeta            // per-file provenance
	loaded  time.Time
	delta   ListDelta // entry-set change of the most recent Reload

	onDelta func(ListDelta) // optional — set via WithDeltaHook (delta rescreen)
	gate    *ProviderGate   // optional — Task 21.3.23 quarantine seam
}

// NewListScreener loads dir immediately — a directory that is missing,
// unreadable, or yields zero usable entries is a construction error so
// a misconfigured deployment fails loudly at boot instead of silently
// screening against an empty list.
func NewListScreener(dir string) (*ListScreener, error) {
	s := &ListScreener{
		dir:  strings.TrimSpace(dir),
		logf: func(string, ...any) {},
	}
	if err := s.Reload(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// WithLogger wires a diagnostic sink (gateway slog adapter).
func (s *ListScreener) WithLogger(f func(format string, args ...any)) *ListScreener {
	if f != nil {
		s.logf = f
	}
	return s
}

// WithDeltaHook registers the delta listener invoked after every
// successful Reload whose entry set changed — Task 21.3.11's monitoring
// service uses it to trigger delta rescreens of the account base.
func (s *ListScreener) WithDeltaHook(fn func(ListDelta)) *ListScreener {
	s.mu.Lock()
	s.onDelta = fn
	s.mu.Unlock()
	return s
}

// WithProviderGate binds the Task 21.3.23 provider-outage quarantine
// gate: while the gate is quarantined every screening call fails closed
// (errUnavailable → SANCTIONS_SERVICE_UNAVAILABLE at the funding seam).
func (s *ListScreener) WithProviderGate(g *ProviderGate) *ListScreener {
	s.gate = g
	return s
}

// Reload re-reads the list directory and swaps the active set
// atomically. Returns the previous set untouched on error — a failed
// reload never unloads a working list. On success the entry-set delta
// is recorded (LastDelta) and the delta hook fires.
func (s *ListScreener) Reload(ctx context.Context) error {
	if s.dir == "" {
		return fmt.Errorf("sanctions: list directory not configured (set EXC_SANCTIONS_LIST_DIR)")
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("sanctions: list dir: %w", err)
	}
	names := map[string]*listEntry{}
	var entries []*listEntry
	var metas []ListMeta
	var parseErrs []string
	for _, de := range ents {
		if de.IsDir() {
			continue
		}
		path := filepath.Join(s.dir, de.Name())
		kind, source, aliasFile := classifyFile(de.Name())
		var got []rawName
		var perr error
		var version string
		switch strings.ToLower(filepath.Ext(de.Name())) {
		case extOFACCSV:
			if aliasFile {
				got, perr = parseOFACAlt(path)
			} else {
				got, version, perr = parseCSVNames(path, source)
			}
		case extXML:
			got, version, perr = parseConsolidatedXML(path)
		case extPlain, extList:
			got, perr = parsePlainList(path)
		default:
			continue // unknown extension — not a list file
		}
		if perr != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", de.Name(), perr))
			continue
		}
		sum := fileSHA256(path)
		meta := ListMeta{File: de.Name(), Source: source, SHA256: sum,
			Version: version}
		added := 0
		for _, rn := range got {
			norm := funding.NormalizeLegalName(rn.name)
			if norm == "" {
				continue
			}
			if _, dup := names[norm]; dup {
				continue
			}
			e := &listEntry{norm: norm, display: strings.TrimSpace(rn.name),
				source: source, kind: kind, alias: rn.alias, ref: rn.ref}
			names[norm] = e
			entries = append(entries, e)
			added++
		}
		if added == 0 {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: no usable names", de.Name()))
			continue
		}
		meta.Entries = added
		metas = append(metas, meta)
	}
	if len(entries) == 0 {
		return fmt.Errorf("sanctions: no usable entries loaded from %s (%v)",
			s.dir, strings.Join(parseErrs, "; "))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].norm < entries[j].norm })
	sort.Slice(metas, func(i, j int) bool { return metas[i].File < metas[j].File })

	// Delta vs the prior set — the Task 21.3.11 rescreen trigger. The
	// first load's delta is the whole list (initial ingestion).
	var delta ListDelta
	s.mu.RLock()
	prev := s.names
	s.mu.RUnlock()
	for norm := range names {
		if _, ok := prev[norm]; !ok {
			delta.Added = append(delta.Added, norm)
		}
	}
	for norm := range prev {
		if _, ok := names[norm]; !ok {
			delta.Removed = append(delta.Removed, norm)
		}
	}
	sort.Strings(delta.Added)
	sort.Strings(delta.Removed)

	s.mu.Lock()
	s.names, s.entries, s.lists, s.loaded = names, entries, metas, time.Now().UTC()
	s.delta = delta
	hook := s.onDelta
	s.mu.Unlock()
	if len(parseErrs) > 0 {
		s.logf("sanctions: partial load — %s", strings.Join(parseErrs, "; "))
	}
	s.logf("sanctions: loaded %d names from %d list file(s) in %s (+%d -%d)",
		len(entries), len(metas), s.dir, len(delta.Added), len(delta.Removed))
	if hook != nil && (len(delta.Added) > 0 || len(delta.Removed) > 0) {
		hook(delta)
	}
	return nil
}

// Stats reports the loaded-list inventory for observability + wiring
// logs (names are the count of distinct normalized entries).
func (s *ListScreener) Stats() (lists []string, entries int, loadedAt time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.lists))
	for _, m := range s.lists {
		names = append(names, m.File)
	}
	return names, len(s.entries), s.loaded
}

// Provenance returns the per-list version inventory (Task 21.3.1):
// file, classified source, content digest, entry count, list-native
// version and load timestamp.
func (s *ListScreener) Provenance() []ListMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ListMeta(nil), s.lists...)
}

// LastDelta returns the entry-set change applied by the most recent
// successful Reload.
func (s *ListScreener) LastDelta() ListDelta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ListDelta{
		Added:   append([]string(nil), s.delta.Added...),
		Removed: append([]string(nil), s.delta.Removed...),
	}
}

// EntryCount reports the number of loaded normalized entries (per kind
// when kind != "").
func (s *ListScreener) EntryCount(kind EntryKind) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if kind == "" {
		return len(s.entries)
	}
	n := 0
	for _, e := range s.entries {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// Match is the normalized-exact then fuzzy (Jaro-Winkler >= 0.85)
// screen of one free-text party name against every loaded entry of
// either kind. Returns the matched list entry for audit detail. Kind-
// restricted matching uses MatchKind; detail uses MatchDetail.
func (s *ListScreener) Match(name string) (string, bool) {
	hit, ok := s.MatchDetail(name)
	if !ok {
		return "", false
	}
	return hit.Matched, true
}

// MatchDetail returns the structured hit (matched entry, source list,
// kind, alias resolution, score) for one candidate name.
func (s *ListScreener) MatchDetail(name string) (MatchHit, bool) {
	return s.matchEntry(name, "")
}

// MatchKind restricts the screen to one entry kind — SANCTIONS for the
// funding-block paths, PEP for the Task 21.3.11 EDD surface.
func (s *ListScreener) MatchKind(name string, kind EntryKind) (MatchHit, bool) {
	return s.matchEntry(name, kind)
}

func (s *ListScreener) matchEntry(name string, kind EntryKind) (MatchHit, bool) {
	norm := funding.NormalizeLegalName(name)
	if norm == "" {
		return MatchHit{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Exact match first (kind-filtered): cheapest and unambiguous.
	if e, ok := s.names[norm]; ok && (kind == "" || e.kind == kind) {
		return s.hitLocked(name, e, 1.0), true
	}
	var best *listEntry
	var bestScore float64
	for _, e := range s.entries {
		if kind != "" && e.kind != kind {
			continue
		}
		sc := funding.JaroWinkler(norm, e.norm)
		if sc >= funding.NameMatchThreshold && (best == nil || sc > bestScore) {
			best, bestScore = e, sc
		}
	}
	if best == nil {
		return MatchHit{}, false
	}
	return s.hitLocked(name, best, bestScore), true
}

// hitLocked renders the hit record; the read lock is already held by
// the caller (matchEntry). Alias resolution below re-locks — see
// aliasOfRLocked.
func (s *ListScreener) hitLocked(candidate string, e *listEntry, score float64) MatchHit {
	h := MatchHit{
		Candidate: candidate,
		Matched:   e.norm,
		Display:   e.display,
		List:      e.source,
		Kind:      string(e.kind),
		Alias:     e.alias,
		Score:     score,
	}
	if e.alias && e.ref != "" {
		for _, p := range s.entries {
			if !p.alias && p.ref != "" && p.ref == e.ref &&
				sameListFamily(e.source, p.source) {
				h.AliasOf = p.norm
				break
			}
		}
	}
	return h
}

// sameListFamily reports whether an alias entry's source and a primary
// entry's source belong to the same upstream list — OFAC publishes the
// alternate-identity file (OFAC_SDN_ALT) separately from the SDN
// primaries, so its refs resolve across the two source labels.
func sameListFamily(aliasSrc, primarySrc string) bool {
	if aliasSrc == primarySrc {
		return true
	}
	return aliasSrc == SrcOFACSDNAlt && primarySrc == SrcOFACSDN
}

// errUnavailable is returned when the screener has no usable entries or
// the provider gate is quarantined — the funding layer maps screener
// errors to fail-closed handling (SANCTIONS_SERVICE_UNAVAILABLE on
// withdrawals, INTERNAL_ERROR on the deposit seam's error return,
// review escalation where documented).
var errUnavailable = fmt.Errorf("sanctions list unavailable — zero entries loaded or provider quarantined")

// errQuarantined is returned when the provider gate reports the
// upstream screening providers unreachable (Task 21.3.23).
var errQuarantined = fmt.Errorf("sanctions provider quarantine active")

// available reports whether the active set is usable: non-empty AND,
// when a provider gate is bound, not quarantined. Fail-closed on either
// axis (spec §2.7 / §14.3, §24 #28).
func (s *ListScreener) available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries) > 0
}

// gateDown reports whether the bound provider gate is quarantined.
func (s *ListScreener) gateDown() bool {
	return s.gate != nil && s.gate.Quarantined()
}

// screenOne screens a single candidate field against one kind; detail
// records which candidate matched which list entry.
func (s *ListScreener) screenOne(kind, value string) (hit bool, matched string) {
	if strings.TrimSpace(value) == "" {
		return false, ""
	}
	if m, ok := s.MatchKind(value, EntryKindSanctions); ok {
		s.logf("sanctions: %s %q matched list entry %q (list=%s score=%.3f)",
			kind, value, m.Matched, m.List, m.Score)
		return true, m.Matched
	}
	return false, ""
}

// ScreenDeposit implements funding.SanctionsScreener — the inbound-wire
// leg of the deposit anti-fraud STANDARD tier. Both the declared sender
// name and the sender account reference are screened (corporate
// accounts frequently embed a trading name in the reference).
// SANCTIONS-kind entries only — PEP flags never hard-block funding.
func (s *ListScreener) ScreenDeposit(_ context.Context, _ int64,
	senderName, senderAccount string) (bool, error) {
	if s.gateDown() {
		return false, errQuarantined
	}
	if !s.available() {
		return false, errUnavailable
	}
	if hit, _ := s.screenOne("deposit sender", senderName); hit {
		return true, nil
	}
	if hit, _ := s.screenOne("deposit sender account", senderAccount); hit {
		return true, nil
	}
	return false, nil
}

// ScreenWithdrawal implements funding.WithdrawalScreener — the outbound
// leg run at withdrawal confirmation. The beneficiary legal name is
// screened first; the raw destination reference is a second candidate
// so an unregistered destination still meets the screen.
func (s *ListScreener) ScreenWithdrawal(_ context.Context, _ int64,
	beneficiaryName, destination string) (bool, error) {
	if s.gateDown() {
		return false, errQuarantined
	}
	if !s.available() {
		return false, errUnavailable
	}
	if hit, _ := s.screenOne("withdrawal beneficiary", beneficiaryName); hit {
		return true, nil
	}
	if hit, _ := s.screenOne("withdrawal destination", destination); hit {
		return true, nil
	}
	return false, nil
}

// ScreenParty screens an ordered candidate set against the chosen entry
// kind and returns every candidate's first hit — the Phase-21 detail
// surface for registration/trade screening and the screening service
// (Task 21.3.11). Empty kind screens both kinds. Fail-closed:
// unavailable screener / quarantined gate → error, never a clean pass.
func (s *ListScreener) ScreenParty(_ context.Context, kind EntryKind,
	candidates ...string) ([]MatchHit, error) {
	if s.gateDown() {
		return nil, errQuarantined
	}
	if !s.available() {
		return nil, errUnavailable
	}
	var hits []MatchHit
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if h, ok := s.matchEntry(c, kind); ok {
			s.logf("sanctions: party %q matched %q (list=%s kind=%s alias=%v score=%.3f)",
				c, h.Matched, h.List, h.Kind, h.Alias, h.Score)
			hits = append(hits, h)
		}
	}
	return hits, nil
}

// ScreenRegistration screens the account-registration party (Task
// 21.3.1 step 2: account creation). Candidates: legal name, trading
// name, and any declared beneficial-owner names — a hit on ANY
// candidate is a SANCTIONS_HIT block.
func (s *ListScreener) ScreenRegistration(ctx context.Context,
	accountID int64, candidates ...string) ([]MatchHit, error) {
	return s.ScreenParty(ctx, EntryKindSanctions, candidates...)
}

// ScreenTrade screens the trading counterparties on the order path's
// Go-side leg (Task 21.3.1 step 2: trades). The hot in-process gate is
// the C++ SanctionsHook (Task 21.3.10) fed by FlagPublisher; this is
// the synchronous check for cold paths (order amend review, FIX desk
// flow, batch-order admission).
func (s *ListScreener) ScreenTrade(ctx context.Context, accountID int64,
	candidates ...string) ([]MatchHit, error) {
	return s.ScreenParty(ctx, EntryKindSanctions, candidates...)
}

// ---------------------------------------------------------------------------
// File classification
// ---------------------------------------------------------------------------

// classifyFile derives the entry kind and list provenance from the file
// name — the documented convention the dev fixtures and the vendor
// refresh drop follow:
//
//	pep*.*           → PEP vendor list (EntryKindPEP, SrcPEP)
//	*alt*.csv        → OFAC alt.csv aliases (EntryKindSanctions, SrcOFACSDNAlt)
//	*hmt*|*ofsi*|*uk-cons*.csv → UK HMT/OFSI consolidated (SrcUKHMT)
//	*eu*.xml         → EU consolidated (SrcEUConsolidated)
//	*un*.xml         → UN consolidated (SrcUNConsolidated)
//	*.xml            → consolidated XML, generic (SrcEUConsolidated default)
//	*.csv            → OFAC SDN-style CSV (SrcOFACSDN)
//	*.txt / *.lst    → plain one-name-per-line (SrcLocal)
func classifyFile(name string) (kind EntryKind, source string, aliasFile bool) {
	lower := strings.ToLower(name)
	kind = EntryKindSanctions
	source = SrcLocal
	if strings.HasPrefix(lower, "pep") || strings.Contains(lower, "_pep") ||
		strings.Contains(lower, ".pep.") {
		kind = EntryKindPEP
		source = SrcPEP
	}
	switch filepath.Ext(lower) {
	case extOFACCSV:
		source = pickFirst(source, SrcOFACSDN)
		if strings.Contains(lower, "alt") {
			source, aliasFile = SrcOFACSDNAlt, true
		} else if strings.Contains(lower, "hmt") ||
			strings.Contains(lower, "ofsi") ||
			strings.Contains(lower, "uk") {
			source = SrcUKHMT
		}
		if kind == EntryKindPEP {
			source = SrcPEP
		}
	case extXML:
		source = pickFirst(source, SrcEUConsolidated)
		switch {
		case strings.Contains(lower, "un"):
			source = SrcUNConsolidated
		case strings.Contains(lower, "eu"):
			source = SrcEUConsolidated
		}
		if kind == EntryKindPEP {
			source = SrcPEP
		}
	}
	return kind, source, aliasFile
}

func pickFirst(cur, def string) string {
	if cur == SrcLocal {
		return def
	}
	return cur
}

// ---------------------------------------------------------------------------
// Parsers
// ---------------------------------------------------------------------------

// rawName is one parsed list name before normalization. ref carries the
// list-native reference (OFAC ent_num, UN DATAID, HMT group id) — for
// primaries it identifies the party; for aliases it identifies the
// PARENT party so the hit can resolve back to the primary listing.
type rawName struct {
	name  string
	ref   string
	alias bool
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// parseCSVNames reads OFAC SDN.CSV rows — field 2 (index 1) is the
// primary name ("LAST, FIRST" for individuals, entity name otherwise),
// field 1 (index 0) the ent_num reference. Header lines whose name
// field is a header token are skipped. UK HMT/OFSI CSV files take the
// header-driven path (parseUKHMTCSV) when a header row is detected.
func parseCSVNames(path string, source string) ([]rawName, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // SDN/OFSI rows are ragged
	r.LazyQuotes = true
	var recs [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		recs = append(recs, rec)
	}
	if source == SrcUKHMT || looksLikeHMTHeader(recs) {
		return parseUKHMTRecords(recs)
	}
	var out []rawName
	for _, rec := range recs {
		var name, ref string
		switch {
		case len(rec) >= 2:
			ref = strings.TrimSpace(rec[0])
			name = rec[1]
		case len(rec) == 1:
			name = rec[0]
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.EqualFold(name, "name") ||
			strings.HasPrefix(name, "#") {
			continue
		}
		out = append(out, rawName{name: name, ref: ref})
	}
	return out, "", nil
}

// looksLikeHMTHeader reports whether the first record carries the UK
// OFSI consolidated CSV header shape (a "Group ID"/"Name1" style row).
func looksLikeHMTHeader(recs [][]string) bool {
	if len(recs) == 0 {
		return false
	}
	for _, cell := range recs[0] {
		l := strings.ToLower(strings.TrimSpace(cell))
		if l == "group id" || l == "name1" || l == "name6" ||
			strings.HasPrefix(l, "alias") {
			return true
		}
	}
	return false
}

// parseUKHMTRecords parses the UK OFSI consolidated CSV: a header row
// names columns; Name1..Name6 compose the primary party name, "Group
// ID"/"GroupID" is the party reference, and any column whose header
// contains "alias" contributes alias rows bound to that group.
func parseUKHMTRecords(recs [][]string) ([]rawName, string, error) {
	if len(recs) < 2 {
		return nil, "", nil
	}
	head := recs[0]
	var nameCols []int
	var aliasCols []int
	refCol := -1
	for i, h := range head {
		l := strings.ToLower(strings.TrimSpace(h))
		switch {
		case l == "group id" || l == "group_id" || l == "groupid":
			refCol = i
		case l == "name1", l == "name2", l == "name3", l == "name4",
			l == "name5", l == "name6", l == "title":
			nameCols = append(nameCols, i)
		case strings.HasPrefix(l, "alias"):
			aliasCols = append(aliasCols, i)
		}
	}
	if len(nameCols) == 0 {
		return nil, "", fmt.Errorf("uk hmt csv: no Name1..6 columns in header")
	}
	var out []rawName
	for _, rec := range recs[1:] {
		var parts []string
		var ref string
		if refCol >= 0 && refCol < len(rec) {
			ref = strings.TrimSpace(rec[refCol])
		}
		for _, c := range nameCols {
			if c < len(rec) {
				if v := strings.TrimSpace(rec[c]); v != "" {
					parts = append(parts, v)
				}
			}
		}
		if len(parts) > 0 {
			out = append(out, rawName{name: strings.Join(parts, " "), ref: ref})
		}
		for _, c := range aliasCols {
			if c >= len(rec) {
				continue
			}
			for _, a := range strings.Split(rec[c], ",") {
				if v := strings.TrimSpace(a); v != "" {
					out = append(out, rawName{name: v, ref: ref, alias: true})
				}
			}
		}
	}
	return out, "", nil
}

// parseOFACAlt reads OFAC's alt.csv alternate-identity file: field 3
// (index 2) is the alternate name, field 2 (index 1) the parent
// ent_num — rows enter as aliases so a match resolves to the primary
// SDN listing.
func parseOFACAlt(path string) ([]rawName, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var out []rawName
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 3 {
			continue
		}
		name := strings.TrimSpace(rec[2])
		if name == "" || strings.EqualFold(name, "alternate name") ||
			strings.HasPrefix(name, "#") {
			continue
		}
		out = append(out, rawName{name: name, ref: strings.TrimSpace(rec[1]),
			alias: true})
	}
}

// consolidatedNameElements are the XML local-names whose text content
// is a whole party name across the EU (WHOLENAME/NAME under SUBJECT)
// and UN consolidated list formats.
var consolidatedNameElements = map[string]bool{
	"WHOLENAME":    true,
	"WHOLE_NAME":   true,
	"NAME":         true,
	"FULL_NAME":    true,
	"NAME_ALIAS":   false, // nested alias containers are not names
	"UN_LIST_TYPE": false,
}

// consolidatedRefElements carry the party's list-native reference id.
var consolidatedRefElements = map[string]bool{
	"DATAID": true, "REFERENCE_ID": true, "EU_REFERENCE_NUMBER": true,
	"LOGICAL_ID": true,
}

// aliasContainers are XML local-names wrapping an alias name — UN
// INDIVIDUAL_ALIAS/ENTITY_ALIAS (ALIAS_NAME children) and EU
// NAME_ALIAS/ALIAS containers (attribute- or element-carried names).
var aliasContainers = map[string]bool{
	"NAME_ALIAS": true, "ALIAS": true, "INDIVIDUAL_ALIAS": true,
	"ENTITY_ALIAS": true, "NAMEALIAS": true,
}

// parseConsolidatedXML extracts party names from EU/UN consolidated
// XML: whole-name elements (WHOLENAME et al.) plus FIRST_NAME…LAST_NAME
// parts composed per INDIVIDUAL/ENTITY/sanctionEntity block, and
// alias-container entries bound to the enclosing party's reference.
// The list version is the root element's dateGenerated/generationDate
// attribute when present.
func parseConsolidatedXML(path string) ([]rawName, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	d := xml.NewDecoder(f)
	var out []rawName
	var version string
	var capture string // element local-name currently capturing text
	var text strings.Builder
	var partyDepth int // >0 while inside INDIVIDUAL/ENTITY/sanctionEntity
	var aliasDepth int // >0 while inside an alias container
	var partNames []string
	var partyRef string
	flushParty := func() {
		if len(partNames) > 0 {
			out = append(out, rawName{
				name: strings.Join(partNames, " "), ref: partyRef})
			partNames = partNames[:0]
		}
	}
	// aliasAttr extracts a composed name from an alias element's
	// attributes (EU nameAlias carries wholeName / firstName+lastName
	// attributes rather than child text).
	aliasAttr := func(t xml.StartElement) string {
		var whole, first, last, middle string
		for _, a := range t.Attr {
			switch strings.ToLower(a.Name.Local) {
			case "wholename", "whole_name", "name", "alias_name":
				whole = a.Value
			case "firstname", "first_name":
				first = a.Value
			case "middlename", "middle_name":
				middle = a.Value
			case "lastname", "last_name":
				last = a.Value
			}
		}
		if whole != "" {
			return whole
		}
		return strings.TrimSpace(strings.Join(
			[]string{first, middle, last}, " "))
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			flushParty()
			return out, version, nil
		}
		if err != nil {
			return nil, "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			ln := t.Name.Local
			// Root-element version attributes (EU EXPORT dateGenerated,
			// UN CONSOLIDATED_LIST dateGenerated / generationDate).
			if partyDepth == 0 && version == "" {
				for _, a := range t.Attr {
					l := strings.ToLower(a.Name.Local)
					if l == "dategenerated" || l == "generationdate" ||
						l == "publish_date" {
						version = a.Value
					}
				}
			}
			switch {
			case ln == "INDIVIDUAL" || ln == "ENTITY" ||
				ln == "sanctionEntity" || ln == "SANCTION_ENTITY":
				if partyDepth == 0 {
					partNames = partNames[:0]
					partyRef = ""
				}
				partyDepth++
				// EU sanctionEntity carries the reference as an
				// attribute (logicalId / euReferenceNumber /
				// unitedNationId).
				for _, a := range t.Attr {
					l := strings.ToLower(a.Name.Local)
					if l == "logicalid" || l == "eureferencenumber" ||
						l == "unitednationid" {
						if partyRef == "" {
							partyRef = a.Value
						}
					}
				}
			case aliasContainers[ln]:
				aliasDepth++
				if v := aliasAttr(t); v != "" {
					out = append(out, rawName{name: v, ref: partyRef,
						alias: true})
				}
			case consolidatedRefElements[ln] && partyDepth > 0 && aliasDepth == 0:
				capture = ln
				text.Reset()
			default:
				if aliasDepth > 0 &&
					(consolidatedNameElements[ln] || isNamePart(ln) ||
						ln == "ALIAS_NAME") {
					capture = ln
					text.Reset()
				} else if consolidatedNameElements[ln] ||
					(partyDepth > 0 && isNamePart(ln)) {
					capture = ln
					text.Reset()
				}
			}
		case xml.CharData:
			if capture != "" {
				text.WriteString(string(t))
			}
		case xml.EndElement:
			ln := t.Name.Local
			if ln == "INDIVIDUAL" || ln == "ENTITY" ||
				ln == "sanctionEntity" || ln == "SANCTION_ENTITY" {
				partyDepth--
				if partyDepth == 0 {
					flushParty()
					partyRef = ""
				}
				continue
			}
			if aliasContainers[ln] {
				aliasDepth--
				continue
			}
			if capture != "" && ln == capture {
				v := strings.TrimSpace(text.String())
				if v != "" {
					switch {
					case aliasDepth > 0:
						out = append(out, rawName{name: v, ref: partyRef,
							alias: true})
					case consolidatedRefElements[ln]:
						if partyRef == "" {
							partyRef = v
						}
					case partyDepth > 0 && isNamePart(ln):
						partNames = append(partNames, v)
					default:
						out = append(out, rawName{name: v, ref: partyRef})
					}
				}
				capture = ""
				text.Reset()
			}
		}
	}
}

// isNamePart reports whether an XML element inside an INDIVIDUAL/ENTITY
// block carries one ordered component of the party name.
func isNamePart(local string) bool {
	switch local {
	case "FIRST_NAME", "SECOND_NAME", "THIRD_NAME", "FOURTH_NAME",
		"MIDDLE_NAME", "LAST_NAME", "TITLE", "NAME_ON_LIST":
		return true
	}
	return false
}

// parsePlainList reads one normalized name per line — the dev/fixture
// format. `#` and `;` prefix lines are comments; inline comments are
// not supported (a `#` inside a name is not legal anyway).
func parsePlainList(path string) ([]rawName, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []rawName
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		out = append(out, rawName{name: line})
	}
	return out, sc.Err()
}
