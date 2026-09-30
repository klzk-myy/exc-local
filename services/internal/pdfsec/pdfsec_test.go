package pdfsec

import (
	"bytes"
	"strings"
	"testing"

	"exchange/internal/reporting"
)

func renderSample() []byte {
	return reporting.RenderPDFDoc([]reporting.PDFLine{
		{Text: "Trade Confirmation — acct 42", Size: 12},
		{Text: "EUR/USD BUY 100,000 @ 1.0850 (commission $5.00)", Size: 10},
		{Text: "Settlement date: T+2  2026-10-02", Size: 10},
	})
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plain := renderSample()
	enc, err := EncryptPDF(plain, "acct-pin-42", "owner-secret")
	if err != nil {
		t.Fatalf("EncryptPDF: %v", err)
	}
	if !bytes.Contains(enc, []byte("/Encrypt")) {
		t.Fatal("encrypted doc lacks /Encrypt trailer entry")
	}
	if !bytes.Contains(enc, []byte("/ID [<")) {
		t.Fatal("encrypted doc lacks /ID")
	}
	if bytes.Contains(enc, []byte("commission")) {
		t.Fatal("plaintext leaked into encrypted document")
	}
	dec, err := DecryptPDF(enc, "acct-pin-42")
	if err != nil {
		t.Fatalf("DecryptPDF: %v", err)
	}
	// Decrypted stream must restore the original text operators.
	for _, want := range []string{"Trade Confirmation", "commission", "1.0850"} {
		if !strings.Contains(string(dec), want) {
			t.Fatalf("decrypted doc missing %q", want)
		}
	}
}

func TestEncryptRequiresPassword(t *testing.T) {
	if _, err := EncryptPDF(renderSample(), "", ""); err == nil {
		t.Fatal("empty password must fail closed")
	}
	if _, err := EncryptPDF([]byte("not a pdf"), "x", ""); err == nil {
		t.Fatal("non-PDF input must fail closed")
	}
}

func TestEncryptWrongPasswordFails(t *testing.T) {
	enc, err := EncryptPDF(renderSample(), "right", "")
	if err != nil {
		t.Fatalf("EncryptPDF: %v", err)
	}
	dec, err := DecryptPDF(enc, "wrong")
	if err == nil {
		if strings.Contains(string(dec), "commission") {
			t.Fatal("wrong password produced valid plaintext")
		}
	}
}
