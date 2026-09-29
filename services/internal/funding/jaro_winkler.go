// Jaro-Winkler string similarity — Phase-11 Task 11.3.11(b): the
// third-party deposit guard compares an inbound wire's originator name
// against the account's verified KYC legal name. Pure Go, no deps.
//
// Direction is pinned: the result is a SIMILARITY in [0,1] — higher is
// more alike — and the acceptance threshold is >= 0.85 (spec-pinned).
package funding

import (
	"strings"
	"unicode"
)

// NameMatchThreshold is the spec-pinned Jaro-Winkler similarity floor:
// originator/legal-name similarity >= 0.85 accepts, < 0.85 quarantines
// as NAME_MISMATCH (third-party deposit).
const NameMatchThreshold = 0.85

// jaroWinklerPrefix is the standard Winkler boost weight (0.1) applied
// over at most jaroWinklerMaxPrefix leading matching characters (4).
const (
	jaroWinklerPrefix    = 0.1
	jaroWinklerMaxPrefix = 4
)

// NormalizeLegalName canonicalises a party name for comparison:
// upper-case, strip punctuation, collapse whitespace, and drop trailing
// corporate-form tokens (LTD/LLC/GMBH/…) which carry no identity
// information but would otherwise depress the similarity score.
func NormalizeLegalName(s string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToUpper(r))
			prevSpace = false
		case r == '&':
			b.WriteString(" AND ")
			prevSpace = false
		default: // punctuation & whitespace → single separator
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	words := strings.Fields(b.String())
	for len(words) > 1 && legalFormTokens[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

// legalFormTokens is the corporate-suffix stoplist (jurisdiction-agnostic
// common forms — matching is fuzzy anyway; stripping these raises the
// signal of the substantive name).
var legalFormTokens = map[string]bool{
	"LTD": true, "LIMITED": true, "LLC": true, "LLP": true, "LP": true,
	"INC": true, "INCORPORATED": true, "CORP": true, "CORPORATION": true,
	"CO": true, "COMPANY": true, "PLC": true, "PTY": true,
	"GMBH": true, "AG": true, "SA": true, "SARL": true, "SAS": true,
	"BV": true, "NV": true, "SPA": true, "SRL": true, "OY": true,
	"AB": true, "AS": true, "KK": true, "PTE": true, "SDN": true,
	"BHD": true, "KGAA": true, "SE": true,
}

// JaroWinkler returns the Jaro-Winkler similarity of a and b in [0,1].
// Inputs are compared raw — callers normalise via NormalizeLegalName
// when comparing party names (tests exercise both entry points).
func JaroWinkler(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	return jaroWinklerRunes([]rune(a), []rune(b))
}

// jaroWinklerRunes is the rune-slice core (avoids byte/rune mismatch on
// non-ASCII legal names).
func jaroWinklerRunes(a, b []rune) float64 {
	j := jaro(a, b)
	if j <= 0 {
		return 0
	}
	// Winkler prefix boost.
	l := 0
	for l < jaroWinklerMaxPrefix && l < len(a) && l < len(b) && a[l] == b[l] {
		l++
	}
	return j + float64(l)*jaroWinklerPrefix*(1-j)
}

// jaro is the base Jaro similarity.
func jaro(a, b []rune) float64 {
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return 0
	}
	matchDist := la
	if lb > matchDist {
		matchDist = lb
	}
	matchDist = matchDist/2 - 1
	if matchDist < 0 {
		matchDist = 0
	}
	aMatch := make([]bool, la)
	bMatch := make([]bool, lb)

	matches := 0
	for i := 0; i < la; i++ {
		start := i - matchDist
		if start < 0 {
			start = 0
		}
		end := i + matchDist + 1
		if end > lb {
			end = lb
		}
		for k := start; k < end; k++ {
			if !bMatch[k] && a[i] == b[k] {
				aMatch[i] = true
				bMatch[k] = true
				matches++
				break
			}
		}
	}
	if matches == 0 {
		return 0
	}
	// Transpositions: half the positional disagreements in matched order.
	var t float64
	k := 0
	for i := 0; i < la; i++ {
		if !aMatch[i] {
			continue
		}
		for !bMatch[k] {
			k++
		}
		if a[i] != b[k] {
			t++
		}
		k++
	}
	t /= 2
	m := float64(matches)
	return (m/float64(la) + m/float64(lb) + (m-t)/m) / 3
}
