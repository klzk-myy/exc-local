// statement_parser_test.go — Task 24.3.12 format-decoder coverage:
// MT940 (full statement + reversal line), MT942 (interim report),
// camt.053.001.08 (Stmt/Bal/Ntry with UETR + EndToEndId), and the
// fail-closed paths (missing mandatory tags, malformed XML, wrong
// namespace).
package backoffice

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

const sampleMT940 = `{1:F01FRNYUS33AXXX0000000000}{2:I940DEUTDEFFXXXXN}{4:
:20:STATEMENT-2025-06-10
:25:US99TEST/12345
:28C:00042/1
:60F:C250609USD1000,00
:61:2506100610C50,25NTRFEXCREF001//BANKREF77
:86:?32Mystery Remitter?31US44ACCT
:61:250610D100,00NMSCPAYREF002//BANKREF88
:61:250610RD25,50NTRFRVREF003//BANKREF99
:62F:C250610USD950,25
-}`

func TestMT940Parse(t *testing.T) {
	st, err := ParseMT940(context.Background(), []byte(sampleMT940))
	if err != nil {
		t.Fatalf("ParseMT940: %v", err)
	}
	if st.Format != "MT940" || st.StatementNumber != "00042" {
		t.Fatalf("header: %+v", st)
	}
	if st.SequenceNumber == nil || *st.SequenceNumber != 1 {
		t.Fatalf("sequence: %+v", st.SequenceNumber)
	}
	if st.IBAN == "" || st.Currency != "USD" {
		t.Fatalf("account/currency: %+v", st)
	}
	if st.OpeningBalance == nil || !st.OpeningBalance.Equal(decimal.RequireFromString("1000.00")) {
		t.Fatalf("opening: %v", st.OpeningBalance)
	}
	if st.ClosingBalance == nil || !st.ClosingBalance.Equal(decimal.RequireFromString("950.25")) {
		t.Fatalf("closing: %v", st.ClosingBalance)
	}
	if len(st.Entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(st.Entries))
	}
	credit := st.Entries[0]
	if !credit.Credit || !credit.Amount.Equal(decimal.RequireFromString("50.25")) ||
		credit.EntryRef != "EXCREF001" || credit.BankRef != "BANKREF77" ||
		credit.TxCode != "NTRF" || credit.RemitterName != "Mystery Remitter" {
		t.Fatalf("credit entry: %+v", credit)
	}
	debit := st.Entries[1]
	if debit.Credit || !debit.Amount.Equal(decimal.RequireFromString("100.00")) {
		t.Fatalf("debit entry: %+v", debit)
	}
	// Reversal line: RD marks a reversal — effective direction flips to credit.
	rev := st.Entries[2]
	if !rev.IsReversal || !rev.Credit || !rev.Amount.Equal(decimal.RequireFromString("25.50")) {
		t.Fatalf("reversal line must flip to credit: %+v", rev)
	}
}

func TestMT940MissingMandatoryTagFailsClosed(t *testing.T) {
	_, err := ParseMT940(context.Background(), []byte("{4:\n:20:X\n:25:IBAN\n:28C:1/1\n-}"))
	if err == nil || !strings.Contains(err.Error(), "missing mandatory tag") {
		t.Fatalf("missing :60F: must fail: %v", err)
	}
}

func TestMT942Parse(t *testing.T) {
	raw := `{4:
:20:INTR-001
:25:US99TEST/12345
:28C:00042/2
:34F:USD100,00
:13D:2506101430+0000
:61:250610C500,00NTRFMIDDAY01//BK1
:90D:1USD0,00
:90C:1USD500,00
-}`
	st, err := ParseMT942(context.Background(), []byte(raw))
	if err != nil {
		t.Fatalf("ParseMT942: %v", err)
	}
	if st.Format != "MT942" || len(st.Entries) != 1 || !st.Entries[0].Credit {
		t.Fatalf("interim report: %+v", st)
	}
	if st.OpeningBalance != nil || st.ClosingBalance != nil {
		t.Fatal("MT942 carries no balances")
	}
}

const sampleCamt053 = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08">
<BkToCstmrStmt>
<Stmt>
<Id>STMT-2025-06-10</Id><ElctrncSeqNb>42</ElctrncSeqNb>
<Acct><Id><IBAN>US99TEST1234</IBAN></Id><Ccy>USD</Ccy>
<Svcr><FinInstnId><BICFI>FRNYUS33</BICFI></FinInstnId></Svcr></Acct>
<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">1000.00</Amt><Dt><Dt>2025-06-09</Dt></Dt></Bal>
<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">1500.00</Amt><Dt><Dt>2025-06-10</Dt></Dt></Bal>
<Ntry><NtryRef>NTRY001</NtryRef><AcctSvcrRef>SVCR77</AcctSvcrRef>
<Amt Ccy="USD">500.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts>
<BookgDt><Dt>2025-06-10</Dt></BookgDt><ValDt><Dt>2025-06-10</Dt></ValDt>
<BkTxCd><Prtry><Cd>TRF+STD</Cd></Prtry></BkTxCd>
<NtryDtls><TxDtls>
<Refs><EndToEndId>E2E-ABC</EndToEndId><UETR>eb6305c9-1f7f-49de-aed0-16487c27b42d</UETR></Refs>
<RltdPties><Dbtr><Nm>Counterparty LLC</Nm></Dbtr>
<DbtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></DbtrAcct></RltdPties>
<RmtInf><Ustrd>Invoice 42 settlement</Ustrd></RmtInf>
</TxDtls></NtryDtls></Ntry>
</Stmt>
</BkToCstmrStmt>
</Document>`

func TestCamt053Parse(t *testing.T) {
	st, err := ParseCamt053(context.Background(), []byte(sampleCamt053))
	if err != nil {
		t.Fatalf("ParseCamt053: %v", err)
	}
	if st.Format != "CAMT053" || st.StatementNumber != "STMT-2025-06-10" {
		t.Fatalf("header: %+v", st)
	}
	if st.SequenceNumber == nil || *st.SequenceNumber != 42 {
		t.Fatalf("sequence: %+v", st.SequenceNumber)
	}
	if st.IBAN != "US99TEST1234" || st.BIC != "FRNYUS33" || st.Currency != "USD" {
		t.Fatalf("account fields: %+v", st)
	}
	if st.OpeningBalance == nil || !st.OpeningBalance.Equal(decimal.RequireFromString("1000.00")) {
		t.Fatalf("opening: %v", st.OpeningBalance)
	}
	if st.ClosingBalance == nil || !st.ClosingBalance.Equal(decimal.RequireFromString("1500.00")) {
		t.Fatalf("closing: %v", st.ClosingBalance)
	}
	if len(st.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(st.Entries))
	}
	e := st.Entries[0]
	if e.UETR != "eb6305c9-1f7f-49de-aed0-16487c27b42d" ||
		e.EndToEndID != "E2E-ABC" || e.BankRef != "SVCR77" ||
		!e.Credit || !e.Amount.Equal(decimal.RequireFromString("500.00")) ||
		e.RemitterName != "Counterparty LLC" || e.TxCode != "TRF+STD" {
		t.Fatalf("entry: %+v", e)
	}
}

func TestCamt053RejectsWrongNamespace(t *testing.T) {
	raw := strings.Replace(sampleCamt053, "camt.053.001.08", "camt.052.001.08", 1)
	if _, err := ParseCamt053(context.Background(), []byte(raw)); err == nil {
		t.Fatal("non-053.001.08 namespace must be rejected")
	}
}

func TestCamt053MalformedFailsClosed(t *testing.T) {
	if _, err := ParseCamt053(context.Background(), []byte("<Document><broken")); err == nil {
		t.Fatal("malformed XML must fail")
	}
	if _, err := ParseCamt053(context.Background(), []byte("")); err == nil {
		t.Fatal("empty document must fail")
	}
}
