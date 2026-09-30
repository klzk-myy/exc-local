// statement_parser.go — SWIFT MT940 (customer statement) and MT942
// (interim transaction report) decoders for Task 24.3.12 (spec §5.34,
// §17.10). Produces the normalized settlement.ParsedStatement model the
// ingestion service validates, persists and reconciles.
//
// MT940 grammar (FIN tag-block, ISO 15022):
//
//	:20:  sender's reference
//	:25:  account identification (IBAN)
//	:28C: statement number[/sequence number]
//	:60F: opening balance   1!a6!n3!a15d      (C/D YYMMDD CCY amount)
//	:61:  statement line    6!n[4!n]2a[1!a]15d1!a3!c16x[//16x][CRLF 34x]
//	:86:  information to account owner (follows :61:)
//	:62F: closing balance
//	:64:/:65:  available / forward balances (carried, not required)
//
// MT942 repeats :25:/:28C:/:61:/:86: plus :34F: floor limits and
// :90D:/:90C: debit/credit totals; balances 60F/62F are absent.
//
// Reversal marks: :61: D/C are plain marks; RD/RC mark a reversal line —
// the entry's IsReversal flag is set and the effective direction flips
// (spec §5.34 row "reversal lines").
//
// Fail-closed: a malformed mandatory field aborts the whole parse —
// partial statements never reach the ledger (spec §2.7).
package backoffice

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/settlement"
	excerrors "exchange/pkg/errors"
)

// mt940Required are the mandatory MT940 tags per Task 24.3.12.
var mt940Required = []string{"20", "25", "28C", "60F", "62F"}

// ParseMT940 decodes a FIN-format MT940 statement. raw may carry the
// :4:-style block wrapper; tag extraction is positional so envelope
// headers (:1:/:2:/:3:) are tolerated and ignored.
func ParseMT940(ctx context.Context, raw []byte) (*settlement.ParsedStatement, error) {
	return parseMT940Like(ctx, raw, "MT940", true)
}

// ParseMT942 decodes an MT942 interim transaction report (no opening/
// closing balances; :90D:/:90C: totals instead).
func ParseMT942(ctx context.Context, raw []byte) (*settlement.ParsedStatement, error) {
	return parseMT940Like(ctx, raw, "MT942", false)
}

func parseMT940Like(_ context.Context, raw []byte, format string, requireBalances bool) (*settlement.ParsedStatement, error) {
	tags := extractSwiftTags(string(raw))
	if len(tags) == 0 {
		return nil, fmt.Errorf("%s: no tag-block content", format)
	}
	have := map[string]bool{}
	for _, t := range tags {
		have[t.tag] = true
	}
	if requireBalances {
		for _, req := range mt940Required {
			if !have[req] {
				return nil, fmt.Errorf("%s: missing mandatory tag :%s:", format, req)
			}
		}
	} else if !have["25"] || !have["28C"] {
		return nil, fmt.Errorf("%s: missing mandatory :25:/:28C:", format)
	}

	st := &settlement.ParsedStatement{Format: format}
	for i := 0; i < len(tags); i++ {
		t := tags[i]
		switch t.tag {
		case "25":
			st.IBAN = strings.TrimSpace(t.value)
		case "28C":
			num, seq, err := parseStatementNumber(t.value)
			if err != nil {
				return nil, fmt.Errorf("%s :28C: %w", format, err)
			}
			st.StatementNumber = num
			st.SequenceNumber = seq
		case "60F", "62F":
			ccy, amt, date, err := parseSwiftBalance(t.value)
			if err != nil {
				return nil, fmt.Errorf("%s :%s: %w", format, t.tag, err)
			}
			if st.Currency == "" {
				st.Currency = ccy
			} else if st.Currency != ccy {
				return nil, fmt.Errorf("%s: currency mismatch %s vs %s", format, ccy, st.Currency)
			}
			d := amt
			if t.tag == "60F" {
				st.OpeningBalance = &d
			} else {
				st.ClosingBalance = &d
				st.StatementDate = date
			}
		case "34F":
			// Floor-limit indicator: 3!a[1!a]15d → currency+amount (MT942).
			if len(t.value) >= 4 {
				if ccy := strings.ToUpper(strings.TrimSpace(t.value[:3])); st.Currency == "" {
					st.Currency = ccy
				} else if st.Currency != ccy {
					return nil, fmt.Errorf("%s: currency mismatch %s vs %s", format, ccy, st.Currency)
				}
			}
		case "90D", "90C":
			// Totals: 2!n3!a15d → count + currency + amount (MT942).
			// Some banks emit a 1-digit count; strip all leading digits.
			rest := strings.TrimLeft(strings.TrimSpace(t.value), "0123456789")
			if len(rest) >= 3 {
				if ccy := strings.ToUpper(rest[:3]); st.Currency == "" {
					st.Currency = ccy
				} else if st.Currency != ccy {
					return nil, fmt.Errorf("%s: currency mismatch %s vs %s", format, ccy, st.Currency)
				}
			}
		case "61":
			e, err := parseMT940Entry(t.value)
			if err != nil {
				return nil, fmt.Errorf("%s :61: entry %d: %w", format, len(st.Entries)+1, err)
			}
			// :86: narrative attaches to the preceding :61:.
			if i+1 < len(tags) && tags[i+1].tag == "86" {
				e.Narrative = strings.TrimSpace(tags[i+1].value)
				e.RemitterName, e.RemitterAccount = parseMT940Remitter(e.Narrative)
				i++
			}
			if st.Currency != "" && e.Currency == "" {
				e.Currency = st.Currency
			}
			st.Entries = append(st.Entries, *e)
		}
	}
	if requireBalances && len(st.Entries) == 0 {
		// A statement with zero :61: lines is legal (no movement) — keep
		// it, but the balances must still be present (checked above).
	}
	if st.Currency == "" {
		return nil, fmt.Errorf("%s: no currency derivable (no :60F:/:62F:/:34F:/:90x:)", format)
	}
	if st.StatementDate.IsZero() && st.ClosingBalance != nil {
		st.StatementDate = time.Now().UTC()
	}
	return st, nil
}

// swiftTag is one extracted ":TAG:value" pair.
type swiftTag struct {
	tag   string
	value string
}

// extractSwiftTags splits a FIN message body into ordered tags. A tag
// starts at a line boundary ":TAG:"; the value runs to the next tag.
func extractSwiftTags(body string) []swiftTag {
	// Normalize CRLF → LF; find the text block ({4:/:4: ... -}) if present.
	// `-}` terminates the FIN text block and can never legitimately occur
	// inside a tag value, so it is stripped unconditionally.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if end := strings.Index(body, "\n-}"); end >= 0 {
		body = body[:end]
	}
	if idx := strings.Index(body, "{4:"); idx >= 0 {
		body = body[idx:]
	} else if idx := strings.Index(body, ":4:"); idx >= 0 {
		body = body[idx:]
	}
	var tags []swiftTag
	var cur *swiftTag
	for _, line := range strings.Split(body, "\n") {
		if isTagLine(line) {
			if cur != nil {
				tags = append(tags, *cur)
			}
			colon := strings.Index(line[1:], ":")
			cur = &swiftTag{tag: line[1 : 1+colon], value: strings.TrimSpace(line[1+colon+1:])}
		} else if cur != nil {
			cur.value += "\n" + line
		}
	}
	if cur != nil {
		tags = append(tags, *cur)
	}
	return tags
}

func isTagLine(line string) bool {
	if len(line) < 4 || line[0] != ':' {
		return false
	}
	rest := line[1:]
	colon := strings.Index(rest, ":")
	if colon <= 0 || colon > 4 {
		return false
	}
	tag := rest[:colon]
	for _, c := range tag {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// parseStatementNumber parses "NNNNN[/SSSSS]" (:28C:).
func parseStatementNumber(v string) (string, *int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil, fmt.Errorf("empty statement number")
	}
	parts := strings.SplitN(v, "/", 2)
	num := parts[0]
	var seq *int
	if len(parts) == 2 {
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return num, nil, fmt.Errorf("bad sequence %q", parts[1])
		}
		seq = &n
	}
	return num, seq, nil
}

// parseSwiftBalance parses "[R][CD]YYMMDDCCYamount," → currency, amount,
// date. D marks a debit balance (negative).
func parseSwiftBalance(v string) (ccy string, amt decimal.Decimal, date time.Time, err error) {
	v = strings.TrimSpace(v)
	if len(v) < 11 {
		return "", amt, date, fmt.Errorf("balance too short: %q", v)
	}
	i := 0
	if v[i] == 'R' { // reversal flag on balances is rare but legal
		i++
	}
	if i >= len(v) || (v[i] != 'C' && v[i] != 'D') {
		return "", amt, date, fmt.Errorf("bad C/D mark in %q", v)
	}
	debit := v[i] == 'D'
	i++
	if len(v)-i < 10 {
		return "", amt, date, fmt.Errorf("balance too short: %q", v)
	}
	date, err = parseSwiftDate(v[i : i+6])
	if err != nil {
		return "", amt, date, fmt.Errorf("bad date %q", v[i:i+6])
	}
	i += 6
	ccy = v[i : i+3]
	i += 3
	amt, err = parseSwiftAmount(v[i:])
	if err != nil {
		return "", amt, date, err
	}
	if debit {
		amt = amt.Neg()
	}
	return ccy, amt, date, nil
}

// parseSwiftDate parses YYMMDD into a UTC date (2000-window).
func parseSwiftDate(s string) (time.Time, error) {
	if len(s) != 6 {
		return time.Time{}, fmt.Errorf("bad date %q", s)
	}
	yy, _ := strconv.Atoi(s[0:2])
	mm, _ := strconv.Atoi(s[2:4])
	dd, _ := strconv.Atoi(s[4:6])
	if mm < 1 || mm > 12 || dd < 1 || dd > 31 {
		return time.Time{}, fmt.Errorf("bad date %q", s)
	}
	return time.Date(2000+yy, time.Month(mm), dd, 0, 0, 0, 0, time.UTC), nil
}

// parseSwiftAmount parses "15d" SWIFT amounts (comma decimal, never
// signed) into a decimal — no float path anywhere.
func parseSwiftAmount(s string) (decimal.Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return decimal.Decimal{}, fmt.Errorf("empty amount")
	}
	return decimal.NewFromString(strings.Replace(s, ",", ".", 1))
}

// parseMT940Entry parses one :61: field:
//
//	6!n          value date YYMMDD
//	[4!n]        entry date MMDD
//	2a           D|C|RD|RC mark
//	[1!a]        funds code
//	15d          amount (comma decimal)
//	1!a3!c       transaction type + identification code (e.g. NTRF, S103)
//	16x          customer's reference
//	[//16x]      bank's reference
//	[\n34x]      supplementary details
func parseMT940Entry(v string) (*settlement.ParsedEntry, error) {
	v = strings.TrimSpace(v)
	if len(v) < 6 {
		return nil, fmt.Errorf("entry too short: %q", v)
	}
	e := &settlement.ParsedEntry{}
	var err error
	e.ValueDate, err = parseSwiftDate(v[:6])
	if err != nil {
		return nil, err
	}
	i := 6
	// Optional entry date MMDD — 4 digits followed by the D/C mark.
	if len(v)-i >= 5 && isDigits(v[i:i+4]) && isMark(v[i+4]) {
		mm, _ := strconv.Atoi(v[i : i+2])
		dd, _ := strconv.Atoi(v[i+2 : i+4])
		// entry date carries the value date's year
		e.BookingDate = time.Date(e.ValueDate.Year(), time.Month(mm), dd, 0, 0, 0, 0, time.UTC)
		i += 4
	}
	if len(v)-i < 2 || !isMark(v[i]) {
		return nil, fmt.Errorf("missing D/C mark in %q", v)
	}
	debit := v[i] == 'D'
	if v[i] == 'R' && len(v)-i >= 2 {
		e.IsReversal = true
		debit = v[i+1] == 'D'
		i++
	}
	i++
	// Optional funds code (single uppercase letter) precedes the amount.
	if len(v)-i >= 1 && v[i] >= 'A' && v[i] <= 'Z' && !isDigit(v[i]) &&
		i+1 < len(v) && isDigit(v[i+1]) {
		i++
	}
	// Amount runs to the transaction-type marker: digits+comma then a
	// letter starting 1!a3!c (N/S/F + 3 alnum).
	start := i
	for i < len(v) && (isDigit(v[i]) || v[i] == ',') {
		i++
	}
	e.Amount, err = parseSwiftAmount(v[start:i])
	if err != nil {
		return nil, err
	}
	e.Credit = !debit
	if e.IsReversal {
		e.Credit = debit // reversal flips the effective direction
	}
	// Transaction type: 1 letter + 3 alnum (NTRF/NMSC/S103/...).
	if len(v)-i >= 4 {
		e.TxCode = v[i : i+4]
		i += 4
	}
	rest := v[i:]
	// "//bankref" splits customer ref from bank ref.
	if idx := strings.Index(rest, "//"); idx >= 0 {
		e.EntryRef = strings.TrimSpace(rest[:idx])
		bank := rest[idx+2:]
		if nl := strings.Index(bank, "\n"); nl >= 0 {
			e.BankRef = strings.TrimSpace(bank[:nl])
			supp := strings.TrimSpace(bank[nl+1:])
			if supp != "" {
				e.Narrative = supp
			}
		} else {
			e.BankRef = strings.TrimSpace(bank)
		}
	} else {
		if nl := strings.Index(rest, "\n"); nl >= 0 {
			e.EntryRef = strings.TrimSpace(rest[:nl])
			supp := strings.TrimSpace(rest[nl+1:])
			if supp != "" {
				e.Narrative = supp
			}
		} else {
			e.EntryRef = strings.TrimSpace(rest)
		}
	}
	return e, nil
}

// parseMT940Remitter extracts a best-effort debtor name/account from the
// :86: narrative (the /32A/-style subfields are bank-proprietary; the
// authoritative name comes from camt Dbtr or the remitter fields when
// structured :86:?32/?59 blocks appear).
func parseMT940Remitter(narrative string) (name, account string) {
	// Structured :86: uses inline ?NN subfields: ?20-?29 purpose text,
	// ?30 creditor bank, ?31 creditor account, ?32/?33 creditor name.
	// They may appear mid-line (multi-field single-line :86:), so scan
	// the whole narrative rather than line prefixes.
	var names []string
	for i := 0; i+3 < len(narrative); i++ {
		if narrative[i] == '?' && isDigit(narrative[i+1]) && isDigit(narrative[i+2]) {
			code := narrative[i+1 : i+3]
			end := len(narrative)
			for j := i + 3; j+2 < len(narrative); j++ {
				if narrative[j] == '?' && isDigit(narrative[j+1]) && isDigit(narrative[j+2]) {
					end = j
					break
				}
			}
			val := strings.TrimSpace(narrative[i+3 : end])
			switch code {
			case "32", "33":
				names = append(names, val)
			case "31":
				account = val
			}
			i = end - 1
		}
	}
	name = strings.TrimSpace(strings.Join(names, " "))
	return name, account
}

func isDigits(s string) bool {
	for _, c := range s {
		if !isDigit(byte(c)) {
			return false
		}
	}
	return len(s) > 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isMark(c byte) bool  { return c == 'C' || c == 'D' || c == 'R' }

// StatementParsers is the registry the settlement.StatementParser seam
// binds: format → decoder.
type StatementParsers struct{}

// Parse dispatches on format (settlement.StatementParser seam).
func (StatementParsers) Parse(ctx context.Context, format string, raw []byte) (*settlement.ParsedStatement, error) {
	switch strings.ToUpper(strings.TrimSpace(format)) {
	case "MT940":
		return ParseMT940(ctx, raw)
	case "MT942":
		return ParseMT942(ctx, raw)
	case "CAMT053", "CAMT.053", "CAMT.053.001.08":
		return ParseCamt053(ctx, raw)
	default:
		return nil, excerrors.New("STATEMENT_MALFORMED",
			fmt.Sprintf("unsupported statement format %q", format))
	}
}
