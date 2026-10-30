package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeRawStore struct {
	objects []rawObject
	bodies  map[string][]byte
	gets    int
}

func (f *fakeRawStore) List(context.Context) ([]rawObject, error) { return f.objects, nil }
func (f *fakeRawStore) Get(_ context.Context, key string, _ int64) ([]byte, error) {
	f.gets++
	return f.bodies[key], nil
}

func memRows(s *server) int { return len(s.mem.messages) + len(s.mem.chunks) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testTokenResponse(req *http.Request) (*http.Response, error) {
	var in struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		return nil, err
	}
	tokens := make([]int, len(strings.Fields(in.Content)))
	b, _ := json.Marshal(map[string]interface{}{"tokens": tokens})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
}

func testEmbeddingResponse(req *http.Request, dims int) (*http.Response, error) {
	var in struct {
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		return nil, err
	}
	data := make([]map[string]interface{}, len(in.Input))
	for i := range data {
		vector := make([]float32, dims)
		data[i] = map[string]interface{}{"index": i, "embedding": vector}
	}
	b, _ := json.Marshal(map[string]interface{}{"data": data})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
}

func TestRawIngestScanIsCrashSafeAndIdempotent(t *testing.T) {
	body := []byte("{\"type\":\"session\",\"id\":\"s1\",\"cwd\":\"/repo/project\"}\n" +
		"{\"type\":\"message\",\"id\":\"u1\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"Remember the exact original text.\"}]}}\n")
	obj := rawObject{Key: "mac/pi/s1.jsonl", ETag: "etag-1", Size: int64(len(body))}
	store := &fakeRawStore{objects: []rawObject{obj}, bodies: map[string][]byte{obj.Key: body}}
	s := testServer()
	s.cfg.RawMaxObjectBytes = 1 << 20
	s.cfg.EmbedBatchSize = 32
	s.cfg.EmbedURL = "http://embed"
	registry, err := loadRawRegistry(t.TempDir() + "/raw.json")
	if err != nil {
		t.Fatal(err)
	}
	embeds := 0
	r := &rawIngestor{server: s, store: store, registry: registry, http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/tokenize" {
			return testTokenResponse(req)
		}
		embeds++
		return testEmbeddingResponse(req, 2)
	})}}
	s.raw = r
	r.scan(context.Background())
	if !registry.Done(obj) {
		t.Fatalf("registry=%+v", registry.Objects[obj.Key])
	}
	if store.gets != 1 {
		t.Fatalf("gets=%d", store.gets)
	}
	if memRows(s) != 2 {
		t.Fatalf("indexed=%d", memRows(s))
	}
	r.scan(context.Background())
	if store.gets != 1 {
		t.Fatalf("completed object downloaded again: gets=%d", store.gets)
	}
	if memRows(s) != 2 {
		t.Fatalf("duplicate rows=%d", memRows(s))
	}

	// A parser version change invalidates only derived state and safely replays.
	entry := registry.Objects[obj.Key]
	entry.ParserVersion = "old-parser"
	registry.Objects[obj.Key] = entry
	r.scan(context.Background())
	if store.gets != 2 {
		t.Fatalf("parser upgrade was not replayed: gets=%d", store.gets)
	}
	if memRows(s) != 2 {
		t.Fatalf("replay duplicated rows=%d", memRows(s))
	}
	if embeds != 1 {
		t.Fatalf("an unchanged unit was re-embedded after a parser bump: embed requests=%d", embeds)
	}
}

func TestRawIngestScanIndexesOmpObject(t *testing.T) {
	body := []byte("{\"type\":\"session\",\"id\":\"omp-s1\",\"cwd\":\"/repo/omp-project\"}\n" +
		"{\"type\":\"message\",\"id\":\"u1\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"Index this OMP user turn.\"}]}}\n")
	obj := rawObject{Key: "mac/omp/s1.jsonl", ETag: "etag-omp", Size: int64(len(body))}
	store := &fakeRawStore{objects: []rawObject{obj}, bodies: map[string][]byte{obj.Key: body}}
	s := testServer()
	s.cfg.RawMaxObjectBytes = 1 << 20
	s.cfg.EmbedBatchSize = 32
	s.cfg.EmbedURL = "http://embed"
	registry, err := loadRawRegistry(t.TempDir() + "/raw.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &rawIngestor{server: s, store: store, registry: registry, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)), Header: make(http.Header)}, nil
	})}}
	s.raw = r
	r.scan(context.Background())
	if !registry.Done(obj) {
		t.Fatalf("registry=%+v", registry.Objects[obj.Key])
	}
	var found bool
	for _, item := range s.mem.messages {
		if item.Metadata.Source == "omp" && item.Metadata.ContainerID == "omp-s1" && item.Metadata.Project == "omp-project" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing OMP item in indexed metadata: %+v", s.mem.messages)
	}
}

func TestRawContainerWalkUsesUntouchedObject(t *testing.T) {
	body := []byte("{\"type\":\"session\",\"id\":\"omp-walk\",\"cwd\":\"/repo/raw-walk\"}\n" +
		"{\"type\":\"message\",\"id\":\"u1\",\"timestamp\":\"2026-09-01T18:20:00Z\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"First raw message stays exact.\"}]}}\n" +
		"{\"type\":\"message\",\"id\":\"a1\",\"timestamp\":\"2026-09-01T18:20:01Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"Second raw message stays exact.\"}]}}\n")
	obj := rawObject{Key: "mac/omp/raw-walk.jsonl", ETag: "etag-walk", Size: int64(len(body))}
	store := &fakeRawStore{objects: []rawObject{obj}, bodies: map[string][]byte{obj.Key: body}}
	s := testServer()
	s.cfg.RawMaxObjectBytes = 1 << 20
	s.cfg.EmbedBatchSize = 32
	s.cfg.EmbedURL = "http://embed"
	registry, err := loadRawRegistry(t.TempDir() + "/raw.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &rawIngestor{server: s, store: store, registry: registry, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[0.1,0.2]},{"index":1,"embedding":[0.2,0.1]}]}`)), Header: make(http.Header)}, nil
	})}}
	r.scan(context.Background())
	if got := registry.Objects[obj.Key].ContainerIDs; len(got) != 1 || got[0] != "omp-walk" {
		t.Fatalf("container ids=%v", got)
	}
	s.mem = newMemStore(2)
	results, ok, err := r.containerWalk(context.Background(), "omp-walk", "mac", "omp", 0, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(results) != 2 {
		t.Fatalf("ok=%v results=%+v", ok, results)
	}
	if results[0].Text != "First raw message stays exact." || results[1].Text != "Second raw message stays exact." {
		t.Fatalf("results=%+v", results)
	}
	page, ok, err := r.containerWalk(context.Background(), "omp-walk", "mac", "omp", metaTS(results[0]), results[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(page) != 1 || page[0].ID != results[1].ID {
		t.Fatalf("page=%+v ok=%v", page, ok)
	}
}

func TestRawContainerStateWalkUsesUntouchedObject(t *testing.T) {
	body := []byte("{\"type\":\"session\",\"id\":\"omp-state\",\"cwd\":\"/repo/raw-state\",\"title\":\"Stateful OMP session\"}\n" +
		"{\"type\":\"message\",\"id\":\"todo-1\",\"timestamp\":\"2026-09-01T18:20:00Z\",\"message\":{\"role\":\"toolResult\",\"toolName\":\"todo\",\"details\":{\"phases\":[{\"phase\":\"Remote todo retrieval\",\"items\":[{\"content\":\"Expose todo session entries\",\"status\":\"in_progress\"}]}]}}}\n" +
		"{\"type\":\"message\",\"id\":\"task-1\",\"timestamp\":\"2026-09-01T18:20:01Z\",\"message\":{\"role\":\"toolResult\",\"toolName\":\"task\",\"content\":[{\"type\":\"text\",\"text\":\"Subagent found task output worth searching.\"}]}}\n")
	obj := rawObject{Key: "mac/omp/2026_omp-state.jsonl", ETag: "etag-state", Size: int64(len(body))}
	store := &fakeRawStore{objects: []rawObject{obj}, bodies: map[string][]byte{obj.Key: body}}
	s := testServer()
	s.cfg.RawMaxObjectBytes = 1 << 20
	registry, err := loadRawRegistry(t.TempDir() + "/raw.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &rawIngestor{server: s, store: store, registry: registry}
	results, ok, err := r.containerStateWalk(context.Background(), "omp-state", "mac", "omp", 0, "", 10, map[string]bool{"todo": true})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(results) != 1 {
		t.Fatalf("ok=%v results=%+v", ok, results)
	}
	if got := results[0].Metadata["kind"]; got != "todo" {
		t.Fatalf("kind=%v", got)
	}
	if !strings.Contains(results[0].Text, "Expose todo session entries") {
		t.Fatalf("todo=%q", results[0].Text)
	}
}

func TestTokenChunksCoverEntireMessageWithOverlap(t *testing.T) {
	words := make([]string, 900)
	for i := range words {
		words[i] = fmt.Sprintf("word%04d", i)
	}
	words[len(words)-1] = "TAIL_SENTINEL"
	text := strings.Join(words, " ")
	r := &rawIngestor{server: testServer(), http: &http.Client{Transport: roundTripFunc(testTokenResponse)}}
	chunks, err := r.splitTokenChunks(context.Background(), text, embedChunkTokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("chunks=%d", len(chunks))
	}
	for i, chunk := range chunks {
		if got := len(strings.Fields(chunk)); got > embedChunkTokens {
			t.Fatalf("chunk[%d] tokens=%d", i, got)
		}
	}
	if !strings.Contains(chunks[len(chunks)-1], "TAIL_SENTINEL") {
		t.Fatalf("tail missing from final chunk")
	}
	if !strings.Contains(chunks[0], strings.Fields(chunks[1])[0]) {
		t.Fatalf("expected overlap between first two chunks")
	}
}

func TestLongMessageTailIsSearchableAndContainerKeepsOriginal(t *testing.T) {
	words := make([]string, 900)
	for i := range words {
		words[i] = fmt.Sprintf("detail%04d", i)
	}
	words[len(words)-1] = "TAIL_SEMANTIC_FACT"
	text := strings.Join(words, " ")
	s := testServer()
	s.dedup.Path = t.TempDir() + "/dedup.json"
	s.cfg.EmbedURL, s.cfg.EmbedBatchSize = "http://embed", 32
	r := &rawIngestor{server: s, http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/tokenize" {
			return testTokenResponse(req)
		}
		return testEmbeddingResponse(req, 2)
	})}}
	message := rawRecord("mbp14", "pi", "session-long", "turn-long", "assistant", text, 123, "project", "message")
	groups := chunkGroups([]MemoryItem{message}, func(MemoryItem, string) bool { return true })
	if _, err := r.indexGroups(context.Background(), groups, func(m MemoryItem) string { return m.ID }); err != nil {
		t.Fatal(err)
	}
	hits := s.mem.search(vectorSearchRequest{
		BM25: &bm25Query{Field: "text", Query: "TAIL_SEMANTIC_FACT"},
		K:    10,
	})
	hits = collapseChunkHits(hits, 5)
	if len(hits) != 1 || hits[0].ID != message.ID {
		t.Fatalf("tail hits=%+v", hits)
	}
	walk := s.mem.byContainer("session-long", "mbp14", "pi", map[string]bool{"message": true}, 0, "", 10)
	if len(walk) != 1 || walk[0].Text != text {
		t.Fatalf("container reconstruction changed: rows=%d", len(walk))
	}
}

func TestRawRegistryRemoveHost(t *testing.T) {
	r := &rawRegistry{Objects: map[string]rawRegistryEntry{
		"mbp14/codex/a.jsonl": {}, "mbp14/pi/b.jsonl": {}, "mbp140/pi/c.jsonl": {}, "vps/codex/d.jsonl": {},
	}}
	if removed := r.RemoveHost("mbp14"); removed != 2 {
		t.Fatalf("removed=%d", removed)
	}
	if len(r.Objects) != 2 {
		t.Fatalf("objects=%v", r.Objects)
	}
}

func TestRawRegistryCoalescesGrowingIndexedObjects(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	old := rawObject{Key: "mac/codex/rollout.jsonl", ETag: "etag-1", Size: 100}
	grown := rawObject{Key: old.Key, ETag: "etag-2", Size: 200}
	r := &rawRegistry{Objects: map[string]rawRegistryEntry{
		old.Key: {
			ETag:          old.ETag,
			Size:          old.Size,
			Status:        "indexed",
			ParserVersion: rawParserVersion,
			UpdatedAt:     now,
		},
	}}

	if r.Due(grown, now.Add(4*time.Minute), 5*time.Minute) {
		t.Fatal("growing indexed object was due before the coalescing interval")
	}
	if !r.Due(grown, now.Add(5*time.Minute), 5*time.Minute) {
		t.Fatal("growing indexed object was not due at the coalescing boundary")
	}
	if !r.Due(rawObject{Key: "mac/codex/new.jsonl", ETag: "new", Size: 1}, now, 5*time.Minute) {
		t.Fatal("new objects must be indexed immediately")
	}

	entry := r.Objects[old.Key]
	entry.ParserVersion = "previous-parser"
	r.Objects[old.Key] = entry
	if !r.Due(grown, now.Add(time.Second), 5*time.Minute) {
		t.Fatal("parser upgrades must bypass write coalescing")
	}
}

func TestRawIngestRetriesFailedEmbedding(t *testing.T) {
	body := []byte("{\"type\":\"session\",\"id\":\"s1\"}\n" +
		"{\"type\":\"message\",\"id\":\"u1\",\"message\":{\"role\":\"user\",\"content\":\"A retry must eventually succeed.\"}}\n")
	obj := rawObject{Key: "mac/pi/retry.jsonl", ETag: "retry-etag", Size: int64(len(body))}
	store := &fakeRawStore{objects: []rawObject{obj}, bodies: map[string][]byte{obj.Key: body}}
	s := testServer()
	s.cfg.RawMaxObjectBytes, s.cfg.EmbedBatchSize, s.cfg.EmbedURL = 1<<20, 32, "http://embed"
	registry, err := loadRawRegistry(t.TempDir() + "/raw.json")
	if err != nil {
		t.Fatal(err)
	}
	embedCalls := 0
	r := &rawIngestor{server: s, store: store, registry: registry, http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/tokenize" {
			return testTokenResponse(req)
		}
		embedCalls++
		if embedCalls == 1 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("not ready")), Header: make(http.Header)}, nil
		}
		return testEmbeddingResponse(req, 2)
	})}}
	r.scan(context.Background())
	entry := registry.Objects[obj.Key]
	if entry.Status != "failed" || entry.Attempts != 1 {
		t.Fatalf("first attempt=%+v", entry)
	}
	entry.UpdatedAt = entry.UpdatedAt.Add(-time.Minute)
	registry.Objects[obj.Key] = entry
	r.scan(context.Background())
	if !registry.Done(obj) || memRows(s) != 2 || embedCalls != 2 {
		t.Fatalf("entry=%+v rows=%d calls=%d", registry.Objects[obj.Key], memRows(s), embedCalls)
	}
}
