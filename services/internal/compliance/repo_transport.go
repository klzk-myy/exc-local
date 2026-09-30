// Vendor transport machinery for the Phase-21 reporting adapters
// (Task 21.3.16, spec §14.5): shared HTTP POST plumbing, endpoint
// configuration from env, synchronous ACK + asynchronous NACK parsing,
// and fail-closed construction — an unconfigured endpoint must never
// silently produce a submission.
package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"exchange/internal/compliance/reporting"
	excerrors "exchange/pkg/errors"
)

// VendorVerdict is the parsed repository response to a dispatch.
type VendorVerdict struct {
	// Accepted/Rejected — nil means the vendor acknowledged receipt but
	// will deliver the verdict asynchronously (HTTP 202-style).
	Status     *reporting.AckStatus
	Code       string // vendor ack/error code
	Text       string // vendor ack/error text
	Ref        string // repository receipt / report id
	HTTPStatus int
}

// VendorClient is the submission seam for one repository class
// (APA/ARM/TR/SDR). Implementations serialize + POST one artifact and
// parse the synchronous verdict.
type VendorClient interface {
	Destination() reporting.Destination
	// EndpointLabel is the env-configured endpoint name recorded in the
	// transport ledger (never credentials).
	EndpointLabel() string
	// Submit dispatches one artifact. A transport-level failure returns
	// error (the dispatcher retries under bounded backoff); a parsed
	// vendor verdict returns a non-nil VendorVerdict.
	Submit(ctx context.Context, sub *reporting.Submission) (*VendorVerdict, error)
}

// ---------------------------------------------------------------------------
// Shared HTTP plumbing
// ---------------------------------------------------------------------------

// vendorHTTP carries the endpoint config + HTTP client for one vendor.
type vendorHTTP struct {
	destination reporting.Destination
	label       string // env var name, e.g. "EXC_APA_URL"
	endpoint    string // configured URL
	authToken   string // optional bearer token env (EXC_*_TOKEN)
	http        *http.Client
	contentType string // request Content-Type
}

// vendorHTTPOpts configures optional transport knobs.
type vendorHTTPOpts struct {
	Timeout   time.Duration
	AuthToken string
	HTTP      *http.Client // overrides the default client entirely
}

func newVendorHTTP(dest reporting.Destination, label, endpoint, contentType string,
	opts vendorHTTPOpts) (*vendorHTTP, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, excerrors.New("SERVICE_DEGRADED",
			fmt.Sprintf("regulatory vendor endpoint %s unconfigured — fail closed", label))
	}
	to := opts.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: to}
	}
	return &vendorHTTP{
		destination: dest, label: label,
		endpoint:  strings.TrimSpace(endpoint),
		authToken: opts.AuthToken,
		http:      hc, contentType: contentType,
	}, nil
}

func (v *vendorHTTP) Destination() reporting.Destination { return v.destination }
func (v *vendorHTTP) EndpointLabel() string              { return v.label }

// post sends body to the configured endpoint. Returns (status, body).
// Network/5xx failures are transport errors the dispatcher retries;
// the caller parses the body for the verdict.
func (v *vendorHTTP) post(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint,
		bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("%s transport: build request: %w", v.label, err)
	}
	req.Header.Set("Content-Type", v.contentType)
	req.Header.Set("Accept", "application/json, application/xml")
	if v.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+v.authToken)
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s transport: %w", v.label, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%s transport: read body: %w", v.label, err)
	}
	return resp.StatusCode, rb, nil
}

// ---------------------------------------------------------------------------
// Verdict parsing — JSON primary, XML fallback. Vendor response shapes:
//   {"status":"ACK"|"NACK"|"PENDING", "code":"...", "message":"...",
//    "ref":"..."}
//   <Ack><Status>ACK|NACK</Status><Code/><Text/><Ref/></Ack>
// HTTP 202 / empty body → async pending (no verdict).
// ---------------------------------------------------------------------------

func parseVerdict(httpStatus int, body []byte) (*VendorVerdict, error) {
	v := &VendorVerdict{HTTPStatus: httpStatus}
	if httpStatus == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
		return v, nil // async — verdict pending
	}

	var j struct {
		Status  string `json:"status"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Ref     string `json:"ref"`
	}
	var x struct {
		XMLName xml.Name `xml:"Ack"`
		Status  string   `xml:"Status"`
		Code    string   `xml:"Code"`
		Text    string   `xml:"Text"`
		Ref     string   `xml:"Ref"`
	}
	var status, code, text, ref string
	if err := json.Unmarshal(body, &j); err == nil && j.Status != "" {
		status, code, text, ref = j.Status, j.Code, j.Message, j.Ref
	} else if err := xml.Unmarshal(body, &x); err == nil && x.Status != "" {
		status, code, text, ref = x.Status, x.Code, x.Text, x.Ref
	}

	switch strings.ToUpper(status) {
	case "ACK", "ACCEPTED":
		s := reporting.AckAccept
		v.Status, v.Code, v.Text, v.Ref = &s, code, text, ref
	case "NACK", "REJECTED":
		s := reporting.AckReject
		v.Status, v.Code, v.Text, v.Ref = &s, code, text, ref
	case "PENDING", "PROCESSING", "":
		// receipt without verdict — async pending
	default:
		s := reporting.AckReject
		v.Status, v.Code, v.Text, v.Ref = &s, code,
			fmt.Sprintf("unparseable vendor status %q", status), ref
	}
	// HTTP 4xx/5xx without a parsed NACK is a transport failure —
	// retried (5xx) or parked as NACK (4xx, a vendor-side rejection).
	if v.Status == nil && httpStatus >= 400 && httpStatus < 500 {
		s := reporting.AckReject
		v.Status = &s
		v.Code = fmt.Sprintf("HTTP_%d", httpStatus)
		if text != "" {
			v.Text = text
		} else {
			v.Text = strings.TrimSpace(string(body[:min(len(body), 512)]))
		}
	}
	if v.Status == nil && httpStatus >= 500 {
		return nil, fmt.Errorf("vendor %d: %s", httpStatus,
			strings.TrimSpace(string(body[:min(len(body), 256)])))
	}
	return v, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
