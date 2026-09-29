// Tax self-certification intake — Task 12.3.13, migration 205.
//
// Onboarding collects IRS self-certifications (W-8BEN / W-8BEN-E / W-9)
// once an account reaches T1+. The table is shaped for direct
// consumption by Phase-21 Task 21.3.22 CRS/FATCA reporting — fields JSONB
// carries the legal name / address / entity-type / treaty-claim payload
// verbatim, no joins or re-shaping.
//
// TIN validation policy (honest, documented):
//   - tin_country "US": 9 digits required; classified against the IRS
//     SSN (excluded ranges), EIN (9-digit, any prefix — format only) and
//     ITIN (9XX + valid middle ranges) patterns. tin_kind records the
//     matched class; "SSN" and "EIN" are format-indistinguishable in
//     isolation, so an explicit tin_kind input disambiguates.
//   - non-US tin_country: pass-through — the TIN is recorded as
//     submitted with tin_kind empty and a PASSTHROUGH_NON_US marker in
//     fields["tin_validation"]. Jurisdiction-specific format rules are a
//     Phase-21 concern; this intake never fabricates a validation claim.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SelfCert mirrors one tax_self_certifications row.
type SelfCert struct {
	ID             int64           `json:"id"`
	AccountID      int64           `json:"account_id"`
	FormType       string          `json:"form_type"` // W-8BEN | W-8BEN-E | W-9
	TIN            string          `json:"tin,omitempty"`
	TINCountry     string          `json:"tin_country,omitempty"`
	TINKind        string          `json:"tin_kind,omitempty"` // SSN|EIN|ITIN — US-validated only
	Fields         json.RawMessage `json:"fields"`             // JSONB verbatim
	Status         string          `json:"status"`
	TINValidatedAt *time.Time      `json:"tin_validated_at,omitempty"`
	ValidatedAt    *time.Time      `json:"validated_at,omitempty"`
	SupersededBy   *int64          `json:"superseded_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// SelfCertInput is the POST body contract.
type SelfCertInput struct {
	AccountID  int64
	FormType   string
	TIN        string
	TINCountry string
	TINKind    string          // optional explicit SSN|EIN|ITIN (W-9 disambiguation)
	Fields     json.RawMessage // JSON object — legal_name required
}

var formTypes = map[string]bool{"W-8BEN": true, "W-8BEN-E": true, "W-9": true}

var digitsOnly = regexp.MustCompile(`^[0-9]{9}$`)

func normalizeTIN(tin string) string {
	var b strings.Builder
	for _, r := range tin {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == '-' || r == ' ' {
			continue
		} else {
			return tin // non-numeric char → let the format check reject/pass-through
		}
	}
	return b.String()
}

// usTINKind classifies a 9-digit US TIN: "ITIN" for the 9xx + valid
// middle-digit ranges, "SSN" when the SSN exclusions pass and tinKind
// requested SSN or was empty, "EIN" for the remainder. ok=false when no
// pattern accepts the value.
//
// SSN exclusions (IRS/SSA): area 000/666/900–999, group 00, serial 0000.
// ITIN (IRS): 9XX where the middle pair is 70–88, 90–92, 94–99.
// EIN: any 9 digits (format check only — prefix lists are IRS-internal).
func usTINKind(digits, hint string) (kind string, ok bool) {
	if !digitsOnly.MatchString(digits) {
		return "", false
	}
	area := int(digits[0]-'0')*100 + int(digits[1]-'0')*10 + int(digits[2]-'0')
	mid := int(digits[3]-'0')*10 + int(digits[4]-'0')
	serial := digits[5:] != "0000"
	isITIN := digits[0] == '9' &&
		((mid >= 70 && mid <= 88) || (mid >= 90 && mid <= 92) || (mid >= 94 && mid <= 99))
	ssnValid := area != 0 && area != 666 && !(area >= 900 && area <= 999) &&
		mid != 0 && serial
	switch strings.ToUpper(hint) {
	case "ITIN":
		if isITIN {
			return "ITIN", true
		}
		return "", false
	case "SSN":
		if !isITIN && ssnValid {
			return "SSN", true
		}
		return "", false
	case "EIN":
		if !isITIN && digits != "000000000" {
			return "EIN", true
		}
		return "", false
	case "":
		// No hint: ITIN patterns are unambiguous; an SSN-valid value is
		// labelled SSN (EIN shares the digit shape — the filer's tin_kind
		// hint disambiguates when it matters).
		if isITIN {
			return "ITIN", true
		}
		if ssnValid {
			return "SSN", true
		}
		if digits != "000000000" {
			return "EIN", true
		}
		return "", false
	}
	return "", false
}

// ValidateSelfCert enforces the intake contract (pure — unit-tested):
//   - form_type ∈ {W-8BEN, W-8BEN-E, W-9}
//   - W-9 ⇒ tin_country "US" + US-format TIN (required)
//   - W-8* ⇒ TIN optional; "US" country validates US-format, non-US is
//     pass-through (PASSTHROUGH_NON_US recorded in fields)
//   - fields must be a JSON object containing a non-empty legal_name
func (s *Service) ValidateSelfCert(in *SelfCertInput) (*SelfCert, error) {
	ft := strings.ToUpper(strings.TrimSpace(in.FormType))
	if !formTypes[ft] {
		return nil, fmt.Errorf("%w: form_type must be W-8BEN|W-8BEN-E|W-9", ErrValidation)
	}
	if len(in.Fields) == 0 || !isJSONObjectWithLegalName(in.Fields) {
		return nil, fmt.Errorf("%w: fields must be a JSON object with a non-empty legal_name", ErrValidation)
	}

	country := strings.ToUpper(strings.TrimSpace(in.TINCountry))
	tin := strings.TrimSpace(in.TIN)
	cert := &SelfCert{
		AccountID:  in.AccountID,
		FormType:   ft,
		TINCountry: country,
		Fields:     in.Fields,
		Status:     "SUBMITTED",
		CreatedAt:  s.now().UTC(),
	}

	switch ft {
	case "W-9":
		if country != "US" {
			return nil, fmt.Errorf("%w: W-9 requires tin_country US", ErrValidation)
		}
		if tin == "" {
			return nil, fmt.Errorf("%w: W-9 requires a TIN", ErrValidation)
		}
	}
	if tin != "" && country == "US" {
		kind, ok := usTINKind(normalizeTIN(tin), in.TINKind)
		if !ok {
			return nil, fmt.Errorf("%w: US TIN fails SSN/EIN/ITIN format validation", ErrValidation)
		}
		now := s.now().UTC()
		cert.TIN = normalizeTIN(tin)
		cert.TINKind = kind
		cert.TINValidatedAt = &now
		cert.Status = "VALIDATED"
	} else if tin != "" {
		// Non-US passthrough — recorded, not validated.
		cert.TIN = tin
		now := s.now().UTC()
		cert.TINValidatedAt = &now
		cert.Status = "VALIDATED"
		cert.Fields = mergeTinMarker(cert.Fields)
	}
	return cert, nil
}

// SubmitSelfCert gates on tier (T1+ per the spec wording — the cert is
// an onboarding artifact collected after verification starts) then
// validates and inserts.
func (s *Service) SubmitSelfCert(ctx context.Context, in SelfCertInput) (*SelfCert, error) {
	tier, err := s.store.AccountTier(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	if TierRank(tier) < TierRank(TierT1) {
		return nil, fmt.Errorf("%w: self-certification requires KYC tier T1+", ErrTierTooLow)
	}
	cert, err := s.ValidateSelfCert(&in)
	if err != nil {
		return nil, err
	}
	if err := s.store.InsertSelfCert(ctx, cert); err != nil {
		return nil, err
	}
	cert.Fields = nil // rows echo through ListSelfCerts, not the write path
	return cert, nil
}

// ListSelfCerts returns the account's certification history (Phase-21
// consumes the table directly; this is the client-facing read).
func (s *Service) ListSelfCerts(ctx context.Context, accountID int64) ([]SelfCert, error) {
	return s.store.ListSelfCerts(ctx, accountID)
}

// isJSONObjectWithLegalName checks the JSONB payload is an object with a
// non-empty string legal_name — the minimum honest shape for every form.
func isJSONObjectWithLegalName(raw []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return false
	}
	name, ok := m["legal_name"].(string)
	return ok && strings.TrimSpace(name) != ""
}

// mergeTinMarker stamps the passthrough marker into fields JSON.
func mergeTinMarker(raw []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw
	}
	m["tin_validation"] = "PASSTHROUGH_NON_US"
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
