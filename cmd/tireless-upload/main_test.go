package main

import (
	"context"
	"testing"
)

func TestParseS3URL(t *testing.T) {
	bucket, prefix, err := parseS3URL("s3://bucket/raw/v1/")
	if err != nil || bucket != "bucket" || prefix != "raw/v1" {
		t.Fatalf("bucket=%q prefix=%q err=%v", bucket, prefix, err)
	}
	if _, _, err := parseS3URL("https://bucket/raw"); err == nil {
		t.Fatal("accepted non-S3 URL")
	}
}

// -grokbot defaults to empty: the source must be skipped without touching S3.
func TestSyncTreeSkipsUnsetRoot(t *testing.T) {
	scanned, uploaded, err := syncTree(context.Background(), nil, "bucket", "raw", "grok-box", "grokbot", "")
	if scanned != 0 || uploaded != 0 || err != nil {
		t.Fatalf("scanned=%d uploaded=%d err=%v", scanned, uploaded, err)
	}
}
