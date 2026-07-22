package main

import (
	"testing"
	"time"
)

func TestParseS3URL(t *testing.T) {
	bucket, prefix, err := parseS3URL("s3://prod-bucket/vector-index")
	if err != nil {
		t.Fatalf("parseS3URL returned error: %v", err)
	}
	if bucket != "prod-bucket" || prefix != "vector-index" {
		t.Fatalf("bucket=%q prefix=%q", bucket, prefix)
	}
}

func TestValidateMemoryItem(t *testing.T) {
	item := MemoryItem{
		ID:             "repo_file_0",
		Vector:         []float32{0.1, 0.2},
		ForwardContent: "package main",
		Metadata: Metadata{
			Scope:       ScopeCode,
			ProjectName: "backend-api",
			FilePath:    "main.go",
			FileHash:    "abc123",
			Timestamp:   time.Now().Unix(),
		},
	}
	if err := validateMemoryItem(item, 2); err != nil {
		t.Fatalf("valid item failed: %v", err)
	}
	item.Metadata.Scope = "bad"
	if err := validateMemoryItem(item, 2); err == nil {
		t.Fatalf("expected invalid scope error")
	}
}

func TestDedupState(t *testing.T) {
	state := &dedupState{Files: map[string]string{}}
	if state.Seen("a.go", "hash1") {
		t.Fatalf("empty state should not match")
	}
	state.Mark("a.go", "hash1")
	if !state.Seen("a.go", "hash1") {
		t.Fatalf("expected hash match")
	}
	if state.Seen("a.go", "hash2") {
		t.Fatalf("new hash should not be skipped")
	}
}
