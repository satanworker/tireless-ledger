package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/earendil-works/pi-memoryd/cmd/tireless/index"
)

// The agent skill ships inside the binary so `tireless init` can install it without Nix.
//
//go:embed skill/SKILL.md
var skillFS embed.FS

// store is one pi-memoryd deployment and the embedder whose model built its index.
type store struct{ name, server, embed string }

func storesPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "tireless", "stores")
	}
	return filepath.Join(home(), ".config", "tireless", "stores")
}

// loadStores reads "name server embed" lines; a missing file means no stores.
func loadStores(path string) ([]store, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []store
	for i, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("%s:%d: want \"name server embed\"", path, i+1)
		}
		out = append(out, store{f[0], f[1], f[2]})
	}
	return out, nil
}

// storeLines turns NAME=HOST pairs into stores file content: server HOST:8090, embedder HOST:8091.
func storeLines(pairs []string) (string, error) {
	if len(pairs) == 0 {
		return "", errors.New("usage: tireless init NAME=HOST [NAME=HOST ...]")
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, p := range pairs {
		name, host, ok := strings.Cut(p, "=")
		if !ok || name == "" || host == "" || strings.ContainsAny(p, " \t#") {
			return "", fmt.Errorf("bad store %q: want NAME=HOST", p)
		}
		if seen[name] {
			return "", fmt.Errorf("store %q given twice", name)
		}
		seen[name] = true
		fmt.Fprintf(&b, "%s http://%s:8090 http://%s:8091\n", name, host, host)
	}
	return b.String(), nil
}

// initCmd writes the stores file and installs the skill where Agent Skills harnesses look.
func initCmd(pairs []string) error {
	lines, err := storeLines(pairs)
	if err != nil {
		return err
	}
	path := storesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	skill, _ := skillFS.ReadFile("skill/SKILL.md")
	// ~/.agents/skills: omp, Codex, Pi; ~/.claude/skills: Claude Code.
	for _, dir := range []string{".agents/skills", ".claude/skills"} {
		dst := filepath.Join(home(), dir, "tireless-recall", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, skill, 0o644); err != nil {
			return fmt.Errorf("%s (managed by Nix? set the skill there instead): %w", dst, err)
		}
		fmt.Printf("wrote %s\n", dst)
	}
	return nil
}

// pick narrows to one named store; an empty name keeps all.
func pick(all []store, name string) ([]store, error) {
	if name == "" {
		return all, nil
	}
	var names []string
	for _, s := range all {
		if s.name == name {
			return []store{s}, nil
		}
		names = append(names, s.name)
	}
	return nil, fmt.Errorf("unknown store %q (configured: %s)", name, strings.Join(names, ", "))
}

type storeResult struct {
	st    store
	items []map[string]any
	err   error
}

// fanOut calls every store in parallel and labels each returned item with its store.
// key is the response's list field: "results" or "sessions".
func fanOut(stores []store, key string, call func(*index.Client) (json.RawMessage, error)) []storeResult {
	out := make([]storeResult, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].st = s
			raw, err := call(index.NewClient(s.server, s.embed, ""))
			if err != nil {
				out[i].err = err
				return
			}
			var body map[string][]map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				out[i].err = fmt.Errorf("decode %s: %w", s.server, err)
				return
			}
			out[i].items = body[key]
			if s.name != "" {
				for _, it := range out[i].items {
					it["store"] = s.name
				}
			}
		}()
	}
	wg.Wait()
	return out
}

// reportErrors prints failed stores to stderr and reports whether every store failed.
func reportErrors(rs []storeResult) (allFailed bool) {
	failed := 0
	for _, r := range rs {
		if r.err != nil {
			failed++
			name := r.st.name
			if name == "" {
				name = r.st.server
			}
			fmt.Fprintf(os.Stderr, "store %s: %v\n", name, r.err)
		}
	}
	return failed == len(rs)
}

// interleave takes hits round-robin by rank: each store's scores are rank-fusion values
// computed within that store, so they don't compare across stores.
func interleave(rs []storeResult, limit int) []map[string]any {
	var out []map[string]any
	for rank := 0; len(out) < limit; rank++ {
		took := false
		for _, r := range rs {
			if rank < len(r.items) && len(out) < limit {
				out = append(out, r.items[rank])
				took = true
			}
		}
		if !took {
			break
		}
	}
	return out
}

func concat(rs []storeResult) []map[string]any {
	var out []map[string]any
	for _, r := range rs {
		out = append(out, r.items...)
	}
	return out
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func when(ts any) string {
	n, ok := ts.(float64)
	if !ok || n <= 0 {
		return ""
	}
	return time.Unix(int64(n), 0).UTC().Format("2006-01-02 15:04")
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func join(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " ")
}

// printHits is the compact digest agents read: header, ids to follow up with, 500-char snippet.
func printHits(w io.Writer, hits []map[string]any, took time.Duration) {
	fmt.Fprintf(w, "%d hits in %dms\n", len(hits), took.Milliseconds())
	for i, h := range hits {
		m, _ := h["metadata"].(map[string]any)
		kind := str(m, "kind")
		if kind == "message" {
			kind = ""
		}
		fmt.Fprintf(w, "\n%s\n%s\n%s\n",
			join(fmt.Sprintf("%d.", i+1), str(h, "store"), str(m, "host")+"/"+str(m, "source"),
				"container="+or(str(m, "container_title"), str(m, "container_id")),
				"author="+or(str(m, "author_name"), str(m, "author_id")),
				prefix("project=", str(m, "project")), when(m["timestamp"]), prefix("kind=", kind)),
			join(prefix("store=", str(h, "store")), "container_id="+str(m, "container_id"), "ts="+str(m, "timestamp"), "id="+str(h, "id")),
			snippet(str(h, "text"), 500))
	}
}

func printSessions(w io.Writer, sessions []map[string]any) {
	fmt.Fprintf(w, "%d sessions\n", len(sessions))
	for i, s := range sessions {
		fmt.Fprintf(w, "\n%s\n%s\n%s\n",
			join(fmt.Sprintf("%d.", i+1), str(s, "store"), str(s, "host")+"/"+str(s, "source"),
				"container="+or(str(s, "container_title"), str(s, "container_id")),
				prefix("project=", str(s, "project")), when(s["first_ts"])+" → "+when(s["last_ts"]),
				"messages="+str(s, "messages")),
			join(prefix("store=", str(s, "store")), "container_id="+str(s, "container_id"), "first_ts="+str(s, "first_ts")),
			snippet(str(s, "first_prompt"), 300))
	}
}

func prefix(p, v string) string {
	if v == "" {
		return ""
	}
	return p + v
}
