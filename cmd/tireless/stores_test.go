package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreLinesRoundTrip(t *testing.T) {
	lines, err := storeLines([]string{"personal=100.127.82.49", "valantis=100.97.80.65"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stores")
	if err := os.WriteFile(path, []byte("# comment\n"+lines), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadStores(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []store{
		{"personal", "http://100.127.82.49:8090", "http://100.127.82.49:8091"},
		{"valantis", "http://100.97.80.65:8090", "http://100.97.80.65:8091"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range [][]string{nil, {"x"}, {"=h"}, {"a="}, {"a=b", "a=c"}} {
		if _, err := storeLines(bad); err == nil {
			t.Errorf("storeLines(%q) accepted", bad)
		}
	}
	if s, _ := loadStores(filepath.Join(t.TempDir(), "missing")); s != nil {
		t.Fatalf("missing file gave %+v", s)
	}
}

func TestPick(t *testing.T) {
	all := []store{{name: "personal"}, {name: "valantis"}}
	if s, _ := pick(all, ""); len(s) != 2 {
		t.Fatal("empty name must keep all stores")
	}
	if s, _ := pick(all, "valantis"); len(s) != 1 || s[0].name != "valantis" {
		t.Fatalf("got %+v", s)
	}
	if _, err := pick(all, "nope"); err == nil || !strings.Contains(err.Error(), "personal, valantis") {
		t.Fatalf("got %v", err)
	}
}

func TestInterleaveByRank(t *testing.T) {
	hit := func(id string) map[string]any { return map[string]any{"id": id} }
	rs := []storeResult{
		{items: []map[string]any{hit("p1"), hit("p2"), hit("p3")}},
		{items: []map[string]any{hit("v1")}},
		{err: os.ErrNotExist},
	}
	var ids []string
	for _, h := range interleave(rs, 4) {
		ids = append(ids, h["id"].(string))
	}
	if got := strings.Join(ids, ","); got != "p1,v1,p2,p3" {
		t.Fatalf("got %s", got)
	}
	if n := len(interleave(rs, 2)); n != 2 {
		t.Fatalf("limit 2 gave %d", n)
	}
}
