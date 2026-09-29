package objectstore_test

// SSE-KMS contract test (Phase-12 Task 12.3.4, spec §24 #102): the
// objectstore PutInput SSE fields must reach the backend as
// x-amz-server-side-encryption[-aws-kms-key-id] and echo back on
// Head/Get. devs3 persists the headers verbatim — on real AWS the same
// fields map to native SSE-KMS PutObject params.

import (
	"bytes"
	"context"
	"testing"

	"exchange/internal/objectstore"
)

func TestSSEKMSRoundTrip(t *testing.T) {
	c, done := newTestClient(t, "kyc-docs")
	defer done()
	ctx := context.Background()

	obj, err := c.Put(ctx, objectstore.PutInput{
		Key:                  "kyc/42/7/passport-abc123.jpg",
		Body:                 bytes.NewReader([]byte{0xFF, 0xD8, 0xFF, 0xE0}),
		Size:                 4,
		ContentType:          "image/jpeg",
		ServerSideEncryption: "aws:kms",
		SSEKMSKeyID:          "arn:aws:kms:us-east-1:123:key/abc",
		Metadata:             map[string]string{"document-type": "PASSPORT"},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	head, err := c.Head(ctx, obj.Key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.ServerSideEncryption != "aws:kms" {
		t.Fatalf("head SSE=%q, want aws:kms", head.ServerSideEncryption)
	}
	if head.SSEKMSKeyID != "arn:aws:kms:us-east-1:123:key/abc" {
		t.Fatalf("head KMS key=%q", head.SSEKMSKeyID)
	}

	_, meta, err := c.Get(ctx, obj.Key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if meta.ServerSideEncryption != "aws:kms" {
		t.Fatalf("get SSE=%q, want aws:kms", meta.ServerSideEncryption)
	}
}

func TestSSEAbsentByDefault(t *testing.T) {
	c, done := newTestClient(t, "kyc-docs")
	defer done()
	obj, err := c.Put(context.Background(), objectstore.PutInput{
		Key:  "plain.bin",
		Body: bytes.NewReader([]byte("x")),
		Size: 1,
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	head, err := c.Head(context.Background(), obj.Key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.ServerSideEncryption != "" || head.SSEKMSKeyID != "" {
		t.Fatalf("unexpected SSE on plain put: %+v", head)
	}
}
