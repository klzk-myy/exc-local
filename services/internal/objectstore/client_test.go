package objectstore_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exchange/internal/devs3"
	"exchange/internal/objectstore"
)

// newTestClient boots the filesystem devs3 over httptest and returns a
// dev-mode client pinned to it — the same code path as EXC_S3_ENDPOINT.
func newTestClient(t *testing.T, bucket string) (objectstore.Client, func()) {
	t.Helper()
	srv, err := devs3.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		Bucket:   bucket,
		Endpoint: ts.URL,
		Region:   "us-east-1",
	})
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return c, ts.Close
}

func TestPutGetHeadList(t *testing.T) {
	c, done := newTestClient(t, "wal-test")
	defer done()
	ctx := context.Background()

	body := []byte("wal segment bytes \x00\x01\x02")
	sum := md5.Sum(body)
	wantETag := hex.EncodeToString(sum[:])

	obj, err := c.Put(ctx, objectstore.PutInput{
		Key:  "0/2026-01-05/00000000000000000042.wal",
		Body: bytes.NewReader(body),
		Size: int64(len(body)),
		Metadata: map[string]string{
			"shard": "0", "sha256": "abc",
		},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if obj.ETag != wantETag {
		t.Fatalf("etag %q != md5 %q", obj.ETag, wantETag)
	}

	got, meta, err := c.Get(ctx, obj.Key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("get bytes mismatch")
	}
	if meta.ETag != wantETag || meta.Size != int64(len(body)) {
		t.Fatalf("meta %+v", meta)
	}
	if meta.Metadata["sha256"] != "abc" {
		t.Fatalf("metadata not round-tripped: %+v", meta.Metadata)
	}

	head, err := c.Head(ctx, obj.Key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.ETag != wantETag || head.Size != int64(len(body)) {
		t.Fatalf("head %+v", head)
	}

	// Second key under same prefix, plus another branch.
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key: "0/2026-01-05/00000000000000000100.wal", Body: bytes.NewReader([]byte("x")), Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key: "0/index.json", Body: bytes.NewReader([]byte("{}")), Size: 2,
	}); err != nil {
		t.Fatal(err)
	}

	lst, err := c.List(ctx, objectstore.ListInput{Prefix: "0/2026-01-05/"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lst.Objects) != 2 || lst.IsTruncated {
		t.Fatalf("list: %+v", lst)
	}
	if lst.Objects[0].Key >= lst.Objects[1].Key {
		t.Fatal("list not lexicographically ordered")
	}
}

func TestListPagination(t *testing.T) {
	c, done := newTestClient(t, "wal-test")
	defer done()
	ctx := context.Background()
	for _, k := range []string{"a/1", "a/2", "a/3", "a/4", "a/5"} {
		if _, err := c.Put(ctx, objectstore.PutInput{
			Key: k, Body: strings.NewReader(k), Size: int64(len(k)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	var token string
	for {
		page, err := c.List(ctx, objectstore.ListInput{
			Prefix: "a/", MaxKeys: 2, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Objects {
			seen = append(seen, o.Key)
		}
		if !page.IsTruncated {
			break
		}
		token = page.NextContinuationToken
	}
	if len(seen) != 5 {
		t.Fatalf("paginated list got %v", seen)
	}

	// ListAll drains the same way.
	all, err := objectstore.ListAll(ctx, c, "a/")
	if err != nil || len(all) != 5 {
		t.Fatalf("ListAll: %v %d", err, len(all))
	}
}

func TestNotFound(t *testing.T) {
	c, done := newTestClient(t, "wal-test")
	defer done()
	ctx := context.Background()
	_, _, err := c.Get(ctx, "nope")
	if !objectstore.IsNotFound(err) {
		t.Fatalf("get: want NotFoundError, got %v", err)
	}
	if _, err := c.Head(ctx, "nope"); !objectstore.IsNotFound(err) {
		t.Fatalf("head: want NotFoundError, got %v", err)
	}
}

func TestObjectLockDeleteDenied(t *testing.T) {
	c, done := newTestClient(t, "worm-test")
	defer done()
	ctx := context.Background()
	_, err := c.Put(ctx, objectstore.PutInput{
		Key:                   "orders/p1/p1.csv.zst",
		Body:                  strings.NewReader("data"),
		Size:                  4,
		ObjectLockMode:        "COMPLIANCE",
		ObjectLockRetainUntil: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := c.Head(ctx, "orders/p1/p1.csv.zst")
	if err != nil {
		t.Fatal(err)
	}
	if head.ObjectLockMode != "COMPLIANCE" {
		t.Fatalf("lock mode not surfaced: %+v", head)
	}
	if err := c.Delete(ctx, "orders/p1/p1.csv.zst"); err == nil {
		t.Fatal("expected WORM delete denial")
	}
	// Unlocked object deletes fine.
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key: "tmp", Body: strings.NewReader("x"), Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "tmp"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
