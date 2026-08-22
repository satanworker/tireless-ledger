package main

import (
	"context"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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
	r := &rawIngestor{server: s, store: store, registry: registry, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)), Header: make(http.Header)}, nil
	})}}
	s.raw = r
	r.scan(context.Background())
	if !registry.Done(obj) {
		t.Fatalf("registry=%+v", registry.Objects[obj.Key])
	}
	if store.gets != 1 {
		t.Fatalf("gets=%d", store.gets)
	}
	if len(s.mem.items) != 1 {
		t.Fatalf("indexed=%d", len(s.mem.items))
	}
	r.scan(context.Background())
	if store.gets != 1 {
		t.Fatalf("completed object downloaded again: gets=%d", store.gets)
	}
	if len(s.mem.items) != 1 {
		t.Fatalf("duplicate rows=%d", len(s.mem.items))
	}

	// A parser version change invalidates only derived state and safely replays.
	entry := registry.Objects[obj.Key]
	entry.ParserVersion = "old-parser"
	registry.Objects[obj.Key] = entry
	r.scan(context.Background())
	if store.gets != 2 {
		t.Fatalf("parser upgrade was not replayed: gets=%d", store.gets)
	}
	if len(s.mem.items) != 1 {
		t.Fatalf("replay duplicated rows=%d", len(s.mem.items))
	}
}

func TestTruncateRunesDoesNotSplitUTF8(t *testing.T) {
	if got := truncateRunes("ab🙂cd", 3); got != "ab🙂" {
		t.Fatalf("got=%q", got)
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
	calls := 0
	r := &rawIngestor{server: s, store: store, registry: registry, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("not ready")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)), Header: make(http.Header)}, nil
	})}}
	r.scan(context.Background())
	entry := registry.Objects[obj.Key]
	if entry.Status != "failed" || entry.Attempts != 1 {
		t.Fatalf("first attempt=%+v", entry)
	}
	entry.UpdatedAt = entry.UpdatedAt.Add(-time.Minute)
	registry.Objects[obj.Key] = entry
	r.scan(context.Background())
	if !registry.Done(obj) || len(s.mem.items) != 1 || calls != 2 {
		t.Fatalf("entry=%+v rows=%d calls=%d", registry.Objects[obj.Key], len(s.mem.items), calls)
	}
}
