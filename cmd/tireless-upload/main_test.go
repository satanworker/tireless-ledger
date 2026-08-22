package main

import "testing"

func TestParseS3URL(t *testing.T) {
	bucket, prefix, err := parseS3URL("s3://bucket/raw/v1/")
	if err != nil || bucket != "bucket" || prefix != "raw/v1" {
		t.Fatalf("bucket=%q prefix=%q err=%v", bucket, prefix, err)
	}
	if _, _, err := parseS3URL("https://bucket/raw"); err == nil {
		t.Fatal("accepted non-S3 URL")
	}
}
