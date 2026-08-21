package main

import "testing"

func TestMemBM25AndSession(t *testing.T) {
	m := newMemStore(2)
	m.upsert([]MemoryItem{
		{ID: "a", Vector: []float32{0, 0}, ForwardContent: "opendata vector cannot index code well", Metadata: Metadata{SessionID: "s1", Timestamp: 2, ProjectName: "x", Scope: ScopeSession, FileHash: "1"}},
		{ID: "b", Vector: []float32{1, 0}, ForwardContent: "walletActivation in swap-ui", Metadata: Metadata{SessionID: "s1", Timestamp: 1, ProjectName: "x", Scope: ScopeSession, FileHash: "2"}},
		{ID: "c", Vector: []float32{0, 1}, ForwardContent: "unrelated grocery list", Metadata: Metadata{SessionID: "s2", Timestamp: 3, ProjectName: "x", Scope: ScopeSession, FileHash: "3"}},
	})
	hits := m.search(vectorSearchRequest{BM25: &bm25Query{Query: "opendata code"}, K: 5})
	if len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("bm25 hits=%+v", hits)
	}
	walk := m.bySession("s1", "", "", 0, "", 10)
	if len(walk) != 2 || walk[0].ID != "b" || walk[1].ID != "a" {
		t.Fatalf("walk=%+v", walk)
	}
	page := m.bySession("s1", "", "", 1, "b", 10)
	if len(page) != 1 || page[0].ID != "a" {
		t.Fatalf("cursor=%+v", page)
	}
}

func TestIsMemoryURL(t *testing.T) {
	if !isMemoryURL("memory://") || isMemoryURL("s3://bucket/p") {
		t.Fatal("memory url detect")
	}
}
