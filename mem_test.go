package main

import "testing"

func TestMemBM25AndContainerWalk(t *testing.T) {
	m := newMemStore(2)
	items := []MemoryItem{
		{ID: "a", Vector: []float32{0, 0}, Text: "opendata vector cannot index code well", Metadata: Metadata{ContainerID: "s1", Kind: "message", Timestamp: 2}},
		{ID: "b", Vector: []float32{1, 0}, Text: "walletActivation in swap-ui", Metadata: Metadata{ContainerID: "s1", Kind: "message", Timestamp: 1}},
		{ID: "c", Vector: []float32{0, 1}, Text: "unrelated grocery list", Metadata: Metadata{ContainerID: "s2", Kind: "message", Timestamp: 3}},
		{ID: "t", Text: "Session title", Metadata: Metadata{ContainerID: "s1", Kind: "title", Timestamp: 1}},
	}
	m.upsert(items, items[:3])
	hits := m.search(vectorSearchRequest{BM25: &bm25Query{Query: "opendata code"}, K: 5})
	if len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("bm25 hits=%+v", hits)
	}
	messages := map[string]bool{"message": true}
	walk := m.byContainer("s1", "", "", messages, 0, "", 10)
	if len(walk) != 2 || walk[0].ID != "b" || walk[1].ID != "a" {
		t.Fatalf("walk=%+v", walk)
	}
	page := m.byContainer("s1", "", "", messages, 1, "b", 10)
	if len(page) != 1 || page[0].ID != "a" {
		t.Fatalf("cursor=%+v", page)
	}
	if titles := m.byContainer("s1", "", "", map[string]bool{"title": true}, 0, "", 10); len(titles) != 1 || titles[0].ID != "t" {
		t.Fatalf("titles=%+v", titles)
	}
}

func TestIsMemoryURL(t *testing.T) {
	if !isMemoryURL("memory://") || isMemoryURL("s3://bucket/p") {
		t.Fatal("memory url detect")
	}
}
