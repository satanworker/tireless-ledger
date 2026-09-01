package main

import "testing"

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
}
