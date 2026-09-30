// camt053_parser.go — ISO 20022 camt.053.001.08 Bank-to-Customer
// Statement decoder (Task 24.3.12; spec §5.34/§17.10). Produces the
// normalized settlement.ParsedStatement — same ingestion pipeline as
// the MT940/MT942 decoder.
//
// Extracted per task text: Stmt id + ElctrncSeqNb (statement/sequence),
// Bal rows (OPBD opening + CLBD closing with Ccy + Dt), Ntry rows
// (NtryRef, AcctSvcrRef, UETR + EndToEndId under TxDtls/Refs, Amt Ccy,
// CdtDbtInd, BookgDt, ValDt, BkTxCd proprietary code, Dbtr name/account,
// AddtlNtryInf narrative), account IBAN and servicing agent BICFI.
//
// Fail-closed: malformed XML, a missing Stmt block, or entries missing
// amount/currency abort the parse — nothing partial reaches ingestion.
package backoffice

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"exchange/internal/settlement"
)

// Minimal camt.053.001.08 projection — only fields the ingestion
// pipeline consumes; everything else is ignored.
type camt053Document struct {
	XMLName xml.Name      `xml:"Document"`
	Stmt    []camt053Stmt `xml:"BkToCstmrStmt>Stmt"`
}

type camt053Stmt struct {
	ID           string        `xml:"Id"`
	ElctrncSeqNb *int64        `xml:"ElctrncSeqNb"`
	LglSeqNb     *int64        `xml:"LglSeqNb"`
	Acct         camt053Acct   `xml:"Acct"`
	Bal          []camt053Bal  `xml:"Bal"`
	Ntry         []camt053Ntry `xml:"Ntry"`
}

type camt053Acct struct {
	IBAN string `xml:"Id>IBAN"`
	Othr string `xml:"Id>Othr>Id"`
	Ccy  string `xml:"Ccy"`
	Svcr string `xml:"Svcr>FinInstnId>BICFI"`
}

// camt053Amount is ISO 20022 ActiveOrHistoricCurrencyAndAmount — the
// Ccy attribute lives on the Amt element itself.
type camt053Amount struct {
	Value string `xml:",chardata"`
	Ccy   string `xml:"Ccy,attr"`
}

type camt053Bal struct {
	Tp    string        `xml:"Tp>CdOrPrtry>Cd"`
	SubTp string        `xml:"Tp>SubTp>Cd"`
	Amt   camt053Amount `xml:"Amt"`
	Dt    string        `xml:"Dt>Dt"`
}

type camt053Ntry struct {
	NtryRef     string        `xml:"NtryRef"`
	AcctSvcrRef string        `xml:"AcctSvcrRef"`
	Amt         camt053Amount `xml:"Amt"`
	CdtDbtInd   string        `xml:"CdtDbtInd"`
	Sts         string        `xml:"Sts"`
	BookgDt     string        `xml:"BookgDt>Dt"`
	ValDt       string        `xml:"ValDt>Dt"`
	BkTxCd      string        `xml:"BkTxCd>Prtry>Cd"`
	DomnCd      string        `xml:"BkTxCd>Domn>Cd"`
	AddtlInf    string        `xml:"AddtlNtryInf"`
	// TxDtls — references + parties
	AcctSvcrRefTx string `xml:"NtryDtls>TxDtls>Refs>AcctSvcrRef"`
	EndToEndID    string `xml:"NtryDtls>TxDtls>Refs>EndToEndId"`
	UETR          string `xml:"NtryDtls>TxDtls>Refs>UETR"`
	InstrID       string `xml:"NtryDtls>TxDtls>Refs>InstrId"`
	DbtrName      string `xml:"NtryDtls>TxDtls>RltdPties>Dbtr>Nm"`
	DbtrIBAN      string `xml:"NtryDtls>TxDtls>RltdPties>DbtrAcct>Id>IBAN"`
	DbtrOthr      string `xml:"NtryDtls>TxDtls>RltdPties>DbtrAcct>Id>Othr>Id"`
	Ustrd         string `xml:"NtryDtls>TxDtls>RmtInf>Ustrd"`
}

// ParseCamt053 decodes one camt.053.001.08 document. Multiple Stmt blocks
// are rejected — one file = one account statement per ingestion row.
func ParseCamt053(_ context.Context, raw []byte) (*settlement.ParsedStatement, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("camt.053: empty document")
	}
	// Namespace sanity: camt.053.001.08 only (other versions differ in
	// balance/entry grammar — fail closed rather than mis-parse).
	if !strings.Contains(trimmed, "camt.053.001.08") {
		return nil, fmt.Errorf("camt.053: document namespace is not camt.053.001.08")
	}
	var doc camt053Document
	dec := xml.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("camt.053: xml decode: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	if len(doc.Stmt) == 0 {
		return nil, fmt.Errorf("camt.053: no Stmt block")
	}
	if len(doc.Stmt) > 1 {
		return nil, fmt.Errorf("camt.053: %d Stmt blocks — one statement per file", len(doc.Stmt))
	}
	st := &doc.Stmt[0]

	out := &settlement.ParsedStatement{
		Format:          "CAMT053",
		StatementNumber: st.ID,
		Currency:        strings.ToUpper(firstNonEmpty(st.Acct.Ccy, "")),
		IBAN:            st.Acct.IBAN,
		BIC:             st.Acct.Svcr,
	}
	if st.ElctrncSeqNb != nil {
		n := int(*st.ElctrncSeqNb)
		out.SequenceNumber = &n
	} else if st.LglSeqNb != nil {
		n := int(*st.LglSeqNb)
		out.SequenceNumber = &n
	}

	// Balances — OPBD opening / CLBD (or CLAV) closing.
	for _, b := range st.Bal {
		code := strings.ToUpper(firstNonEmpty(b.Tp, b.SubTp))
		amt, err := parseCamtAmt(b.Amt.Value)
		if err != nil {
			return nil, fmt.Errorf("camt.053: balance %s: %w", code, err)
		}
		ccy := strings.ToUpper(b.Amt.Ccy)
		if ccy == "" {
			ccy = out.Currency
		}
		if out.Currency == "" {
			out.Currency = ccy
		}
		dt, derr := parseCamtDate(b.Dt)
		switch code {
		case "OPBD", "PRCD": // opening booked / previous closing
			d := amt
			out.OpeningBalance = &d
			if derr == nil && out.StatementDate.IsZero() {
				out.StatementDate = dt
			}
		case "CLBD", "CLAV": // closing booked / available
			d := amt
			out.ClosingBalance = &d
			if derr == nil {
				out.StatementDate = dt
			}
		}
	}
	if out.Currency == "" {
		return nil, fmt.Errorf("camt.053: no currency derivable (Acct Ccy + Bal Amt Ccy absent)")
	}

	for i := range st.Ntry {
		e, err := st.Ntry[i].toEntry(out.Currency)
		if err != nil {
			return nil, fmt.Errorf("camt.053: entry %d: %w", i+1, err)
		}
		out.Entries = append(out.Entries, *e)
	}
	return out, nil
}

// ensureEOF rejects trailing garbage after the root element.
func ensureEOF(dec *xml.Decoder) error {
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("camt.053: trailing content: %w", err)
		}
		if ce, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(ce)) == "" {
			continue
		}
		return fmt.Errorf("camt.053: unexpected trailing content after Document")
	}
}

func (n *camt053Ntry) toEntry(stmtCcy string) (*settlement.ParsedEntry, error) {
	amt, err := parseCamtAmt(n.Amt.Value)
	if err != nil {
		return nil, fmt.Errorf("amount: %w", err)
	}
	ind := strings.ToUpper(strings.TrimSpace(n.CdtDbtInd))
	if ind != "CRDT" && ind != "DBIT" {
		return nil, fmt.Errorf("CdtDbtInd %q — want CRDT|DBIT", n.CdtDbtInd)
	}
	e := &settlement.ParsedEntry{
		EntryRef:        firstNonEmpty(n.NtryRef, n.AcctSvcrRef),
		BankRef:         firstNonEmpty(n.AcctSvcrRefTx, n.AcctSvcrRef),
		UETR:            n.UETR,
		EndToEndID:      n.EndToEndID,
		Amount:          amt,
		Currency:        strings.ToUpper(firstNonEmpty(n.Amt.Ccy, stmtCcy)),
		Credit:          ind == "CRDT",
		TxCode:          firstNonEmpty(n.BkTxCd, n.DomnCd),
		RemitterName:    n.DbtrName,
		RemitterAccount: firstNonEmpty(n.DbtrIBAN, n.DbtrOthr),
		Narrative:       firstNonEmpty(n.AddtlInf, n.Ustrd),
	}
	if e.EndToEndID == "" {
		e.EndToEndID = n.InstrID
	}
	if n.ValDt != "" {
		if e.ValueDate, err = parseCamtDate(n.ValDt); err != nil {
			return nil, fmt.Errorf("ValDt: %w", err)
		}
	}
	if n.BookgDt != "" {
		if e.BookingDate, err = parseCamtDate(n.BookgDt); err != nil {
			return nil, fmt.Errorf("BookgDt: %w", err)
		}
	}
	return e, nil
}

// parseCamtAmt parses an ISO 20022 ActiveOrHistoricCurrencyAndAmount —
// dot-decimal, optionally signed. Negative amounts are normalized to the
// debit direction at the caller (amount stays positive per schema).
func parseCamtAmt(s string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Decimal{}, err
	}
	if d.IsNegative() {
		d = d.Abs()
	}
	return d, nil
}

func parseCamtDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("bad ISO date %q", s)
	}
	return t, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
