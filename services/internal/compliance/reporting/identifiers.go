package reporting

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Identifier generation + validation (Task 21.3.14, spec §14.1a/§14.11):
//   UTI — ISO 23897: {issuer LEI (20)}{local key (≤32)} — total ≤ 52 chars
//   USI — CFTC: {namespace}{local id} (namespace registered with the CFTC)
//   UPI — ISO 4914: 12-char OTC derivative product identifier (DSB-issued;
//         a deterministic local placeholder is generated until the DSB
//         product-definition API is integrated — the resolver seam is the
//         honest boundary)
//   LEI — ISO 17442: 18 alnum + 2 mod-97-2 check digits
// ---------------------------------------------------------------------------

var (
	reUTI = regexp.MustCompile(`^[A-Z0-9]{20}[A-Z0-9]{1,32}$`)
	reUSI = regexp.MustCompile(`^[A-Z0-9]{1,10}[A-Z0-9]{1,42}$`)
	reUPI = regexp.MustCompile(`^[A-Z0-9]{12}$`)
	reLEI = regexp.MustCompile(`^[0-9A-Z]{18}[0-9]{2}$`)
	reMIC = regexp.MustCompile(`^[A-Z0-9]{4}$`)
)

// TestVenueLEI is the synthetic LEI used when no venue LEI is configured
// — 20 chars, ISO 17442-format with a valid mod-97-2 checksum so
// generated UTIs pass structural validation, clearly a test identifier
// (the "00" check-digit pair + TEST body). Production MUST configure
// EXC_VENUE_LEI with the entity's registered LEI; a real submission with
// the test LEI is a configuration defect.
const TestVenueLEI = "213800TESTTEST000042"

// DefaultVenueMIC is the segment MIC used when EXC_VENUE_MIC is unset —
// the venue is a firm-operated FX ECN; XOFF is the ISO 10383 code for
// off-exchange/bilateral and is wrong for our own venue reports, so the
// dev default is the synthetic venue MIC "XEXC". Production pins the
// assigned MIC via env.
const DefaultVenueMIC = "XEXC"

// TestUSINamespace is the dev CFTC USI namespace (≤10 chars, alnum).
const TestUSINamespace = "EXC"

// ValidateLEI applies the ISO 17442 format + mod-97-2 checksum.
func ValidateLEI(lei string) error {
	if !reLEI.MatchString(lei) {
		return fmt.Errorf("lei %q: not 18 alnum + 2 check digits", lei)
	}
	// mod-97-2: numeric expansion (A=10…Z=35) mod 97 must equal 1.
	var b strings.Builder
	for _, r := range lei {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteString(strconv.Itoa(int(r - 'A' + 10)))
		}
	}
	// Large-number mod: carry the remainder digit-wise.
	rem := 0
	for _, r := range b.String() {
		rem = (rem*10 + int(r-'0')) % 97
	}
	if rem != 1 {
		return fmt.Errorf("lei %q: ISO 17442 checksum failed (mod97=%d)", lei, rem)
	}
	return nil
}

// ValidateUTI enforces ISO 23897: 20-char issuer id + 1–32 local chars.
func ValidateUTI(uti string) error {
	if !reUTI.MatchString(uti) {
		return fmt.Errorf("uti %q: invalid ISO 23897 format", uti)
	}
	return nil
}

// ValidateUSI enforces the CFTC USI shape (namespace + transaction id).
func ValidateUSI(usi string) error {
	if !reUSI.MatchString(usi) {
		return fmt.Errorf("usi %q: invalid CFTC USI format", usi)
	}
	return nil
}

// ValidateUPI enforces ISO 4914: 12 alphanumeric characters.
func ValidateUPI(upi string) error {
	if !reUPI.MatchString(upi) {
		return fmt.Errorf("upi %q: invalid ISO 4914 format", upi)
	}
	return nil
}

// ValidateMIC enforces ISO 10383 (4 alphanumeric).
func ValidateMIC(mic string) error {
	if !reMIC.MatchString(mic) {
		return fmt.Errorf("mic %q: invalid ISO 10383 format", mic)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Generators
// ---------------------------------------------------------------------------

// b36 upper-cases a positive int64 into base-36 (0-9A-Z) — the
// deterministic local-key alphabet for UTI/USI local parts.
func b36(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		d := n % 36
		if d < 10 {
			b[i] = byte('0' + d)
		} else {
			b[i] = byte('A' + d - 10)
		}
		n /= 36
	}
	return string(b[i:])
}

// UTIFor mints the venue-issued ISO 23897 UTI for a reportable event:
// venueLEI (20) + zero-padded base-36 local key derived deterministically
// from (scope, id) — collision-reject is enforced downstream by the
// (uti, regime, report_seq) unique key plus the ID_COLLISION reconciler.
// scope/id must be stable for the trade (e.g. "TRADE", trade_id) so a
// replayed fill regenerates the same UTI (idempotent re-derivation).
func UTIFor(venueLEI, scope string, id int64) string {
	local := b36(int64(len(scope))) + scope + b36(id)
	if len(local) > 32 {
		local = local[len(local)-32:]
	}
	return venueLEI + local
}

// USIFor mints the CFTC USI: {namespace}{local id}. The namespace is the
// venue's CFTC-registered prefix; the local id is the trade's deterministic
// key, distinct from the UTI local part by the namespace prefix itself.
func USIFor(namespace, scope string, id int64) string {
	local := b36(int64(len(scope))) + scope + b36(id)
	if len(local) > 42 {
		local = local[len(local)-42:]
	}
	return namespace + local
}

// upiClassPrefix maps an instrument_type to the DSB-style product prefix
// letter for the locally-minted UPI placeholder.
func upiClassPrefix(instrumentType string) byte {
	switch strings.ToUpper(instrumentType) {
	case "FORWARD":
		return 'J' // DSB FX forward UPIs are CFI 'J*' — keep the class letter
	case "SWAP":
		return 'S'
	case "NDF":
		return 'N'
	case "OPTION":
		return 'O'
	default:
		return 'X'
	}
}

// UPIFor mints a deterministic 12-char local UPI for a derivative
// product: [class letter][9-char base36 sha256 fragment][2 check digits].
// This is a LOCAL identifier pending DSB API integration (the UPIResolver
// seam replaces it with the DSB-issued code when available); it is stable
// per product tuple so reconciliation can compare identifier collisions.
func UPIFor(instrumentType, base, quote string) string {
	h := sha256.Sum256([]byte(instrumentType + "|" + base + "|" + quote))
	var n int64
	for i := 0; i < 8; i++ {
		n = n<<8 | int64(h[i])
	}
	if n < 0 {
		n = -n
	}
	body := fmt.Sprintf("%036s", b36(n))
	body = body[len(body)-9:]
	prefix := string(upiClassPrefix(instrumentType)) + body
	// 2-digit check: sum of char values mod 97, zero-padded.
	sum := 0
	for _, r := range prefix {
		switch {
		case r >= '0' && r <= '9':
			sum += int(r - '0')
		default:
			sum += int(r-'A') + 10
		}
	}
	return fmt.Sprintf("%s%02d", prefix, sum%97)
}
