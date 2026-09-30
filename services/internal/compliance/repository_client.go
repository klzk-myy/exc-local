// Repository clients — EMIR TR (auth.030 XML → EXC_TR_URL) and CFTC
// SDR (Parts 43/45 JSON → EXC_SDR_URL). Same transport + verdict
// contract as the APA/ARM adapters; unconfigured endpoints fail closed.
package compliance

import (
	"context"
	"fmt"
	"os"
	"strings"

	"exchange/internal/compliance/reporting"
)

// TRClient submits EMIR REFIT auth.030 reports to a Trade Repository.
type TRClient struct {
	t *vendorHTTP
}

// NewTRClient constructs the client against an explicit endpoint.
func NewTRClient(endpoint string, hc vendorHTTPOpts) (*TRClient, error) {
	t, err := newVendorHTTP(reporting.DestinationTR, "EXC_TR_URL",
		endpoint, "application/xml", hc)
	if err != nil {
		return nil, err
	}
	return &TRClient{t: t}, nil
}

// TRClientFromEnv resolves EXC_TR_URL / EXC_TR_TOKEN; absent → fail closed.
func TRClientFromEnv() (*TRClient, error) {
	return NewTRClient(os.Getenv("EXC_TR_URL"), vendorHTTPOpts{
		AuthToken: strings.TrimSpace(os.Getenv("EXC_TR_TOKEN")),
	})
}

func (c *TRClient) Destination() reporting.Destination { return c.t.Destination() }
func (c *TRClient) EndpointLabel() string              { return c.t.EndpointLabel() }

// Submit dispatches the artifact's auth.030 XML body.
func (c *TRClient) Submit(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error) {
	body := []byte(sub.PayloadXML)
	if len(body) == 0 {
		return nil, fmt.Errorf("tr: submission %d has no XML payload",
			sub.ReportSubmissionID)
	}
	status, rb, err := c.t.post(ctx, body)
	if err != nil {
		return nil, err
	}
	return parseVerdict(status, rb)
}

// SDRClient submits CFTC Parts 43/45 reports to a Swap Data Repository.
type SDRClient struct {
	t *vendorHTTP
}

// NewSDRClient constructs the client against an explicit endpoint.
func NewSDRClient(endpoint string, hc vendorHTTPOpts) (*SDRClient, error) {
	t, err := newVendorHTTP(reporting.DestinationSDR, "EXC_SDR_URL",
		endpoint, "application/json", hc)
	if err != nil {
		return nil, err
	}
	return &SDRClient{t: t}, nil
}

// SDRClientFromEnv resolves EXC_SDR_URL / EXC_SDR_TOKEN; absent → fail
// closed (Dodd-Frank reporting to a nil SDR is a hard defect).
func SDRClientFromEnv() (*SDRClient, error) {
	return NewSDRClient(os.Getenv("EXC_SDR_URL"), vendorHTTPOpts{
		AuthToken: strings.TrimSpace(os.Getenv("EXC_SDR_TOKEN")),
	})
}

func (c *SDRClient) Destination() reporting.Destination { return c.t.Destination() }
func (c *SDRClient) EndpointLabel() string              { return c.t.EndpointLabel() }

// Submit dispatches the artifact's JSON payload (CFTC submission schema).
func (c *SDRClient) Submit(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error) {
	if len(sub.Payload) == 0 {
		return nil, fmt.Errorf("sdr: submission %d has empty payload",
			sub.ReportSubmissionID)
	}
	status, rb, err := c.t.post(ctx, sub.Payload)
	if err != nil {
		return nil, err
	}
	return parseVerdict(status, rb)
}

// MockVendorClient is the development/test transport — records
// submissions and returns a scripted verdict (nil → async pending).
// Field Fn is invoked per Submit; nil Fn = ACK everything.
type MockVendorClient struct {
	Dest  reporting.Destination
	Label string
	Fn    func(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error)
	Seen  []*reporting.Submission
}

func (m *MockVendorClient) Destination() reporting.Destination { return m.Dest }
func (m *MockVendorClient) EndpointLabel() string {
	if m.Label != "" {
		return m.Label
	}
	return "MOCK"
}
func (m *MockVendorClient) Submit(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error) {
	m.Seen = append(m.Seen, sub)
	if m.Fn != nil {
		return m.Fn(ctx, sub)
	}
	s := reporting.AckAccept
	return &VendorVerdict{Status: &s, Ref: fmt.Sprintf("MOCK-%d", sub.ReportSubmissionID), HTTPStatus: 200}, nil
}
