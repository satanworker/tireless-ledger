package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestXChatIndexesLikeTelegram(t *testing.T) {
	lines := strings.Join([]string{
		`{"id":"1","chat_id":"c42","chat_type":"dm","chat_title":"Dima","topic_id":null,"date":1000,"sender_id":"9","sender_name":"Dima","sender_handle":"dima_v","encrypted":true,"verified":false,"message_id":"m-1","source_account":"devin","reply_to":null,"media":null,"text":"is the vault live?","unknown_field":{"a":1}}`,
		`{"id":"2","chat_id":"c42","chat_type":"dm","chat_title":"Dima","date":1060,"sender_id":"7","sender_name":"Devin","sender_handle":"devin","encrypted":"yes","source_account":"devin","reply_to":"1","text":"yes, since noon"}`,
		`{"id":"3","chat_id":"c42","chat_type":"dm","chat_title":"Dima","date":1100,"sender_id":"9","sender_name":"Dima","media":"photo","text":""}`,
		`{"id":"4","chat_id":"c42","chat_type":"dm","chat_title":"Dima","date":1120,"sender_id":"9","sender_name":"Dima","text":"great, sharing with the team"}`,
	}, "\n")
	obj := rawObject{Key: "devin/x/c42/2026-11.jsonl", ETag: "e1", Size: int64(len(lines))}
	tg := rawObject{Key: "tgbox/telegram/c42/2026-11.jsonl", ETag: "e2", Size: 1}
	store := &fakeRawStore{objects: []rawObject{obj, tg}, bodies: map[string][]byte{obj.Key: []byte(lines),
		tg.Key: []byte(`{"id":"1","chat_id":"c42","chat_type":"group","chat_title":"Core","date":1010,"sender_id":"a","sender_name":"Alice","text":"unrelated telegram line"}`)}}

	items, err := parseRawSessionForIndex(obj.Key, []byte(lines))
	if err != nil || len(items) != 3 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	first := items[0]
	if first.ID != "devin/x/c42/1" || first.Metadata.Source != "x" || first.Metadata.ContainerKind != "dm" || first.Metadata.ReplyTo != "" ||
		first.Metadata.Attrs != `{"encrypted":true,"message_id":"m-1","sender_handle":"dima_v","source_account":"devin","verified":false}` {
		t.Fatalf("first=%+v", first)
	}
	if items[1].Metadata.ReplyTo != "devin/x/c42/1" || items[1].Metadata.Attrs != `{"encrypted":"yes","sender_handle":"devin","source_account":"devin"}` {
		t.Fatalf("second=%+v", items[1])
	}

	embedder := &fakeEmbedder{}
	embed := httptest.NewServer(embedder.handler(2))
	defer embed.Close()
	s := testServer()
	s.dedup.Path = t.TempDir() + "/dedup.json"
	s.cfg.RawMaxObjectBytes, s.cfg.EmbedBatchSize, s.cfg.EmbedURL = 1<<20, 32, embed.URL
	registry, err := loadRawRegistry(t.TempDir() + "/raw.json")
	if err != nil {
		t.Fatal(err)
	}
	s.raw = &rawIngestor{server: s, store: store, registry: registry, http: http.DefaultClient}
	s.raw.scan(context.Background())

	// Two-line lead-in from the same X conversation only (not the Telegram chat c42).
	want := "X DM with \"Dima\"\nDima: is the vault live?\nDevin: yes, since noon\nDima: great, sharing with the team"
	if !strings.Contains(strings.Join(embedder.inputs, "\n---\n"), want) {
		t.Fatalf("embedded=%q", embedder.inputs)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/memory/query", s.query)
	mux.HandleFunc("GET /v1/raw/status", s.rawStatus)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	yes := true
	b, _ := json.Marshal(queryRequest{QueryText: "vault", Embed: &yes, Source: "x", Limit: 10})
	resp, err := http.Post(ts.URL+"/v1/memory/query", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	var out queryResponse
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	// All three X messages fall within one 10-minute moment: one hit.
	if len(out.Results) != 1 || out.Results[0].Metadata["source"] != "x" || !strings.HasPrefix(out.Results[0].Text, `X DM with "Dima"`) {
		t.Fatalf("results=%+v", out.Results)
	}

	resp, err = http.Get(ts.URL + "/v1/raw/status")
	if err != nil {
		t.Fatal(err)
	}
	var status rawStatusResponse
	json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if status.Indexed != 2 || status.Sources["x"].Indexed != 1 || status.Sources["telegram"].Indexed != 1 {
		t.Fatalf("status=%+v", status)
	}
}

func TestXHeaders(t *testing.T) {
	for _, c := range []struct{ kind, title, want string }{
		{"dm", "Dima", `X DM with "Dima"`}, {"group", "Core", `X group "Core"`}, {"dm", "", "X DM"}, {"", "", "X chat"},
	} {
		if got := contextHeader(Metadata{Source: "x", ContainerKind: c.kind, ContainerTitle: c.title}); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestCollapseKeepsChatSourcesApart(t *testing.T) {
	hit := func(id, source string) queryResult {
		return queryResult{ID: id, Metadata: map[string]interface{}{"source": source, "container_id": "c1", "timestamp": float64(100), "content_hash": id}}
	}
	if hits := collapseChunkHits([]queryResult{hit("t1", "telegram"), hit("x1", "x"), hit("x2", "x")}, 10); len(hits) != 2 {
		t.Fatalf("hits=%+v", hits)
	}
}

// Chat platforms go through isChatSource; nothing else compares source to "telegram".
func TestNoTelegramLiteralOutsideHelper(t *testing.T) {
	re := regexp.MustCompile(`"telegram"`)
	files, _ := os.ReadDir(".")
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(name)
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) && !strings.HasPrefix(line, "var chatSources") && !strings.HasPrefix(line, "var rawSources") {
				t.Errorf("%s:%d: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
