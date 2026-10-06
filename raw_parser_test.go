package main

import (
	"strings"
	"testing"
)

func TestParseRawPiPreservesText(t *testing.T) {
	body := []byte(`{"type":"session","id":"pi-session","cwd":"/Users/me/project"}
{"type":"message","id":"u1","timestamp":"2026-08-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Keep **this formatting**, punctuation—and UTF-8."}]}}
{"type":"message","id":"a1","timestamp":"2026-08-22T10:01:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"Formatting remains exact."}]}}
`)
	items, err := parseRawSession("mbp14/pi/2026/session.jsonl", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items=%d", len(items))
	}
	if got := items[0].ForwardContent; got != "Keep **this formatting**, punctuation—and UTF-8." {
		t.Fatalf("text=%q", got)
	}
	if items[0].Metadata.Host != "mbp14" || items[0].Metadata.Harness != "pi" || items[0].Metadata.SessionID != "pi-session" {
		t.Fatalf("metadata=%+v", items[0].Metadata)
	}
	if items[1].ForwardContent != "Formatting remains exact." {
		t.Fatalf("assistant=%q", items[1].ForwardContent)
	}
}

func TestParseRawOmpKeepsHarnessAndTitlelessMessages(t *testing.T) {
	body := []byte(`{"type":"title","v":1,"title":"Fix tireless recall nix service syncing","source":"auto"}
{"type":"session","version":3,"id":"omp-session","timestamp":"2026-09-01T18:19:23.097Z","cwd":"/Users/me/Developer/NixOS/satanworker","title":"Fix tireless recall nix service syncing","titleSource":"auto"}
{"type":"model_change","id":"m1","model":"xai-oauth/grok-4.6"}
{"type":"message","id":"u1","timestamp":"2026-09-01T18:20:00Z","message":{"role":"user","content":[{"type":"text","text":"Prepare tireless-ledger backend for OMP sessions."}]}}
{"type":"message","id":"a1","timestamp":"2026-09-01T18:20:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"Backend now accepts OMP JSONL."}]}}
`)
	items, err := parseRawSession("mbp14/omp/-Developer-NixOS-satanworker/session.jsonl", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items=%d", len(items))
	}
	if items[0].Metadata.Host != "mbp14" || items[0].Metadata.Harness != "omp" || items[0].Metadata.SessionID != "omp-session" {
		t.Fatalf("metadata=%+v", items[0].Metadata)
	}
	if items[0].Metadata.ProjectName != "satanworker" {
		t.Fatalf("project=%q", items[0].Metadata.ProjectName)
	}
	if items[1].ForwardContent != "Backend now accepts OMP JSONL." {
		t.Fatalf("assistant=%q", items[1].ForwardContent)
	}
}

func TestParseRawOmpIndexesImportantState(t *testing.T) {
	body := []byte(`{"type":"title","id":"title-1","title":"Serving table people search"}
{"type":"session","id":"omp-session","timestamp":"2026-09-01T18:19:23Z","cwd":"/repo/brimstone","title":"Serving table people search"}
{"type":"message","id":"todo-1","timestamp":"2026-09-01T18:20:00Z","message":{"role":"toolResult","toolName":"todo","details":{"phases":[{"phase":"Remote todo retrieval","items":[{"content":"Index todo snapshots for search","status":"pending"}]}]}}}
{"type":"custom","id":"edit-1","timestamp":"2026-09-01T18:20:01Z","customType":"user_todo_edit","data":{"phases":[{"phase":"Remote todo retrieval","items":[{"content":"Expose todo session entries","status":"in_progress"}]}]}}
{"type":"message","id":"task-1","timestamp":"2026-09-01T18:20:02Z","message":{"role":"toolResult","toolName":"task","content":[{"type":"text","text":"Subagent found serving.person_search rebuild notes."}]}}
`)
	items, err := parseRawSessionForIndex("mbp14/omp/-repo-brimstone/2026_omp-session.jsonl", body)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, item := range items {
		kinds[item.Metadata.RecordKind]++
		if item.Metadata.ProjectName != "brimstone" {
			t.Fatalf("project=%q", item.Metadata.ProjectName)
		}
	}
	if kinds["title"] != 2 || kinds["todo"] != 2 || kinds["task"] != 1 {
		t.Fatalf("kinds=%v", kinds)
	}
	if !strings.Contains(items[2].ForwardContent, "Index todo snapshots for search") {
		t.Fatalf("todo text=%q", items[2].ForwardContent)
	}
}

func TestParseRawCodexStableResumeID(t *testing.T) {
	body := []byte(`{"timestamp":"2026-08-22T10:00:00Z","type":"session_meta","payload":{"id":"thread-1","cwd":"/repo/tireless-ledger"}}
{"timestamp":"2026-08-22T10:00:01Z","type":"response_item","payload":{"type":"message","id":"msg-1","role":"user","content":[{"type":"input_text","text":"Build the raw upload pipeline."}]}}
{"timestamp":"2026-08-22T10:00:02Z","type":"response_item","payload":{"type":"message","id":"msg-1","role":"user","content":[{"type":"input_text","text":"Build the raw upload pipeline."}]}}
`)
	first, err := parseRawSession("mbp14/codex/2026/rollout-a.jsonl", body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseRawSession("mbp14/codex/2026/rollout-b.jsonl", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("first=%d second=%d", len(first), len(second))
	}
	if first[0].ID != second[0].ID {
		t.Fatalf("resume IDs differ: %s %s", first[0].ID, second[0].ID)
	}
	if first[0].Metadata.ProjectName != "tireless-ledger" {
		t.Fatalf("project=%q", first[0].Metadata.ProjectName)
	}
}

func TestParseRawClaudeSession(t *testing.T) {
	body := []byte(`{"type":"ai-title","aiTitle":"Claude ingestion","sessionId":"c1"}
{"type":"user","uuid":"m0","isMeta":true,"timestamp":"2026-09-30T10:00:00Z","cwd":"/repo/tireless-ledger","sessionId":"c1","message":{"role":"user","content":"<local-command-caveat>Caveat: ignore</local-command-caveat>"}}
{"type":"user","uuid":"u1","timestamp":"2026-09-30T10:00:01Z","cwd":"/repo/tireless-ledger","sessionId":"c1","message":{"role":"user","content":"Index Claude sessions too."}}
{"type":"assistant","uuid":"a1","timestamp":"2026-09-30T10:00:02Z","sessionId":"c1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"Adding a claude harness."}]}}
{"type":"assistant","uuid":"a2","timestamp":"2026-09-30T10:00:03Z","sessionId":"c1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t-todo","name":"TodoWrite","input":{"todos":[{"content":"Parse Claude JSONL","status":"in_progress"}]}},{"type":"tool_use","id":"t-sync","name":"Agent","input":{}},{"type":"tool_use","id":"t-async","name":"Agent","input":{}},{"type":"tool_use","id":"t-bash","name":"Bash","input":{}}]}}
{"type":"user","uuid":"r1","timestamp":"2026-09-30T10:00:04Z","sessionId":"c1","toolUseResult":{"status":"completed"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-sync","content":[{"type":"text","text":"Subagent found the uploader roots."}]}]}}
{"type":"user","uuid":"r2","timestamp":"2026-09-30T10:00:05Z","sessionId":"c1","toolUseResult":{"isAsync":true},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-async","content":"Async agent launched successfully."}]}}
{"type":"user","uuid":"r3","timestamp":"2026-09-30T10:00:06Z","sessionId":"c1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-bash","content":"bash output"}]}}
{"type":"user","uuid":"u2","timestamp":"2026-09-30T10:00:07Z","sessionId":"c1","message":{"role":"user","content":"<local-command-stdout>Set model</local-command-stdout>"}}
{"type":"ai-title","aiTitle":"Claude ingestion","sessionId":"c1"}
`)
	items, err := parseRawSessionForIndex("home-satan/claude/-repo-tireless-ledger/c1.jsonl", body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, item := range items {
		if item.Metadata.Harness != "claude" || item.Metadata.SessionID != "c1" || item.Metadata.ProjectName != "tireless-ledger" {
			t.Fatalf("metadata=%+v", item.Metadata)
		}
		got[item.Metadata.RecordKind] = append(got[item.Metadata.RecordKind], item.ForwardContent)
	}
	if len(got["message"]) != 2 || got["message"][0] != "Index Claude sessions too." || got["message"][1] != "Adding a claude harness." {
		t.Fatalf("messages=%q", got["message"])
	}
	if len(got["title"]) != 1 || got["title"][0] != "TITLE ai-title: Claude ingestion" {
		t.Fatalf("titles=%q", got["title"])
	}
	if len(got["todo"]) != 1 || !strings.Contains(got["todo"][0], "Parse Claude JSONL") {
		t.Fatalf("todos=%q", got["todo"])
	}
	if len(got["task"]) != 1 || !strings.Contains(got["task"][0], "Subagent found the uploader roots.") {
		t.Fatalf("tasks=%q", got["task"])
	}

	sub, err := parseRawSession("home-satan/claude/-repo-tireless-ledger/c1/subagents/agent-a9.jsonl", []byte(`{"type":"user","uuid":"s1","isSidechain":true,"agentId":"a9","sessionId":"c1","cwd":"/repo/tireless-ledger","message":{"role":"user","content":"Subagent brief for Claude parsing."}}`))
	if err != nil || len(sub) != 1 || sub[0].Metadata.SessionID != "agent-a9" {
		t.Fatalf("subagent=%+v err=%v", sub, err)
	}
}

func TestRawObjectIdentityRejectsUnknownFiles(t *testing.T) {
	if _, _, ok := rawObjectIdentity("mbp14/notes/file.txt"); ok {
		t.Fatal("accepted unsupported key")
	}
	if host, harness, ok := rawObjectIdentity("mbp14/codex/a.jsonl"); !ok || host != "mbp14" || harness != "codex" {
		t.Fatalf("host=%q harness=%q ok=%v", host, harness, ok)
	}
	if host, harness, ok := rawObjectIdentity("mbp14/omp/a.jsonl"); !ok || host != "mbp14" || harness != "omp" {
		t.Fatalf("host=%q harness=%q ok=%v", host, harness, ok)
	}
	if host, harness, ok := rawObjectIdentity("home-satan/claude/-repo/c1.jsonl"); !ok || host != "home-satan" || harness != "claude" {
		t.Fatalf("host=%q harness=%q ok=%v", host, harness, ok)
	}
}
