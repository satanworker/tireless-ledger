package index

import (
	"strings"
	"testing"
)

func TestClipChars(t *testing.T) {
	if clipChars("short", 800) != "short" {
		t.Fatal("short")
	}
	raw := "**ClickHouse serving-layer.md**"
	if clipChars(raw, 800) != raw {
		t.Fatal("raw markdown")
	}
	long := strings.Repeat("ä", 801)
	got := clipChars(long, 800)
	if []rune(got)[0] != 'ä' || len([]rune(got)) != 800 {
		t.Fatalf("runes=%d", len([]rune(got)))
	}
}

func TestParsePi(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"session","version":3,"id":"sess123","timestamp":"2026-07-14T07:53:22.423Z","cwd":"/Users/x/Developer/tireless-ledger"}`,
		`{"type":"message","id":"u1","timestamp":"2026-07-14T07:53:36.796Z","message":{"role":"user","content":[{"type":"text","text":"is it a good solution for indexing codebases as well?"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-07-14T07:53:44.819Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"OK as shared vector store. Weak as full indexer."}]}}`,
		`{"type":"message","id":"t1","timestamp":"2026-07-14T07:53:40Z","message":{"role":"toolResult","content":[{"type":"text","text":"ignore me please"}]}}`,
		"",
	}, "\n")
	st := &fileState{Source: "pi", Host: "mac", Project: "unknown"}
	turns, n, err := ReadTurns(strings.NewReader(raw), st)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(raw)) {
		t.Fatalf("consumed=%d want=%d", n, len(raw))
	}
	if len(turns) != 2 || turns[0].Author != "user" || turns[1].Author != "assistant" {
		t.Fatalf("turns=%+v", turns)
	}
	if turns[0].Container != "sess123" || turns[0].Project != "tireless-ledger" {
		t.Fatalf("meta=%+v", turns[0])
	}
	if strings.Contains(turns[1].Text, "thinking") {
		t.Fatal("kept thinking")
	}
}

func TestParseOmpKeepsSource(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"session","version":3,"id":"omp123","timestamp":"2026-09-01T18:19:23.097Z","cwd":"/Users/x/Developer/satanworker"}`,
		`{"type":"message","id":"u1","timestamp":"2026-09-01T18:20:42.948Z","message":{"role":"user","content":[{"type":"text","text":"sync omp history please"}]}}`,
		"",
	}, "\n")
	st := &fileState{Source: "omp", Host: "mac", Project: "unknown"}
	turns, _, err := ReadTurns(strings.NewReader(raw), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Source != "omp" || turns[0].Container != "omp123" {
		t.Fatalf("turns=%+v", turns)
	}
	if turns[0].ID != "mac/omp/omp123/u1" {
		t.Fatalf("id=%s", turns[0].ID)
	}
}

func TestParseCodexSkipAgents(t *testing.T) {
	user := "when I run unsloth studio, I cannot select Qwen"
	raw := strings.Join([]string{
		`{"timestamp":"2026-06-03T21:09:41.734Z","type":"session_meta","payload":{"id":"019e8f52-0cc9-7e91-a2bc-daea4b591f58","cwd":"/Users/x/swap-ui"}}`,
		`{"timestamp":"2026-06-03T21:09:42.850Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /tmp\nskip"}]}}`,
		`{"timestamp":"2026-06-03T21:09:42.857Z","type":"response_item","payload":{"type":"message","id":"msg-user","role":"user","content":[{"type":"input_text","text":"` + user + `"}]}}`,
		`{"timestamp":"2026-06-03T21:09:50Z","type":"event_msg","payload":{"type":"user_message","message":"` + user + `"}}`,
		`{"timestamp":"2026-06-03T21:10:00Z","type":"response_item","payload":{"type":"message","id":"msg-assistant","role":"assistant","content":[{"type":"output_text","text":"Studio picker is GGUF only, your Qwen is MLX."}]}}`,
		`{"timestamp":"2026-06-03T21:10:01Z","type":"response_item","payload":{"type":"message","id":"msg-assistant","role":"assistant","content":[{"type":"output_text","text":"Studio picker is GGUF only, your Qwen is MLX."}]}}`,
		"",
	}, "\n")
	st := &fileState{Source: "codex", Host: "mac", Project: "unknown"}
	turns, _, err := ReadTurns(strings.NewReader(raw), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Author != "user" || turns[1].Author != "assistant" {
		t.Fatalf("turns=%+v", turns)
	}
	if turns[0].Container != "019e8f52-0cc9-7e91-a2bc-daea4b591f58" || turns[0].Project != "swap-ui" {
		t.Fatalf("meta=%+v", turns[0])
	}
	if turns[0].ID != "mac/codex/019e8f52-0cc9-7e91-a2bc-daea4b591f58/msg-user" {
		t.Fatalf("id=%s", turns[0].ID)
	}
	if strings.Contains(turns[0].Text, "AGENTS.md") {
		t.Fatal("kept agents dump")
	}
}

func TestReadTurnsPartialLine(t *testing.T) {
	st := &fileState{Source: "pi", Host: "mac", Container: "s", Project: "p"}
	full := "{\"type\":\"message\",\"id\":\"u1\",\"timestamp\":\"2026-07-14T07:53:36Z\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hello there friend\"}]}}\n{\"type\":\"message\""
	turns, n, err := ReadTurns(strings.NewReader(full), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns=%d", len(turns))
	}
	if int(n) >= len(full) {
		t.Fatalf("consumed partial line n=%d len=%d", n, len(full))
	}
}
