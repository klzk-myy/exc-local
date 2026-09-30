// APA client — MiFID II Articles 20/21 post-trade transparency
// submissions (Task 21.3.16, spec §14.5; §24 #178). RTS 1 JSON payloads
// POST to the Approved Publication Arrangement endpoint configured via
// EXC_APA_URL (token via EXC_APA_TOKEN). Unconfigured → construction
// fails closed (SERVICE_DEGRADED) — a silently-skipped transparency
// report is a regulatory breach.
package compliance

import (
	"context"
	"fmt"
	"os"
	"strings"

	"exchange/internal/compliance/reporting"
)

// APAClient submits RTS 1 post-trade transparency reports.
type APAClient struct {
	t *vendorHTTP
}

// NewAPAClient constructs the client against an explicit endpoint (the
// test seam); use APAClientFromEnv in composition.
func NewAPAClient(endpoint string, hc vendorHTTPOpts) (*APAClient, error) {
	t, err := newVendorHTTP(reporting.DestinationAPA, "EXC_APA_URL",
		endpoint, "application/json", hc)
	if err != nil {
		return nil, err
	}
	return &APAClient{t: t}, nil
}

// APAClientFromEnv resolves EXC_APA_URL / EXC_APA_TOKEN; absent endpoint
// fails closed.
func APAClientFromEnv() (*APAClient, error) {
	return NewAPAClient(os.Getenv("EXC_APA_URL"), vendorHTTPOpts{
		AuthToken: strings.TrimSpace(os.Getenv("EXC_APA_TOKEN")),
	})
}

func (c *APAClient) Destination() reporting.Destination { return c.t.Destination() }
func (c *APAClient) EndpointLabel() string              { return c.t.EndpointLabel() }

// Submit dispatches the artifact's RTS 1 JSON payload (payload_xml is
// an EMIR/ARM concern — APA consumes JSON).
func (c *APAClient) Submit(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error) {
	if len(sub.Payload) == 0 {
		return nil, fmt.Errorf("apa: submission %d has empty payload",
			sub.ReportSubmissionID)
	}
	status, body, err := c.t.post(ctx, sub.Payload)
	if err != nil {
		return nil, err
	}
	return parseVerdict(status, body)
}
