package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOnceSkipsUnchangedFileWithoutSaving(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(session, []byte("complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(session)
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cursor := &CursorDB{
		Path:  filepath.Join(blocker, "cursor.json"),
		Files: map[string]FileCursor{session: {Offset: info.Size()}},
	}

	_, _, files, err := (SyncOpts{PiRoot: root, Cursor: cursor}).Once()
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("files=%d", files)
	}
}
