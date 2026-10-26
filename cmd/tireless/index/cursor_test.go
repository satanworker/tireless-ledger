package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorAdvancesOnlyCompleteLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	line := `{"type":"session","id":"s1","cwd":"/tmp/proj"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-07-14T07:53:36Z","message":{"role":"user","content":[{"type":"text","text":"hello there friend"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(line+"{\"type\":\"message\""), 0o600); err != nil {
		t.Fatal(err)
	}
	cur := &CursorDB{Files: map[string]FileCursor{}}
	cli := NewClient("", "", "mac")
	cli.DryRun = true
	opts := SyncOpts{PiRoot: dir, Host: "mac", Cursor: cur, Client: cli}
	a, s, n, err := opts.Once()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || a+s != 1 {
		t.Fatalf("files=%d acc=%d skip=%d", n, a, s)
	}
	got := cur.Get(p)
	if got.Offset != int64(len(line)) {
		t.Fatalf("offset=%d want=%d", got.Offset, len(line))
	}

	// finish the partial line + another turn
	more := `{"type":"message","id":"a1","timestamp":"2026-07-14T07:53:40Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back at you now"}]}}` + "\n"
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(more), got.Offset); err != nil {
		t.Fatal(err)
	}
	f.Close()
	a, s, _, err = opts.Once()
	if err != nil {
		t.Fatal(err)
	}
	if a+s != 1 {
		t.Fatalf("second pass acc+skip=%d", a+s)
	}
	if cur.Get(p).Offset != int64(len(line)+len(more)) {
		t.Fatalf("offset after append=%d", cur.Get(p).Offset)
	}
	if !strings.HasPrefix(more, "{") {
		t.Fatal("sanity")
	}
}
