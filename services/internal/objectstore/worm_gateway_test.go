package objectstore_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"exchange/internal/objectstore"
)

// TestWORMGatewayEnforcement is a live integration test against a real S3
// Object Lock backend (versitygw posix — the same gateway deploy/scripts/
// ch_restore_drill.sh uses for ClickHouse backups). It proves the WORM
// contract through the committed objectstore.Client seam — the exact code
// path services/internal/archiver uses (COMPLIANCE + retain_until):
//
//	Put COMPLIANCE -> Head echoes lock -> Delete denied -> overwrite denied
//	unlocked delete allowed -> retention expiry releases the lock
//
// Skipped unless the environment points at a provisioned gateway:
//
//	EXC_WORM_ENDPOINT  base endpoint, e.g. http://127.0.0.1:17073 (required)
//	EXC_WORM_BUCKET    bucket pre-created WITH object-lock enabled (required)
//	EXC_WORM_ACCESS / EXC_WORM_SECRET   gateway credentials
//
// deploy/scripts/worm_lock_drill.sh provisions all of this and invokes the
// test; run it directly for ad-hoc verification.
func TestWORMGatewayEnforcement(t *testing.T) {
	ep := os.Getenv("EXC_WORM_ENDPOINT")
	bucket := os.Getenv("EXC_WORM_BUCKET")
	if ep == "" || bucket == "" {
		t.Skip("EXC_WORM_ENDPOINT/EXC_WORM_BUCKET unset — run deploy/scripts/worm_lock_drill.sh")
	}
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		Bucket:          bucket,
		Endpoint:        ep,
		Region:          "us-east-1",
		AccessKeyID:     os.Getenv("EXC_WORM_ACCESS"),
		SecretAccessKey: os.Getenv("EXC_WORM_SECRET"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	lockedKey := "worm-it/locked.parquet"
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key:                   lockedKey,
		Body:                  strings.NewReader("parquet-bytes"),
		Size:                  13,
		ObjectLockMode:        "COMPLIANCE",
		ObjectLockRetainUntil: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("put locked: %v", err)
	}
	head, err := c.Head(ctx, lockedKey)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.ObjectLockMode != "COMPLIANCE" || head.ObjectLockRetainUntil.IsZero() {
		t.Fatalf("lock not surfaced by gateway: %+v", head)
	}

	if err := c.Delete(ctx, lockedKey); err == nil ||
		!strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("gateway did not deny locked delete: %v", err)
	}
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key: lockedKey, Body: strings.NewReader("hijack"), Size: 6,
	}); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("gateway did not deny locked overwrite: %v", err)
	}

	// Control: unlocked object deletes freely.
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key: "worm-it/tmp", Body: strings.NewReader("x"), Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "worm-it/tmp"); err != nil {
		t.Fatalf("unlocked delete: %v", err)
	}

	// Retention expiry releases the lock.
	shortKey := "worm-it/short.parquet"
	if _, err := c.Put(ctx, objectstore.PutInput{
		Key:                   shortKey,
		Body:                  strings.NewReader("s"),
		Size:                  1,
		ObjectLockMode:        "COMPLIANCE",
		ObjectLockRetainUntil: time.Now().Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("put short-retention: %v", err)
	}
	if err := c.Delete(ctx, shortKey); err == nil {
		t.Fatal("expected pre-expiry delete denial")
	}
	time.Sleep(5 * time.Second)
	if err := c.Delete(ctx, shortKey); err != nil {
		t.Fatalf("post-expiry delete: %v", err)
	}
}
