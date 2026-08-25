package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type blockingLanceStore struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingLanceStore) Close() error                               { return nil }
func (b *blockingLanceStore) Upsert(context.Context, []MemoryItem) error { return nil }
func (b *blockingLanceStore) Search(context.Context, vectorSearchRequest) ([]queryResult, error) {
	b.calls.Add(1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return nil, nil
}
func (b *blockingLanceStore) Optimize(context.Context) error          { return nil }
func (b *blockingLanceStore) CreateVectorIndex(context.Context) error { return nil }
func (b *blockingLanceStore) DropVectorIndex(context.Context) error   { return nil }
func (b *blockingLanceStore) AuditDuplicates(context.Context) (duplicateAudit, error) {
	return duplicateAudit{}, nil
}
func (b *blockingLanceStore) DeleteHost(context.Context, string) (int, error) { return 0, nil }
func (b *blockingLanceStore) MigrateSplit(context.Context, int, string) error { return nil }
func (b *blockingLanceStore) FragmentCounts(context.Context) (map[string]int, error) {
	return nil, nil
}

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

func TestDedupStateRemoveHost(t *testing.T) {
	state := &dedupState{Files: map[string]string{
		"mbp14/codex/session/turn":  "a",
		"mbp140/codex/session/turn": "b",
		"vps/codex/session/turn":    "c",
		"mbp14/pi/session/turn":     "d",
	}}
	if removed := state.RemoveHost("mbp14"); removed != 2 {
		t.Fatalf("removed=%d", removed)
	}
	if len(state.Files) != 2 || state.Files["mbp140/codex/session/turn"] != "b" {
		t.Fatalf("files=%v", state.Files)
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

func TestTimedOutNativeSearchKeepsConcurrencySlot(t *testing.T) {
	store := &blockingLanceStore{started: make(chan struct{}, 2), release: make(chan struct{})}
	s := &server{
		cfg:        runtimeConfig{QueryTimeout: 20 * time.Millisecond},
		querySlots: make(chan struct{}, 1),
		lance:      store,
	}

	if _, err := s.runSearch(context.Background(), vectorSearchRequest{Table: "chunks", K: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first search error=%v", err)
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("native calls after first timeout=%d", got)
	}
	if _, err := s.runSearch(context.Background(), vectorSearchRequest{Table: "chunks", K: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued search error=%v", err)
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("queued timeout launched another native call: calls=%d", got)
	}

	close(store.release)
	deadline := time.Now().Add(time.Second)
	for len(s.querySlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.querySlots) != 0 {
		t.Fatal("native slot was not released after the underlying call returned")
	}
	if _, err := s.runSearch(context.Background(), vectorSearchRequest{Table: "chunks", K: 1}); err != nil {
		t.Fatalf("search after release=%v", err)
	}
}
