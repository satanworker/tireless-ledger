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

func TestRRFMerge(t *testing.T) {
	a := []queryResult{{ID: "x", Score: 1, ForwardContent: "ann"}, {ID: "y", Score: 2, ForwardContent: "ann-y"}}
	b := []queryResult{{ID: "y", Score: 9, ForwardContent: "bm25-y"}, {ID: "z", Score: 8, ForwardContent: "bm25-z"}}
	out := rrfMerge(a, b, 3)
	if len(out) != 3 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].ID != "y" {
		t.Fatalf("want y first, got %q score=%v", out[0].ID, out[0].Score)
	}
}

func TestQueryFilter(t *testing.T) {
	if queryFilter(queryRequest{}) != nil {
		t.Fatal("empty filter must omit")
	}
	f := queryFilter(queryRequest{SessionID: "abc"})
	if f == nil || f.Eq == nil || f.Eq.Field != "session_id" || f.Eq.Value != "abc" {
		t.Fatalf("filter=%+v", f)
	}
	f = queryFilter(queryRequest{Scope: "session_memory", SessionID: "abc"})
	if f == nil || len(f.And) != 2 {
		t.Fatalf("and=%+v", f)
	}
}
