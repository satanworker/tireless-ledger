package main

import (
	"strings"
	"testing"
)

func TestLanceQuoteAndFilterSQL(t *testing.T) {
	if got, want := lanceQuote(`a\b'c`), `'a\\b''c'`; got != want {
		t.Fatalf("quote=%q want=%q", got, want)
	}
	f := &vectorFilter{And: []vectorFilter{
		{Eq: &comparisonFilter{Field: "container_id", Value: "s'1"}},
		{Eq: &comparisonFilter{Field: "host", Value: "mac"}},
		{Eq: &comparisonFilter{Field: "kind", Value: "todo"}},
	}}
	if got, want := lanceFilterSQL(f), "(container_id = 's''1' AND host = 'mac' AND kind = 'todo')"; got != want {
		t.Fatalf("filter=%q want=%q", got, want)
	}
	if got := lanceFilterSQL(&vectorFilter{Eq: &comparisonFilter{Field: "timestamp", Value: 1}}); got != "" {
		t.Fatalf("unsupported field leaked into SQL: %q", got)
	}
}

func TestLanceCursorSQL(t *testing.T) {
	cases := []struct {
		ts   int64
		id   string
		want string
	}{
		{0, "", ""},
		{0, "a", "id > 'a'"},
		{10, "", "timestamp >= 10"}, // must match afterCursor: walking from a hit's ts includes the hit
		{10, "a'b", "(timestamp > 10 OR (timestamp = 10 AND id > 'a''b'))"},
	}
	for _, tc := range cases {
		if got := lanceCursorSQL(tc.ts, tc.id); got != tc.want {
			t.Errorf("cursor(%d,%q)=%q want=%q", tc.ts, tc.id, got, tc.want)
		}
	}
}

// A migrated v4 row must get the id the raw parser derives for the same turn, or the
// post-cutover raw re-scan re-embeds it under a second id.
func TestV4MessageIDMatchesRawRecord(t *testing.T) {
	for _, tc := range []struct{ path, turn, kind string }{
		{"h/codex/s/message/line-3", "line-3", "message"},
		{"h/omp/s/379c5ced", "379c5ced", "message"},
		{"h/omp/s/todo/t1", "t1", "todo"},
		{"h/omp/s/title/t1", "t1", "title"},
	} {
		id, kind := v4MessageID(tc.path, "v4-id")
		want := rawRecord("h", strings.Split(tc.path, "/")[1], "s", tc.turn, "user", "x", 1, "", tc.kind).ID
		if id != want || kind != tc.kind {
			t.Errorf("v4MessageID(%q)=%q,%q want %q,%q", tc.path, id, kind, want, tc.kind)
		}
	}
}
