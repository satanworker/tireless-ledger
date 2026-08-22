package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestIngestQuerySessionHTTP(t *testing.T) {
	s := testServer()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)
	mux.HandleFunc("GET /v1/memory/session", s.sessionWalk)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := ingestRequest{Records: []MemoryItem{{
		ID: "t1", Vector: []float32{0.1, 0.2}, ForwardContent: "opendata is weak for indexing codebases",
		Metadata: Metadata{Scope: ScopeSession, ProjectName: "tireless-ledger", FilePath: "mac/pi/s1/u1", FileHash: "h1", Timestamp: 10, SessionID: "s1", Host: "mac", Harness: "pi", Role: "user"},
	}, {
		ID: "t2", Vector: []float32{0.2, 0.1}, ForwardContent: "walletActivation lives in swap-ui",
		Metadata: Metadata{Scope: ScopeSession, ProjectName: "swap-ui", FilePath: "mac/pi/s1/a1", FileHash: "h2", Timestamp: 11, SessionID: "s1", Host: "mac", Harness: "pi", Role: "assistant"},
	}, {
		ID: "t1", Vector: []float32{0.1, 0.2}, ForwardContent: "opendata is weak for indexing codebases",
		Metadata: Metadata{Scope: ScopeSession, ProjectName: "tireless-ledger", FilePath: "mac/pi/s1/u1", FileHash: "h1", Timestamp: 10, SessionID: "s1", Host: "mac", Harness: "pi", Role: "user"},
	}}}
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
	if len(qr.Results) == 0 || qr.Results[0].ID != "t1" {
		t.Fatalf("query=%+v", qr.Results)
	}

	resp, err = http.Get(ts.URL + "/v1/memory/session?session_id=s1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) != 2 {
		t.Fatalf("session walk=%d", len(qr.Results))
	}
	resp, err = http.Get(ts.URL + "/v1/memory/session?session_id=s1&limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) != 1 || qr.Results[0].ID != "t1" {
		t.Fatalf("page1=%+v", qr.Results)
	}
	resp, err = http.Get(ts.URL + "/v1/memory/session?session_id=s1&limit=1&after_ts=10&after_id=t1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		t.Fatal(err)
	}
	if len(qr.Results) != 1 || qr.Results[0].ID != "t2" {
		t.Fatalf("page2=%+v", qr.Results)
	}
}
