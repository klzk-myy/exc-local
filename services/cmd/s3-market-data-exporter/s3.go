package main

// Minimal S3-compatible PutObject uploader implementing
// persistence.Uploader. The shared objectstore package is owned by another
// workstream (Task boundary); until it lands, this path-style SigV4 PUT
// covers AWS S3 and S3-compatible stores (MinIO/R2/Ceph via EXC_S3_ENDPOINT /
// AWS_ENDPOINT_URL).
//
// Signature: SigV4 with the real payload SHA256 (the exporter already
// computes it and passes it via meta["sha256"]), so no second read of the
// body is needed. All x-amz-* headers sent are signed.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"exchange/internal/persistence"
)

type s3Uploader struct {
	Endpoint      string // e.g. https://s3.eu-west-1.amazonaws.com (path-style)
	Bucket        string
	Region        string
	AccessKey     string
	SecretKey     string
	SessionToken  string
	PublicReadACL bool // data.{domain} is a public bucket: set for canned ACL
	Client        *http.Client
}

func (s *s3Uploader) Put(ctx context.Context, key string, r io.Reader, size int64,
	meta map[string]string) (persistence.ObjectInfo, error) {

	payloadHash := meta["sha256"]
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}

	u := s.Endpoint + "/" + s.Bucket + "/" + escapeKey(key)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateScope := now.Format("20060102")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, r)
	if err != nil {
		return persistence.ObjectInfo{}, err
	}
	req.ContentLength = size
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Content-Type", "application/zip")
	if s.SessionToken != "" {
		req.Header.Set("x-amz-security-token", s.SessionToken)
	}
	if s.PublicReadACL {
		req.Header.Set("x-amz-acl", "public-read")
	}
	for k, v := range meta {
		req.Header.Set("x-amz-meta-"+k, v)
	}
	if err := s.sign(req, payloadHash, amzDate, dateScope); err != nil {
		return persistence.ObjectInfo{}, err
	}

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return persistence.ObjectInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return persistence.ObjectInfo{}, fmt.Errorf("s3 PUT %s: HTTP %d: %s",
			key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return persistence.ObjectInfo{ETag: resp.Header.Get("ETag")}, nil
}

// escapeKey percent-encodes each path segment (S3 keys allow '/').
func escapeKey(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// sign computes the AWS SigV4 Authorization header in place.
func (s *s3Uploader) sign(req *http.Request, payloadHash, amzDate, dateScope string) error {
	host := req.URL.Host
	uri := req.URL.EscapedPath()

	// Sorted signed headers (all x-amz-* headers must be signed).
	type kv struct{ k, v string }
	hdr := []kv{
		{"host", host},
		{"x-amz-content-sha256", payloadHash},
		{"x-amz-date", amzDate},
	}
	if s.PublicReadACL {
		hdr = append(hdr, kv{"x-amz-acl", "public-read"})
	}
	if s.SessionToken != "" {
		hdr = append(hdr, kv{"x-amz-security-token", s.SessionToken})
	}
	for name, vals := range req.Header {
		lname := strings.ToLower(name)
		if strings.HasPrefix(lname, "x-amz-meta-") {
			hdr = append(hdr, kv{lname, strings.TrimSpace(strings.Join(vals, ","))})
		}
	}
	// Insertion sort by header name.
	for i := 1; i < len(hdr); i++ {
		for j := i; j > 0 && hdr[j].k < hdr[j-1].k; j-- {
			hdr[j], hdr[j-1] = hdr[j-1], hdr[j]
		}
	}
	var canonHdrs, signedNames strings.Builder
	for i, h := range hdr {
		if i > 0 {
			signedNames.WriteByte(';')
		}
		canonHdrs.WriteString(h.k)
		canonHdrs.WriteByte(':')
		canonHdrs.WriteString(h.v)
		canonHdrs.WriteByte('\n')
		signedNames.WriteString(h.k)
	}

	canonical := strings.Join([]string{
		req.Method, uri, req.URL.RawQuery, canonHdrs.String(),
		signedNames.String(), payloadHash,
	}, "\n")
	canonHash := sha256.Sum256([]byte(canonical))

	scope := dateScope + "/" + s.Region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" +
		hex.EncodeToString(canonHash[:])

	h := func(key []byte, data string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(data))
		return m.Sum(nil)
	}
	kDate := h([]byte("AWS4"+s.SecretKey), dateScope)
	kRegion := h(kDate, s.Region)
	kService := h(kRegion, "s3")
	kSigning := h(kService, "aws4_request")
	sig := hex.EncodeToString(h(kSigning, stringToSign))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/"+scope+
			", SignedHeaders="+signedNames.String()+
			", Signature="+sig)
	return nil
}
