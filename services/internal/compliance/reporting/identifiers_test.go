package reporting

import (
	"strings"
	"testing"
	"time"
)

// validLEI is a real-format ISO 17442 LEI with a correct mod-97-2
// checksum (the well-known GLEIF example entity id shape).
func leiWithChecksum(t *testing.T, body18 string) string {
	t.Helper()
	if len(body18) != 18 {
		t.Fatalf("body must be 18 chars, got %d", len(body18))
	}
	// compute check digits: append "00", expand, mod 97, check = 98-rem
	var b strings.Builder
	for _, r := range body18 + "00" {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteString(itoa(int(r - 'A' + 10)))
		}
	}
	rem := 0
	for _, r := range b.String() {
		rem = (rem*10 + int(r-'0')) % 97
	}
	return body18 + pad2(98-rem)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func TestValidateLEI_Checksum(t *testing.T) {
	good := leiWithChecksum(t, "529900T8BM49AURSDO") // known-good prefix
	if err := ValidateLEI(good); err != nil {
		t.Fatalf("expected valid LEI, got %v", err)
	}
	if err := ValidateLEI("529900T8BM49AURSDO00"); err == nil {
		t.Fatal("expected checksum failure")
	}
	if err := ValidateLEI("short"); err == nil {
		t.Fatal("expected format failure")
	}
}

func TestUTIFor_DeterministicAndValid(t *testing.T) {
	lei := leiWithChecksum(t, "TESTTESTTESTTEST00")
	u1 := UTIFor(lei, "TRADE", 42)
	u2 := UTIFor(lei, "TRADE", 42)
	if u1 != u2 {
		t.Fatal("UTI must be deterministic")
	}
	if err := ValidateUTI(u1); err != nil {
		t.Fatalf("UTI format: %v", err)
	}
	if UTIFor(lei, "TRADE", 43) == u1 {
		t.Fatal("distinct trades must not collide")
	}
}

func TestUSIFor_Format(t *testing.T) {
	u := USIFor(TestUSINamespace, "TRADE", 7)
	if err := ValidateUSI(u); err != nil {
		t.Fatalf("USI format: %v", err)
	}
	if !strings.HasPrefix(u, TestUSINamespace) {
		t.Fatalf("USI must carry the namespace: %s", u)
	}
}

func TestUPIFor_FormatAndClass(t *testing.T) {
	u := UPIFor("FORWARD", "EUR", "USD")
	if err := ValidateUPI(u); err != nil {
		t.Fatalf("UPI format: %v", err)
	}
	if u[0] != 'J' {
		t.Fatalf("forward class letter: %s", u)
	}
	if UPIFor("SWAP", "EUR", "USD")[0] != 'S' {
		t.Fatal("swap class letter")
	}
	if UPIFor("FORWARD", "EUR", "USD") == UPIFor("FORWARD", "EUR", "GBP") {
		t.Fatal("distinct products must not collide")
	}
}

func TestValidateMIC(t *testing.T) {
	if err := ValidateMIC("XEXC"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMIC("TOOLONG"); err == nil {
		t.Fatal("expected MIC failure")
	}
}

func TestRTS22Deadline_T1_2359CET(t *testing.T) {
	// 2024-06-03 is a Monday — a trade at 10:00 UTC must report by
	// Tuesday 23:59 CEST (UTC+2) = 21:59 UTC.
	at := time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)
	d := RTS22Deadline(at)
	if d.UTC().Format("2006-01-02 15:04") != "2024-06-04 21:59" {
		t.Fatalf("deadline %s", d.UTC())
	}
}
