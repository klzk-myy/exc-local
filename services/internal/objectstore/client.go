// Package objectstore is the single S3 seam for the exchange Go services
// (Task 4.3.2 WAL archive, 4.3.3 replay-from-archive, 4.3.7 partition
// archival WORM pipeline).
//
// Every S3-touching feature takes the narrow Client interface so unit tests
// can substitute a fake and integration tests can point at the
// filesystem-backed dev server (services/cmd/devs3, internal/devs3) via a
// base-endpoint override. Two constructors cover the deployment split:
//
//	NewAWS(cfg) — real AWS/R2: default credential chain, virtual-host or
//	              path style per provider, endpoint only if configured.
//	NewDev(cfg) — dev/stub: requires cfg.Endpoint, forces path-style
//	              addressing and static credentials (devs3 ignores auth).
//
// ETag contract: both implementations return the classic S3 ETag —
// hex(md5(body)) — because clients are built with
// RequestChecksumCalculation=WHEN_REQUIRED so no trailing-checksum
// algorithm is negotiated. Callers that upload must verify the returned
// ETag against their own md5 before treating the write as durable
// (zero-loss WAL guard, spec §3.5 / §24).
package objectstore

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Object is the metadata view shared by Head and List.
type Object struct {
	Key          string
	Size         int64
	ETag         string // unquoted hex md5
	LastModified time.Time
	Metadata     map[string]string
	// ObjectLockMode / ObjectLockRetainUntil model WORM state. devs3
	// persists them as headers; on real AWS they map to the native
	// Object Lock fields (bucket must be created with Object Lock enabled).
	ObjectLockMode        string
	ObjectLockRetainUntil time.Time
}

// PutInput describes one object write.
type PutInput struct {
	Key         string
	Body        io.Reader
	Size        int64 // -1 unknown (devs3 buffers; AWS requires >= 0, we buffer when < 0)
	ContentType string
	Metadata    map[string]string
	// ObjectLockMode "COMPLIANCE"|"GOVERNANCE" models S3 Object Lock.
	// Empty = no lock. RetainUntil zero = use bucket default.
	ObjectLockMode        string
	ObjectLockRetainUntil time.Time
}

// ListInput narrows ListObjectsV2 to what the services need.
type ListInput struct {
	Prefix            string
	Delimiter         string
	MaxKeys           int32 // 0 = server default
	ContinuationToken string
}

// ListOutput mirrors ListObjectsV2.
type ListOutput struct {
	Objects               []Object
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
}

// Client is the narrow object-store contract used by recovery and archiver.
type Client interface {
	Put(ctx context.Context, in PutInput) (Object, error)
	Get(ctx context.Context, key string) ([]byte, Object, error)
	Head(ctx context.Context, key string) (Object, error)
	List(ctx context.Context, in ListInput) (ListOutput, error)
	Delete(ctx context.Context, key string) error
	// Bucket returns the bucket this client is bound to (clients are
	// single-bucket by construction; keys never carry a bucket prefix).
	Bucket() string
}

// ListAll drains pagination for callers that want a full prefix listing.
func ListAll(ctx context.Context, c Client, prefix string) ([]Object, error) {
	var out []Object
	in := ListInput{Prefix: prefix}
	for {
		page, err := c.List(ctx, in)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Objects...)
		if !page.IsTruncated {
			return out, nil
		}
		in.ContinuationToken = page.NextContinuationToken
	}
}

// IsNotFound reports whether err is an object-miss (NoSuchKey / 404).
type NotFoundError struct {
	Key string
	Err error
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("objectstore: key %q not found: %v", e.Key, e.Err)
}
func (e *NotFoundError) Unwrap() error { return e.Err }

// IsNotFound unwraps a NotFoundError.
func IsNotFound(err error) bool {
	for err != nil {
		if _, ok := err.(*NotFoundError); ok {
			return true
		}
		err = unwrap(err)
	}
	return false
}

func unwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}
