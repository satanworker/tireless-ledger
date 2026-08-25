package main

import "testing"

func TestLanceQuoteAndFilterSQL(t *testing.T) {
	if got, want := lanceQuote(`a\b'c`), `'a\\b''c'`; got != want {
		t.Fatalf("quote=%q want=%q", got, want)
	}
	f := &vectorFilter{And: []vectorFilter{
		{Eq: &comparisonFilter{Field: "session_id", Value: "s'1"}},
		{Eq: &comparisonFilter{Field: "host", Value: "mac"}},
		{Eq: &comparisonFilter{Field: "record_kind", Value: "chunk"}},
	}}
	if got, want := lanceFilterSQL(f), "(session_id = 's''1' AND host = 'mac' AND record_kind = 'chunk')"; got != want {
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
		{10, "", "timestamp > 10"},
		{10, "a'b", "(timestamp > 10 OR (timestamp = 10 AND id > 'a''b'))"},
	}
	for _, tc := range cases {
		if got := lanceCursorSQL(tc.ts, tc.id); got != tc.want {
			t.Errorf("cursor(%d,%q)=%q want=%q", tc.ts, tc.id, got, tc.want)
		}
	}
}
