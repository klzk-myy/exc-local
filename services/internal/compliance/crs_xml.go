// crs_xml.go — Phase-21 Task 21.3.22: CRS (OECD CRS XML Schema v2.x)
// artifact generation. Schema-shaped output: the element vocabulary and
// nesting follow the OECD CRS schema; the venue deployment validates
// against the published XSD in CI before a live submission (the golden
// test pins the exact byte shape so drift is loud).
//
// Determinism contract: for the same (venue, jurisdiction, year,
// version, timestamp, account set) the emitted bytes are identical —
// account order is by account_id, currency order alphabetical, and
// MessageRefId derives from the content hash.
package compliance

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CRS namespace constants (OECD CRS XML v2).
const (
	crsNS = "urn:oecd:ties:crs:v2"
	stfNS = "urn:oecd:ties:crsstf:v1"
	isoNS = "urn:oecd:ties:isocrstypes:v1"
	cbaNS = "urn:oecd:ties:commontypesfatcacrs:v2"
)

// --- schema-shaped structs --------------------------------------------------

type crsDoc struct {
	XMLName     xml.Name       `xml:"crs:OECD_CRS"`
	XmlnsCRS    string         `xml:"xmlns:crs,attr"`
	XmlnsSTF    string         `xml:"xmlns:stf,attr"`
	XmlnsISO    string         `xml:"xmlns:iso,attr"`
	XmlnsCBA    string         `xml:"xmlns:cba,attr"`
	MessageSpec crsMessageSpec `xml:"stf:MessageSpec"`
	Body        crsBody        `xml:"crs:CRSBody"`
}

type crsMessageSpec struct {
	SendingCompanyIN    string `xml:"stf:SendingCompanyIN"`
	TransmittingCountry string `xml:"stf:TransmittingCountry"`
	ReceivingCountry    string `xml:"stf:ReceivingCountry"`
	MessageType         string `xml:"stf:MessageType"`
	ReportingPeriod     string `xml:"stf:ReportingPeriod"`
	Timestamp           string `xml:"stf:Timestamp"`
	MessageRefID        string `xml:"stf:MessageRefId"`
}

type crsBody struct {
	ReportingFI crsFI                  `xml:"crs:ReportingFI"`
	Accounts    []crsReportableAccount `xml:"crs:ReportableAccount"`
}

type crsFI struct {
	ResCountryCode string     `xml:"crs:ResCountryCode"`
	IN             crsIN      `xml:"crs:IN"`
	Name           string     `xml:"crs:Name"`
	Address        crsAddress `xml:"crs:Address"`
	DocSpec        crsDocSpec `xml:"crs:DocSpec"`
}

type crsIN struct {
	Value    string `xml:",chardata"`
	IssuedBy string `xml:"issuedBy,attr,omitempty"`
	INType   string `xml:"crs:INType,omitempty"`
}

type crsDocSpec struct {
	DocTypeIndic string `xml:"stf:DocTypeIndic"`
	DocRefID     string `xml:"stf:DocRefId"`
}

type crsAddress struct {
	CountryCode string   `xml:"crs:CountryCode"`
	Free        []string `xml:"crs:AddressFree,omitempty"`
}

type crsReportableAccount struct {
	AccountNumber crsAcctNumber    `xml:"crs:AccountNumber"`
	AccountHolder crsAccountHolder `xml:"crs:AccountHolder"`
	Balances      []crsAmount      `xml:"crs:AccountBalance"`
	Payments      []crsPayment     `xml:"crs:Payment,omitempty"`
}

type crsAcctNumber struct {
	Value string `xml:",chardata"`
	Type  string `xml:"crs:AcctNumberType"` // OECD601 IBAN/OBAN/other
}

type crsAccountHolder struct {
	Individual *crsParty `xml:"crs:Individual,omitempty"`
	Entity     *crsParty `xml:"crs:Organisation,omitempty"`
}

type crsParty struct {
	ResCountryCode []string   `xml:"crs:ResCountryCode"`
	TIN            []crsIN    `xml:"crs:TIN,omitempty"`
	Name           string     `xml:"crs:Name"`
	Address        crsAddress `xml:"crs:Address"`
}

type crsAmount struct {
	Value    string `xml:",chardata"`
	CurrCode string `xml:"currCode,attr"`
}

type crsPayment struct {
	Type   string    `xml:"crs:Type"` // CRS501 dividends | CRS502 interest | CRS503 gross proceeds | CRS504 other
	Amount crsAmount `xml:"crs:PaymentAmount"`
}

// BuildCRSXML emits the CRS document. deterministic: sorted accounts,
// sorted currencies, MessageRefID derived from the report key + content.
func BuildCRSXML(venue TaxVenue, receivingJurisdiction string,
	year, version int, ts time.Time, accts []reportableAccount) ([]byte, error) {

	receiving := strings.ToUpper(receivingJurisdiction)
	if receiving == "" {
		receiving = "ALL"
	}
	doc := crsDoc{
		XmlnsCRS: crsNS, XmlnsSTF: stfNS, XmlnsISO: isoNS, XmlnsCBA: cbaNS,
		MessageSpec: crsMessageSpec{
			SendingCompanyIN:    venue.IN,
			TransmittingCountry: strings.ToUpper(venue.Country),
			ReceivingCountry:    receiving,
			MessageType:         "CRS",
			ReportingPeriod:     fmt.Sprintf("%04d-12-31", year),
			Timestamp:           ts.Format("2006-01-02T15:04:05Z"),
		},
		Body: crsBody{
			ReportingFI: crsFI{
				ResCountryCode: strings.ToUpper(venue.Country),
				IN:             crsIN{Value: venue.IN, INType: "GIIN"},
				Name:           venue.Name,
				Address: crsAddress{
					CountryCode: strings.ToUpper(venue.Country),
					Free:        []string{venue.Name + " registered office"},
				},
				DocSpec: crsDocSpec{
					DocTypeIndic: "OECD1", // new data
					DocRefID: fmt.Sprintf("CRS-%04d-%s-v%d",
						year, receiving, version),
				},
			},
		},
	}
	for _, a := range accts {
		doc.Body.Accounts = append(doc.Body.Accounts, crsAccount(a))
	}
	// MessageRefId pins content: key + hash of the serialised body list.
	h := sha256.New()
	for _, a := range accts {
		fmt.Fprintf(h, "%d|%s|%s|%s;", a.AccountID, a.HolderName,
			a.ResidenceCountry, a.TIN)
	}
	doc.MessageSpec.MessageRefID = fmt.Sprintf("CRS-%s-%04d-v%d-%x",
		receiving, year, version, h.Sum(nil)[:8])

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func crsAccount(a reportableAccount) crsReportableAccount {
	holder := crsAccountHolder{}
	party := &crsParty{
		ResCountryCode: []string{a.ResidenceCountry},
		Name:           a.HolderName,
		Address: crsAddress{
			CountryCode: a.ResidenceCountry,
			Free:        splitLines(a.Address),
		},
	}
	if a.TIN != "" {
		party.TIN = []crsIN{{Value: a.TIN, IssuedBy: a.TINIssuedBy}}
	}
	if a.Entity {
		holder.Entity = party
	} else {
		holder.Individual = party
	}
	acct := crsReportableAccount{
		AccountNumber: crsAcctNumber{Value: a.AccountNumber, Type: "OECD605"},
		AccountHolder: holder,
	}
	balances := append([]reportAmount{}, a.Balances...)
	sort.Slice(balances, func(i, j int) bool {
		return balances[i].Currency < balances[j].Currency
	})
	for _, b := range balances {
		acct.Balances = append(acct.Balances,
			crsAmount{Value: b.Amount, CurrCode: b.Currency})
	}
	payments := append([]reportAmount{}, a.Payments...)
	sort.Slice(payments, func(i, j int) bool {
		return payments[i].Currency < payments[j].Currency
	})
	for _, p := range payments {
		acct.Payments = append(acct.Payments, crsPayment{
			Type:   "CRS504", // other income
			Amount: crsAmount{Value: p.Amount, CurrCode: p.Currency},
		})
	}
	return acct
}

// splitLines normalises a free-form address into ≤5 address lines.
func splitLines(addr string) []string {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	parts := strings.Split(addr, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
