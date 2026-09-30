// ARM client — MiFID II Article 26 RTS 22 transaction reports submitted
// to an Approved Reporting Mechanism (Task 21.3.16, spec §14.5).
// auth.016.001.05 XML batches POST to EXC_ARM_URL (token via
// EXC_ARM_TOKEN). Unconfigured → construction fails closed.
package compliance

import (
	"context"
	"fmt"
	"os"
	"strings"

	"exchange/internal/compliance/reporting"
)

// ARMClient submits RTS 22 auth.016 XML transactions.
type ARMClient struct {
	t *vendorHTTP
}

// NewARMClient constructs the client against an explicit endpoint.
func NewARMClient(endpoint string, hc vendorHTTPOpts) (*ARMClient, error) {
	t, err := newVendorHTTP(reporting.DestinationARM, "EXC_ARM_URL",
		endpoint, "application/xml", hc)
	if err != nil {
		return nil, err
	}
	return &ARMClient{t: t}, nil
}

// ARMClientFromEnv resolves EXC_ARM_URL / EXC_ARM_TOKEN; absent endpoint
// fails closed.
func ARMClientFromEnv() (*ARMClient, error) {
	return NewARMClient(os.Getenv("EXC_ARM_URL"), vendorHTTPOpts{
		AuthToken: strings.TrimSpace(os.Getenv("EXC_ARM_TOKEN")),
	})
}

func (c *ARMClient) Destination() reporting.Destination { return c.t.Destination() }
func (c *ARMClient) EndpointLabel() string              { return c.t.EndpointLabel() }

// Submit dispatches the artifact's auth.016 XML body (payload_xml);
// falls back to the JSON record when no XML was serialized (defensive —
// the artifact builder always produces XML for ARM rows).
func (c *ARMClient) Submit(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error) {
	body := []byte(sub.PayloadXML)
	if len(body) == 0 {
		if len(sub.Payload) == 0 {
			return nil, fmt.Errorf("arm: submission %d has no payload",
				sub.ReportSubmissionID)
		}
		body = sub.Payload
	}
	status, rb, err := c.t.post(ctx, body)
	if err != nil {
		return nil, err
	}
	return parseVerdict(status, rb)
}
