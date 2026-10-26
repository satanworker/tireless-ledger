package index

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestClientWireFormat(t *testing.T) {
	var ingest struct {
		Records []map[string]any `json:"records"`
	}
	var query map[string]any
	var walk url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": make([]float32, Dims)}}})
		case "/v1/memory/ingest":
			_ = json.NewDecoder(r.Body).Decode(&ingest)
			_, _ = w.Write([]byte(`{"accepted":1}`))
		case "/v1/memory/query":
			_ = json.NewDecoder(r.Body).Decode(&query)
			_, _ = w.Write([]byte(`{"results":[]}`))
		case "/v1/memory/container":
			walk = r.URL.Query()
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.URL, "mac")

	turn := makeTurn("mac", "pi", "s1", "u1", "user", "hello there", 42, "proj")
	if _, err := c.Ingest([]Turn{turn}); err != nil {
		t.Fatal(err)
	}
	rec := ingest.Records[0]
	meta := rec["metadata"].(map[string]any)
	if rec["id"] != "mac/pi/s1/u1" || rec["text"] != "hello there" || meta["container_id"] != "s1" || meta["source"] != "pi" || meta["author_id"] != "user" || meta["project"] != "proj" || meta["kind"] != "message" || len(meta) != 7 {
		t.Fatalf("ingest record=%v", rec)
	}

	if _, err := c.Query("hi", false, map[string]string{"container_id": "s1", "source": "", "kind": "todo"}, 3); err != nil {
		t.Fatal(err)
	}
	if query["container_id"] != "s1" || query["kind"] != "todo" || query["query_text"] != "hi" || len(query) != 4 {
		t.Fatalf("query=%v", query)
	}

	if _, err := c.Container(map[string]string{"container_id": "-100:7", "host": "", "after_ts": "5", "include": "todos"}, 8); err != nil {
		t.Fatal(err)
	}
	if walk.Get("container_id") != "-100:7" || walk.Get("after_ts") != "5" || walk.Get("include") != "todos" || walk.Get("limit") != "8" || walk.Has("host") {
		t.Fatalf("container=%v", walk)
	}
}
