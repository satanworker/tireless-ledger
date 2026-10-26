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
	mu       sync.RWMutex
	dims     int
	messages map[string]MemoryItem
	chunks   map[string]MemoryItem
}

func newMemStore(dims int) *memStore {
	return &memStore{dims: dims, messages: map[string]MemoryItem{}, chunks: map[string]MemoryItem{}}
}

func (m *memStore) upsert(messages, chunks []MemoryItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range messages {
		m.messages[it.ID] = it
	}
	for _, it := range chunks {
		m.chunks[it.ID] = it
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

func (m *memStore) byContainer(containerID, host, source string, kinds map[string]bool, afterTS int64, afterID string, limit int) []queryResult {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]queryResult, 0)
	for _, it := range m.messages {
		if it.Metadata.ContainerID != containerID || !kinds[it.Metadata.Kind] {
			continue
		}
		if host != "" && it.Metadata.Host != host {
			continue
		}
		if source != "" && it.Metadata.Source != source {
			continue
		}
		if !afterCursor(it.Metadata.Timestamp, it.ID, afterTS, afterID) {
			continue
		}
		out = append(out, queryResult{ID: it.ID, Score: float64(it.Metadata.Timestamp), Text: it.Text, Metadata: metadataMap(it.Metadata)})
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := metaTS(out[i]), metaTS(out[j])
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
	table := m.chunks
	if req.Table == "messages" {
		table = m.messages
	}
	hits := make([]scored, 0)
	for id, it := range table {
		if !memMatch(it, req.Filter) {
			continue
		}
		if !afterCursor(it.Metadata.Timestamp, id, req.AfterTS, req.AfterID) {
			continue
		}
		var score float64
		if req.BM25 != nil {
			score = bm25Score(req.BM25.Query, it.Text)
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
		out = append(out, queryResult{ID: h.id, Score: h.score, Text: h.item.Text, Metadata: metadataMap(h.item.Metadata)})
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
	if f.Eq == nil || !lanceStringFields[f.Eq.Field] {
		return true
	}
	want, _ := f.Eq.Value.(string)
	got, _ := metadataMap(it.Metadata)[f.Eq.Field].(string)
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
