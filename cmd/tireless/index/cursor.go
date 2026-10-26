package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type FileCursor struct {
	Offset  int64  `json:"offset"`
	Session string `json:"session,omitempty"`
	Project string `json:"project,omitempty"`
}

type CursorDB struct {
	mu    sync.Mutex
	Path  string                 `json:"-"`
	Files map[string]FileCursor `json:"files"`
}

func LoadCursor(path string) (*CursorDB, error) {
	db := &CursorDB{Path: path, Files: map[string]FileCursor{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, db); err != nil {
		return nil, err
	}
	if db.Files == nil {
		db.Files = map[string]FileCursor{}
	}
	db.Path = path
	return db, nil
}

func (c *CursorDB) Get(file string) FileCursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Files[file]
}

func (c *CursorDB) Set(file string, cur FileCursor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Files[file] = cur
}

func (c *CursorDB) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}
