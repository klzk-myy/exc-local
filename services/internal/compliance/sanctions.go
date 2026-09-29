// Phase-13.5 Task 13.5.3.3 — file-backed sanctions list screener.
//
// ListScreener loads sanctions name lists (OFAC SDN CSV, EU/UN
// consolidated XML, or one-name-per-line plain text) from a configured
// directory and answers the funding.SanctionsScreener /
// funding.WithdrawalScreener seams. It reuses the Phase-11
// Jaro-Winkler matcher + legal-name normalizer (internal/funding)
// rather than re-implementing them — normalized exact match first,
// then fuzzy match at the spec-pinned >= 0.85 threshold.
//
// Scope honesty: this is the development/validation binding. The
// deploy/security/sanctions-dev/ fixture ships a handful of clearly
// labelled fictitious entries so the screening path is executable in
// dev and CI. Production vendor feeds (Dow Jones / World-Check /
// official OFAC-EU-UN pulls with delta ingestion, alias resolution,
// PEP/adverse-media and ongoing monitoring) are owned by Phase-21 —
// see docs/security/compliance-deferred-phase21.md.
//
// Fail-closed contract: a screener with zero loaded entries reports
// itself unavailable — ScreenDeposit/ScreenWithdrawal return a
// non-nil error, which the funding layer surfaces as
// SANCTIONS_SERVICE_UNAVAILABLE (or escalates STANDARD-tier deposits
// to PENDING_REVIEW when the seam is nil). A positive match returns
// hit=true; the funding layer stamps the SANCTIONS_HIT flag and parks
// the flow in PENDING_REVIEW for compliance disposition.
package compliance

import (
	"bufio"
	"context"
	"encoding/csv"
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

// ListScreener is an in-memory, file-backed sanctions name list. The
// loaded set is immutable between Reload calls; matching never blocks
// on I/O.
type ListScreener struct {
	dir  string
	logf func(format string, args ...any)

	mu      sync.RWMutex
	names   map[string]struct{} // normalized names — exact-match set
	entries []string            // normalized names — fuzzy scan order
	lists   []string            // file names that contributed entries
	loaded  time.Time
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

// Reload re-reads the list directory and swaps the active set
// atomically. Returns the previous set untouched on error — a failed
// reload never unloads a working list.
func (s *ListScreener) Reload(ctx context.Context) error {
	if s.dir == "" {
		return fmt.Errorf("sanctions: list directory not configured (set EXC_SANCTIONS_LIST_DIR)")
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("sanctions: list dir: %w", err)
	}
	names := map[string]struct{}{}
	var entries, lists []string
	var parseErrs []string
	for _, de := range ents {
		if de.IsDir() {
			continue
		}
		path := filepath.Join(s.dir, de.Name())
		var got []string
		var perr error
		switch strings.ToLower(filepath.Ext(de.Name())) {
		case extOFACCSV:
			got, perr = parseOFACSDN(path)
		case extXML:
			got, perr = parseConsolidatedXML(path)
		case extPlain, extList:
			got, perr = parsePlainList(path)
		default:
			continue // unknown extension — not a list file
		}
		if perr != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", de.Name(), perr))
			continue
		}
		if len(got) == 0 {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: no usable names", de.Name()))
			continue
		}
		lists = append(lists, de.Name())
		for _, n := range got {
			norm := funding.NormalizeLegalName(n)
			if norm == "" {
				continue
			}
			if _, dup := names[norm]; dup {
				continue
			}
			names[norm] = struct{}{}
			entries = append(entries, norm)
		}
	}
	if len(entries) == 0 {
		return fmt.Errorf("sanctions: no usable entries loaded from %s (%v)",
			s.dir, strings.Join(parseErrs, "; "))
	}
	sort.Strings(entries)
	sort.Strings(lists)
	s.mu.Lock()
	s.names, s.entries, s.lists, s.loaded = names, entries, lists, time.Now().UTC()
	s.mu.Unlock()
	if len(parseErrs) > 0 {
		s.logf("sanctions: partial load — %s", strings.Join(parseErrs, "; "))
	}
	s.logf("sanctions: loaded %d names from %d list file(s) in %s",
		len(entries), len(lists), s.dir)
	return nil
}

// Stats reports the loaded-list inventory for observability + wiring
// logs (names are the count of distinct normalized entries).
func (s *ListScreener) Stats() (lists []string, entries int, loadedAt time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.lists...), len(s.entries), s.loaded
}

// Match is the normalized-exact then fuzzy (Jaro-Winkler >= 0.85)
// screen of one free-text party name against every loaded entry.
// Returns the matched list entry for audit detail.
func (s *ListScreener) Match(name string) (string, bool) {
	norm := funding.NormalizeLegalName(name)
	if norm == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.names[norm]; ok {
		return norm, true
	}
	for _, e := range s.entries {
		if funding.JaroWinkler(norm, e) >= funding.NameMatchThreshold {
			return e, true
		}
	}
	return "", false
}

// errUnavailable is returned when the screener has no usable entries —
// the funding layer maps screener errors to fail-closed handling
// (SANCTIONS_SERVICE_UNAVAILABLE on withdrawals, INTERNAL_ERROR on the
// deposit seam's error return, review escalation where documented).
var errUnavailable = fmt.Errorf("sanctions list unavailable — zero entries loaded")

// loaded reports whether the active set is usable.
func (s *ListScreener) available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries) > 0
}

// screenOne screens a single candidate field; detail records which
// candidate matched which list entry.
func (s *ListScreener) screenOne(kind, value string) (hit bool, matched string) {
	if strings.TrimSpace(value) == "" {
		return false, ""
	}
	if m, ok := s.Match(value); ok {
		s.logf("sanctions: %s %q matched list entry %q", kind, value, m)
		return true, m
	}
	return false, ""
}

// ScreenDeposit implements funding.SanctionsScreener — the inbound-wire
// leg of the deposit anti-fraud STANDARD tier. Both the declared sender
// name and the sender account reference are screened (corporate
// accounts frequently embed a trading name in the reference).
func (s *ListScreener) ScreenDeposit(_ context.Context, _ int64,
	senderName, senderAccount string) (bool, error) {
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

// ---------------------------------------------------------------------------
// Parsers
// ---------------------------------------------------------------------------

// parseOFACSDN reads OFAC SDN.CSV rows — field 2 (index 1) is the
// primary name ("LAST, FIRST" for individuals, entity name otherwise).
// Header lines whose name field is a header token are skipped.
func parseOFACSDN(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // SDN rows are ragged
	r.LazyQuotes = true
	var out []string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var name string
		switch {
		case len(rec) >= 2:
			name = rec[1]
		case len(rec) == 1:
			name = rec[0]
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.EqualFold(name, "name") ||
			strings.HasPrefix(name, "#") {
			continue
		}
		out = append(out, name)
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

// parseConsolidatedXML extracts party names from EU/UN consolidated
// XML: whole-name elements (WHOLENAME et al.) plus FIRST_NAME…LAST_NAME
// parts composed per INDIVIDUAL/ENTITY block.
func parseConsolidatedXML(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := xml.NewDecoder(f)
	var out []string
	var capture string // element local-name currently capturing text
	var text strings.Builder
	var partyDepth int // >0 while inside INDIVIDUAL/ENTITY
	var partNames []string
	flushParty := func() {
		if len(partNames) > 0 {
			out = append(out, strings.Join(partNames, " "))
			partNames = partNames[:0]
		}
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			flushParty()
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			ln := t.Name.Local
			switch ln {
			case "INDIVIDUAL", "ENTITY":
				if partyDepth == 0 {
					partNames = partNames[:0]
				}
				partyDepth++
			default:
				if consolidatedNameElements[ln] ||
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
			if ln == "INDIVIDUAL" || ln == "ENTITY" {
				partyDepth--
				if partyDepth == 0 {
					flushParty()
				}
				continue
			}
			if capture != "" && ln == capture {
				v := strings.TrimSpace(text.String())
				if v != "" {
					if partyDepth > 0 && isNamePart(ln) {
						partNames = append(partNames, v)
					} else {
						out = append(out, v)
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
func parsePlainList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}
