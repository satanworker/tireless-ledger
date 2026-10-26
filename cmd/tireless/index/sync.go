package index

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SyncOpts struct {
	PiRoot    string
	CodexRoot string
	OmpRoot   string
	Host      string
	Cursor    *CursorDB
	Client    *Client
	Limit     int
	Poll      time.Duration
}

func guessContainer(path, source string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if source == "pi" {
		if i := strings.LastIndex(base, "_"); i >= 0 {
			return base[i+1:]
		}
	}
	parts := strings.Split(base, "-")
	if len(parts) >= 5 {
		return strings.Join(parts[len(parts)-5:], "-")
	}
	return base
}

func listJSONL(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func (o SyncOpts) scanFile(path, source string) (int, int, error) {
	cur := o.Cursor.Get(path)
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	if fi.Size() == cur.Offset {
		return 0, 0, nil
	}
	if fi.Size() < cur.Offset {
		cur.Offset = 0 // truncated/rotated
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(cur.Offset, io.SeekStart); err != nil {
		return 0, 0, err
	}
	st := &fileState{
		Source:    source,
		Host:      o.Host,
		Container: cur.Session,
		Project:   cur.Project,
	}
	if st.Container == "" {
		st.Container = guessContainer(path, source)
	}
	if st.Project == "" {
		st.Project = "unknown"
	}
	turns, n, err := ReadTurns(f, st)
	if err != nil {
		return 0, 0, err
	}
	full := true
	if o.Limit > 0 && len(turns) > o.Limit {
		turns = turns[:o.Limit]
		full = false
	}

	acc, skip := 0, 0
	for i := 0; i < len(turns); i += PostBatch {
		end := i + PostBatch
		if end > len(turns) {
			end = len(turns)
		}
		resp, err := o.Client.Ingest(turns[i:end])
		if err != nil {
			return acc, skip, err
		}
		acc += resp.Accepted
		skip += resp.Skipped
	}
	if full {
		o.Cursor.Set(path, FileCursor{Offset: cur.Offset + n, Session: st.Container, Project: st.Project})
	}
	return acc, skip, nil
}

func (o SyncOpts) Once() (accepted, skipped, files int, err error) {
	type file struct {
		path   string
		source string
	}
	var paths []file
	for _, src := range []file{
		{o.PiRoot, "pi"},
		{o.CodexRoot, "codex"},
		{o.OmpRoot, "omp"},
	} {
		ps, e := listJSONL(src.path)
		if e != nil {
			return 0, 0, 0, e
		}
		for _, p := range ps {
			paths = append(paths, file{p, src.source})
		}
	}
	for _, p := range paths {
		before := o.Cursor.Get(p.path)
		a, s, e := o.scanFile(p.path, p.source)
		if e != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", p.path, e)
			continue
		}
		accepted += a
		skipped += s
		files++
		if a+s > 0 {
			fmt.Fprintf(os.Stderr, "sync %s accepted=%d skipped=%d\n", p.path, a, s)
		}
		if o.Cursor.Get(p.path) != before {
			if err := o.Cursor.Save(); err != nil {
				return accepted, skipped, files, err
			}
		}
		if o.Limit > 0 {
			o.Limit -= a + s
			if o.Limit <= 0 {
				break
			}
		}
	}
	return accepted, skipped, files, nil
}

func (o SyncOpts) Watch() error {
	if o.Poll <= 0 {
		o.Poll = 3 * time.Second
	}
	for {
		a, s, n, err := o.Once()
		if err != nil {
			return err
		}
		if a+s > 0 {
			fmt.Fprintf(os.Stderr, "sync files=%d accepted=%d skipped=%d\n", n, a, s)
		}
		time.Sleep(o.Poll)
	}
}
