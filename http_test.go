package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadyChecksBackingStore(t *testing.T) {
	s := testServer()
	s.readyCheck = func(context.Context) error { return errors.New("missing Lance index file") }
	recorder := httptest.NewRecorder()
	s.ready(recorder, httptest.NewRequest(http.MethodGet, "/-/ready", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func testServer() *server {
	return &server{
		cfg:   runtimeConfig{Dimensions: 2},
		dedup: &dedupState{Files: map[string]string{}},
		mem:   newMemStore(2),
	}
}

func TestIngestQueryContainerHTTP(t *testing.T) {
	s := testServer()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)
	mux.HandleFunc("GET /v1/memory/container", s.containerWalk)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	user := Metadata{Source: "pi", Host: "mac", ContainerID: "s1", Project: "tireless-ledger", AuthorID: "user", Timestamp: 10}
	body := ingestRequest{Records: []MemoryItem{
		{ID: "mac/pi/s1/u1", Vector: []float32{0.1, 0.2}, Text: "opendata is weak for indexing codebases", Metadata: user},
		{ID: "mac/pi/s1/a1", Vector: []float32{0.2, 0.1}, Text: "walletActivation lives in swap-ui",
			Metadata: Metadata{Source: "pi", Host: "mac", ContainerID: "s1", Project: "swap-ui", AuthorID: "assistant", Timestamp: 11}},
		{ID: "mac/pi/s1/u1", Vector: []float32{0.1, 0.2}, Text: "opendata is weak for indexing codebases", Metadata: user},
	}}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/v1/memory/ingest", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ing ingestResponse
	if err := json.NewDecoder(resp.Body).Decode(&ing); err != nil {
		t.Fatal(err)
	}
	if ing.Accepted != 2 {
		t.Fatalf("accepted=%d", ing.Accepted)
	}
	if ing.Skipped != 1 {
		t.Fatalf("skipped=%d", ing.Skipped)
	}

	qb, _ := json.Marshal(queryRequest{QueryText: "opendata code", Limit: 5})
	resp, err = http.Post(ts.URL+"/v1/memory/query", "application/json", bytes.NewReader(qb))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var qr queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) == 0 || qr.Results[0].ID != "mac/pi/s1/u1" || qr.Results[0].Metadata["matched_chunk_id"] != "mac/pi/s1/u1#0" {
		t.Fatalf("query=%+v", qr.Results)
	}

	resp, err = http.Get(ts.URL + "/v1/memory/container?container_id=s1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) != 2 {
		t.Fatalf("container walk=%d", len(qr.Results))
	}
	resp, err = http.Get(ts.URL + "/v1/memory/container?container_id=s1&limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) != 1 || qr.Results[0].ID != "mac/pi/s1/u1" {
		t.Fatalf("page1=%+v", qr.Results)
	}
	resp, err = http.Get(ts.URL + "/v1/memory/container?container_id=s1&limit=1&after_ts=10&after_id=mac/pi/s1/u1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) != 1 || qr.Results[0].ID != "mac/pi/s1/a1" {
		t.Fatalf("page2=%+v", qr.Results)
	}
}

func TestSessionsWindow(t *testing.T) {
	s := testServer()
	msg := func(id, container, author string, ts int64, text string) MemoryItem {
		return MemoryItem{ID: id, Text: text, Metadata: Metadata{Source: "omp", Host: "mac", ContainerID: container, Project: "p", AuthorID: author, Kind: "message", Timestamp: ts}}
	}
	title := msg("mac/omp/s1/title/t", "s1", "system", 150, "a title")
	title.Metadata.Kind = "title"
	s.mem.upsert([]MemoryItem{
		msg("mac/omp/s1/u0", "s1", "user", 50, "before the window"),
		msg("mac/omp/s1/a1", "s1", "assistant", 100, "assistant first"),
		msg("mac/omp/s1/u1", "s1", "user", 120, "first user prompt in window"),
		msg("mac/omp/s1/u2", "s1", "user", 130, "second user prompt"),
		title,
		msg("mac/omp/s2/u1", "s2", "user", 110, "other session"),
		msg("mac/omp/s2/u2", "s2", "user", 200, "at until, excluded"),
		{ID: "tg/1", Text: "chat line", Metadata: Metadata{Source: "telegram", Host: "tg", ContainerID: "c1", AuthorID: "42", Kind: "message", Timestamp: 140}},
	}, nil)
	get := func(query string) (int, []sessionSummary) {
		rec := httptest.NewRecorder()
		s.sessions(rec, httptest.NewRequest(http.MethodGet, "/v1/memory/sessions?"+query, nil))
		var out struct{ Sessions []sessionSummary }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Sessions
	}
	if code, _ := get("until=200"); code != http.StatusBadRequest {
		t.Fatalf("missing since: status=%d", code)
	}
	_, got := get("since=100&until=200")
	want := []sessionSummary{
		{ContainerID: "s1", Host: "mac", Source: "omp", Project: "p", FirstTS: 100, LastTS: 130, Messages: 3, FirstPrompt: "first user prompt in window"},
		{ContainerID: "s2", Host: "mac", Source: "omp", Project: "p", FirstTS: 110, LastTS: 110, Messages: 1, FirstPrompt: "other session"},
		{ContainerID: "c1", Host: "tg", Source: "telegram", FirstTS: 140, LastTS: 140, Messages: 1, FirstPrompt: "chat line"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sessions=%+v", got)
	}
	if _, got := get("since=100&source=telegram"); len(got) != 1 || got[0].ContainerID != "c1" {
		t.Fatalf("source filter=%+v", got)
	}
}
