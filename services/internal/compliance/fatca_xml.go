// fatca_xml.go — Phase-21 Task 21.3.22: FATCA (IRS FATCA XML v2.x /
// IGA Model 1) artifact generation. Same determinism contract as
// crs_xml.go — sorted accounts/currencies, content-derived
// MessageRefId.
package compliance

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"sort"
	"time"
)

// FATCA namespace constants (IRS FATCA XML v2.0).
const (
	ftcNS  = "urn:fatca:idessenderfile:v2.0"
	tfaNS  = "urn:oecd:ties:fatca:v2"
	sfaNS  = "urn:oecd:ties:stffatcatypes:v1"
	isoNSF = "urn:oecd:ties:isofatcatypes:v1"
)

type fatcaDoc struct {
	XMLName     xml.Name         `xml:"ftc:FATCA_OECD"`
	XmlnsFTC    string           `xml:"xmlns:ftc,attr"`
	XmlnsTFA    string           `xml:"xmlns:tfa,attr"`
	XmlnsSFA    string           `xml:"xmlns:sfa,attr"`
	XmlnsISO    string           `xml:"xmlns:iso,attr"`
	MessageSpec fatcaMessageSpec `xml:"ftc:MessageSpec"`
	Fatca       fatcaBody        `xml:"ftc:FATCA"`
}

type fatcaMessageSpec struct {
	SendingCompanyIN    string `xml:"sfa:SendingCompanyIN"`
	TransmittingCountry string `xml:"sfa:TransmittingCountry"`
	ReceivingCountry    string `xml:"sfa:ReceivingCountry"`
	MessageType         string `xml:"sfa:MessageType"`
	ReportingPeriod     string `xml:"sfa:ReportingPeriod"`
	Timestamp           string `xml:"sfa:Timestamp"`
	MessageRefID        string `xml:"sfa:MessageRefId"`
}

type fatcaBody struct {
	ReportingFI    fatcaFI             `xml:"ftc:ReportingFI"`
	ReportingGroup fatcaReportingGroup `xml:"ftc:ReportingGroup"`
}

type fatcaFI struct {
	ResCountryCode string       `xml:"ftc:ResCountryCode"`
	IN             string       `xml:"ftc:IN"` // GIIN
	Name           string       `xml:"ftc:Name"`
	Address        fatcaAddress `xml:"ftc:Address"`
	DocSpec        fatcaDocSpec `xml:"ftc:DocSpec"`
}

type fatcaDocSpec struct {
	DocTypeIndic string `xml:"ftc:DocTypeIndic"`
	DocRefID     string `xml:"ftc:DocRefId"`
}

type fatcaAddress struct {
	CountryCode string   `xml:"ftc:CountryCode"`
	Free        []string `xml:"ftc:AddressFree,omitempty"`
}

type fatcaReportingGroup struct {
	Accounts []fatcaAccountReport `xml:"ftc:AccountReport"`
}

type fatcaAccountReport struct {
	DocSpec       fatcaDocSpec    `xml:"ftc:DocSpec"`
	AccountNumber fatcaAcctNumber `xml:"ftc:AccountNumber"`
	AccountHolder fatcaHolder     `xml:"ftc:AccountHolder"`
	Balance       []fatcaAmount   `xml:"ftc:AccountBalance"`
	Payments      []fatcaPayment  `xml:"ftc:Payment,omitempty"`
}

type fatcaAcctNumber struct {
	Value string `xml:",chardata"`
	Type  string `xml:"ftc:AcctNumberType"`
}

type fatcaHolder struct {
	Individual *fatcaParty `xml:"ftc:Individual,omitempty"`
	Entity     *fatcaParty `xml:"ftc:Organisation,omitempty"`
}

type fatcaParty struct {
	ResCountryCode string       `xml:"ftc:ResCountryCode"`
	TIN            []fatcaTIN   `xml:"ftc:TIN"`
	Name           string       `xml:"ftc:Name"`
	Address        fatcaAddress `xml:"ftc:Address"`
}

type fatcaTIN struct {
	Value    string `xml:",chardata"`
	IssuedBy string `xml:"issuedBy,attr,omitempty"`
}

type fatcaAmount struct {
	Value    string `xml:",chardata"`
	CurrCode string `xml:"currCode,attr"`
}

type fatcaPayment struct {
	Type   string      `xml:"ftc:Type"` // FATCA501..504 income classes
	Amount fatcaAmount `xml:"ftc:PaymentAmount"`
}

// BuildFATCAXML emits the FATCA report for US-person accounts. The
// caller (Generate) guarantees every account carries a US TIN —
// TIN-missing accounts never reach this builder (fail-closed upstream).
func BuildFATCAXML(venue TaxVenue, year, version int,
	ts time.Time, accts []reportableAccount) ([]byte, error) {

	doc := fatcaDoc{
		XmlnsFTC: ftcNS, XmlnsTFA: tfaNS, XmlnsSFA: sfaNS, XmlnsISO: isoNSF,
		MessageSpec: fatcaMessageSpec{
			SendingCompanyIN:    venue.IN,
			TransmittingCountry: venue.Country,
			ReceivingCountry:    "US",
			MessageType:         "FATCA",
			ReportingPeriod:     fmt.Sprintf("%04d-12-31", year),
			Timestamp:           ts.Format("2006-01-02T15:04:05Z"),
		},
	}
	doc.Fatca.ReportingFI = fatcaFI{
		ResCountryCode: venue.Country,
		IN:             venue.IN,
		Name:           venue.Name,
		Address: fatcaAddress{CountryCode: venue.Country,
			Free: []string{venue.Name + " registered office"}},
		DocSpec: fatcaDocSpec{
			DocTypeIndic: "FATCA1",
			DocRefID:     fmt.Sprintf("FATCA-%04d-v%d", year, version),
		},
	}
	for i, a := range accts {
		doc.Fatca.ReportingGroup.Accounts = append(
			doc.Fatca.ReportingGroup.Accounts,
			fatcaAccount(a, year, version, i))
	}
	h := sha256.New()
	for _, a := range accts {
		fmt.Fprintf(h, "%d|%s|%s;", a.AccountID, a.HolderName, a.TIN)
	}
	doc.MessageSpec.MessageRefID = fmt.Sprintf("FATCA-US-%04d-v%d-%x",
		year, version, h.Sum(nil)[:8])

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

func fatcaAccount(a reportableAccount, year, version, idx int) fatcaAccountReport {
	party := &fatcaParty{
		ResCountryCode: a.ResidenceCountry,
		Name:           a.HolderName,
		Address: fatcaAddress{
			CountryCode: a.ResidenceCountry,
			Free:        splitLines(a.Address),
		},
		TIN: []fatcaTIN{{Value: a.TIN, IssuedBy: "US"}},
	}
	rep := fatcaAccountReport{
		DocSpec: fatcaDocSpec{
			DocTypeIndic: "FATCA1",
			DocRefID: fmt.Sprintf("FATCA-%04d-v%d-%d",
				year, version, a.AccountID),
		},
		AccountNumber: fatcaAcctNumber{Value: a.AccountNumber, Type: "FATCA605"},
	}
	if a.Entity {
		rep.AccountHolder.Entity = party
	} else {
		rep.AccountHolder.Individual = party
	}
	balances := append([]reportAmount{}, a.Balances...)
	sort.Slice(balances, func(i, j int) bool {
		return balances[i].Currency < balances[j].Currency
	})
	for _, b := range balances {
		rep.Balance = append(rep.Balance,
			fatcaAmount{Value: b.Amount, CurrCode: b.Currency})
	}
	payments := append([]reportAmount{}, a.Payments...)
	sort.Slice(payments, func(i, j int) bool {
		return payments[i].Currency < payments[j].Currency
	})
	for _, p := range payments {
		rep.Payments = append(rep.Payments, fatcaPayment{
			Type:   "FATCA504",
			Amount: fatcaAmount{Value: p.Amount, CurrCode: p.Currency},
		})
	}
	return rep
}
