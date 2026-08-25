package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// memStore is the pure-Go test store for memory://.
// Enough for Mac smoke tests: L2 ANN + bag-of-words BM25-ish.
type memStore struct {
	mu    sync.RWMutex
	dims  int
	items map[string]MemoryItem
}

func newMemStore(dims int) *memStore {
	return &memStore{dims: dims, items: map[string]MemoryItem{}}
}

func (m *memStore) upsert(items []MemoryItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		m.items[it.ID] = it
	}
}

func afterCursor(ts int64, id string, afterTS int64, afterID string) bool {
	if afterTS <= 0 && afterID == "" {
		return true
	}
	if afterTS <= 0 {
		return id > afterID
	}
	if ts > afterTS {
		return true
	}
	return ts == afterTS && id > afterID
}

func (m *memStore) bySession(sessionID, host, harness string, afterTS int64, afterID string, limit int) []queryResult {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]queryResult, 0)
	for _, it := range m.items {
		if it.Metadata.SessionID != sessionID {
			continue
		}
		if it.Metadata.RecordKind != "" && it.Metadata.RecordKind != "message" {
			continue
		}
		if host != "" && it.Metadata.Host != host {
			continue
		}
		if harness != "" && it.Metadata.Harness != harness {
			continue
		}
		if !afterCursor(it.Metadata.Timestamp, it.ID, afterTS, afterID) {
			continue
		}
		out = append(out, queryResult{
			ID:             it.ID,
			Score:          float64(it.Metadata.Timestamp),
			ForwardContent: it.ForwardContent,
			Metadata: map[string]interface{}{
				"scope":        string(it.Metadata.Scope),
				"project_name": it.Metadata.ProjectName,
				"file_path":    it.Metadata.FilePath,
				"file_hash":    it.Metadata.FileHash,
				"timestamp":    it.Metadata.Timestamp,
				"session_id":   it.Metadata.SessionID,
				"host":         it.Metadata.Host,
				"harness":      it.Metadata.Harness,
				"role":         it.Metadata.Role,
				"record_kind":  it.Metadata.RecordKind,
				"parent_id":    it.Metadata.ParentID,
				"chunk_index":  it.Metadata.ChunkIndex,
				"chunk_count":  it.Metadata.ChunkCount,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ti, _ := out[i].Metadata["timestamp"].(int64)
		tj, _ := out[j].Metadata["timestamp"].(int64)
		if ti == tj {
			return out[i].ID < out[j].ID
		}
		return ti < tj
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *memStore) search(req vectorSearchRequest) []queryResult {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type scored struct {
		id    string
		score float64
		item  MemoryItem
	}
	hits := make([]scored, 0)
	for id, it := range m.items {
		if !memMatch(it, req.Filter) {
			continue
		}
		if !afterCursor(it.Metadata.Timestamp, id, req.AfterTS, req.AfterID) {
			continue
		}
		var score float64
		if req.BM25 != nil {
			score = bm25Score(req.BM25.Query, it.ForwardContent)
		} else if len(req.Vector) > 0 {
			score = l2(req.Vector, it.Vector)
		} else {
			score = float64(it.Metadata.Timestamp)
		}
		hits = append(hits, scored{id: id, score: score, item: it})
	}
	sort.Slice(hits, func(i, j int) bool {
		if req.BM25 != nil {
			if hits[i].score == hits[j].score {
				return hits[i].id < hits[j].id
			}
			return hits[i].score > hits[j].score
		}
		if len(req.Vector) == 0 {
			ti, tj := hits[i].item.Metadata.Timestamp, hits[j].item.Metadata.Timestamp
			if ti == tj {
				return hits[i].id < hits[j].id
			}
			return ti < tj
		}
		if hits[i].score == hits[j].score {
			return hits[i].id < hits[j].id
		}
		return hits[i].score < hits[j].score
	})
	if req.K > 0 && len(hits) > req.K {
		hits = hits[:req.K]
	}
	out := make([]queryResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, queryResult{
			ID:             h.id,
			Score:          h.score,
			ForwardContent: h.item.ForwardContent,
			Metadata: map[string]interface{}{
				"scope":        string(h.item.Metadata.Scope),
				"project_name": h.item.Metadata.ProjectName,
				"file_path":    h.item.Metadata.FilePath,
				"file_hash":    h.item.Metadata.FileHash,
				"timestamp":    h.item.Metadata.Timestamp,
				"session_id":   h.item.Metadata.SessionID,
				"host":         h.item.Metadata.Host,
				"harness":      h.item.Metadata.Harness,
				"role":         h.item.Metadata.Role,
				"record_kind":  h.item.Metadata.RecordKind,
				"parent_id":    h.item.Metadata.ParentID,
				"chunk_index":  h.item.Metadata.ChunkIndex,
				"chunk_count":  h.item.Metadata.ChunkCount,
			},
		})
	}
	return out
}

func memMatch(it MemoryItem, f *vectorFilter) bool {
	if f == nil {
		return true
	}
	if f.And != nil {
		for i := range f.And {
			if !memMatch(it, &f.And[i]) {
				return false
			}
		}
		return true
	}
	if f.Eq == nil {
		return true
	}
	want, _ := f.Eq.Value.(string)
	var got string
	switch f.Eq.Field {
	case "scope":
		got = string(it.Metadata.Scope)
	case "project_name":
		got = it.Metadata.ProjectName
	case "session_id":
		got = it.Metadata.SessionID
	case "host":
		got = it.Metadata.Host
	case "harness":
		got = it.Metadata.Harness
	case "role":
		got = it.Metadata.Role
	case "file_path":
		got = it.Metadata.FilePath
	case "record_kind":
		got = it.Metadata.RecordKind
	default:
		return true
	}
	return got == want
}

func l2(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float64
	for i := 0; i < n; i++ {
		d := float64(a[i] - b[i])
		s += d * d
	}
	return math.Sqrt(s)
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func bm25Score(query, doc string) float64 {
	// Lightweight term overlap is sufficient for memory:// tests.
	q := tokenize(query)
	if len(q) == 0 {
		return 0
	}
	df := map[string]int{}
	for _, t := range tokenize(doc) {
		df[t]++
	}
	var score float64
	for _, t := range q {
		if df[t] > 0 {
			score += 1 + math.Log(1+float64(df[t]))
		}
	}
	return score
}
