package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSessionUnitsCarryTitleAndQuestion(t *testing.T) {
	items := []MemoryItem{
		rawRecord("box", "claude", "s1", "u1", "user", "is   auto-titling\nautomated now?", 1, "imp-home", "message"),
		rawRecord("box", "claude", "s1", "a1", "assistant", "Yes, it's automated now.", 2, "imp-home", "message"),
		rawRecord("box", "claude", "s1", "t1", "system", "TITLE ai-title: Auto-title threads", 3, "imp-home", "title"),
	}
	units := chunkGroups(items, func(MemoryItem, string) bool { return true })
	if len(units) != 3 {
		t.Fatalf("units=%+v", units)
	}
	answer := units[1]
	if answer.header != `Claude Code session "Auto-title threads" in imp-home` {
		t.Fatalf("header=%q", answer.header)
	}
	if answer.text != "Re: is auto-titling automated now?\nYes, it's automated now." {
		t.Fatalf("text=%q", answer.text)
	}
	// The title reaches the messages table too, so hits show it.
	if answer.pending[0].Metadata.ContainerTitle != "Auto-title threads" || answer.pending[0].Text != "Yes, it's automated now." {
		t.Fatalf("pending=%+v", answer.pending[0])
	}
	if h := contextHeader(Metadata{Source: "crm", ContainerID: "org:borsa", ContainerTitle: "Borsa"}); h != `CRM notes on "Borsa"` {
		t.Fatalf("crm header=%q", h)
	}
}

// fakeEmbedder records what reaches the model and answers with a fixed vector.
type fakeEmbedder struct {
	mu     sync.Mutex
	inputs []string
}

func (f *fakeEmbedder) handler(dims int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokenize" {
			resp, _ := testTokenResponse(r)
			io.Copy(w, resp.Body)
			return
		}
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.inputs = append(f.inputs, in.Input...)
		f.mu.Unlock()
		data := make([]map[string]interface{}, len(in.Input))
		for i := range data {
			vector := make([]float32, dims)
			vector[0] = 1
			data[i] = map[string]interface{}{"index": i, "embedding": vector}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	}
}

func TestTextPushQueryEmbedAndDuplicateCollapse(t *testing.T) {
	embedder := &fakeEmbedder{}
	embed := httptest.NewServer(embedder.handler(2))
	defer embed.Close()
	s := testServer()
	s.dedup.Path = t.TempDir() + "/dedup.json"
	s.cfg.EmbedURL, s.cfg.EmbedBatchSize = embed.URL, 32
	s.cfg.QueryPrefix, s.cfg.DocPrefix = "query: ", "doc: "
	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/ready", s.ready)
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	post := func(path string, v interface{}, out interface{}) {
		t.Helper()
		b, _ := json.Marshal(v)
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: HTTP %d %s", path, resp.StatusCode, b)
		}
		json.NewDecoder(resp.Body).Decode(out)
	}

	profile := MemoryItem{ID: "crm/org:borsa", Text: "Borsa is a HIP-3 perp DEX team.",
		Metadata: Metadata{Source: "crm", Host: "crm", ContainerID: "org:borsa", ContainerTitle: "Borsa", Kind: "profile", Timestamp: 5}}
	prompt := "You are imp. Review pull request 42 carefully."
	records := []MemoryItem{profile}
	for i := 0; i < 3; i++ {
		records = append(records, rawRecord("imp", "claude", fmt.Sprintf("s%d", i), "u1", "user", prompt, 10, "prime", "message"))
	}
	var ing ingestResponse
	post("/v1/memory/ingest", ingestRequest{Records: records}, &ing)
	if ing.Accepted != 4 || len(ing.Errors) != 0 {
		t.Fatalf("ingest=%+v", ing)
	}
	if got := embedder.inputs[0]; got != "doc: CRM notes on \"Borsa\"\nBorsa is a HIP-3 perp DEX team." {
		t.Fatalf("embedded=%q", got)
	}
	// An unchanged push is skipped, a changed one re-embedded under the same id.
	post("/v1/memory/ingest", ingestRequest{Records: []MemoryItem{profile}}, &ing)
	if ing.Accepted != 0 || ing.Skipped != 1 {
		t.Fatalf("repeat=%+v", ing)
	}
	profile.Text = "Borsa is a HIP-3 perp DEX team; Valantis supplies the stake."
	post("/v1/memory/ingest", ingestRequest{Records: []MemoryItem{profile}}, &ing)
	if ing.Accepted != 1 {
		t.Fatalf("changed=%+v", ing)
	}

	var ready map[string]interface{}
	resp, err := http.Get(ts.URL + "/-/ready")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&ready)
	resp.Body.Close()
	if ready["query_embed"] != true {
		t.Fatalf("ready=%v", ready)
	}

	yes := true
	var out queryResponse
	post("/v1/memory/query", queryRequest{QueryText: "review pull request", Embed: &yes, Limit: 10}, &out)
	if got := embedder.inputs[len(embedder.inputs)-1]; got != "query: review pull request" {
		t.Fatalf("query embedded as %q", got)
	}
	prompts := 0
	for _, hit := range out.Results {
		if strings.Contains(hit.Text, prompt) {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("the same prompt in 3 sessions must show once: %+v", out.Results)
	}
	// A vector from another model (an old client) is replaced by the server's own.
	before := len(embedder.inputs)
	post("/v1/memory/query", queryRequest{QueryText: "perp", QueryVector: []float32{1, 0, 0}, Limit: 3}, &out)
	if len(embedder.inputs) != before+1 || len(out.Results) == 0 {
		t.Fatalf("stale vector not re-embedded: inputs=%d results=%d", len(embedder.inputs)-before, len(out.Results))
	}
}

func TestLongUnitRepeatsHeaderOnEveryChunk(t *testing.T) {
	embedder := &fakeEmbedder{}
	embed := httptest.NewServer(embedder.handler(2))
	defer embed.Close()
	s := testServer()
	s.dedup.Path = t.TempDir() + "/dedup.json"
	s.cfg.EmbedURL, s.cfg.ChunkTokens = embed.URL, 200
	words := make([]string, 600)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	item := rawRecord("box", "codex", "s1", "a1", "assistant", strings.Join(words, " "), 1, "repo", "message")
	groups := chunkGroups([]MemoryItem{item}, func(MemoryItem, string) bool { return true })
	if _, err := s.indexer().indexGroups(t.Context(), groups, func(m MemoryItem) string { return m.ID }); err != nil {
		t.Fatal(err)
	}
	if len(embedder.inputs) < 3 {
		t.Fatalf("chunks=%d", len(embedder.inputs))
	}
	for i, text := range embedder.inputs {
		if !strings.HasPrefix(text, "Codex session in repo\n") || len(strings.Fields(text)) > 200 {
			t.Fatalf("chunk %d: %d tokens, %q", i, len(strings.Fields(text)), text[:40])
		}
	}
}

func TestCollapseKeepsOneHitPerTelegramMoment(t *testing.T) {
	hit := func(id string, ts int64, container string) queryResult {
		return queryResult{ID: id + "#0", Metadata: map[string]interface{}{"message_id": id, "source": "telegram", "container_id": container, "timestamp": float64(ts), "content_hash": id}}
	}
	hits := collapseChunkHits([]queryResult{
		hit("m2", 1060, "-1"), hit("m3", 1120, "-1"), hit("x1", 1100, "-2"), hit("m9", 1000+telegramLeadGap+100, "-1"),
	}, 10)
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "m2,x1,m9" {
		t.Fatalf("ids=%v", ids)
	}
}
