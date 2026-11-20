//go:build cgo

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lancedb/lancedb-go/pkg/contracts"
)

// filteredIndexTable models the home-satan chunks table under a selective filter
// (project=grokbot-legal): the IVF-PQ search returns none of the matching rows, a flat
// scan and FTS return them, and Lance's native hybrid RRF fails on the empty vector side.
type filteredIndexTable struct {
	contracts.ITable
	rows []map[string]interface{}
}

func (f *filteredIndexTable) Select(_ context.Context, c contracts.QueryConfig) ([]map[string]interface{}, error) {
	var score string
	switch {
	case c.VectorSearch != nil && c.VectorSearch.FullTextQuery != "":
		return nil, errors.New(`Invalid input, expected column _distance not found in rank. found columns ["message_id", "_score", "_rowid"]`)
	case c.VectorSearch != nil && !c.VectorSearch.BypassVectorIndex:
		return nil, nil
	case c.VectorSearch != nil:
		score = "_distance"
	case c.FTSSearch != nil:
		score = "_score"
	case !strings.HasPrefix(c.Where, "_rowid IN"):
		return nil, fmt.Errorf("unexpected query %+v", c)
	}
	var out []map[string]interface{}
	for i, row := range f.rows {
		if score == "" && !strings.Contains(c.Where, fmt.Sprint(row["_rowid"])) {
			continue
		}
		got := map[string]interface{}{}
		for _, col := range c.Columns {
			got[col] = row[col]
		}
		if c.WithRowID {
			got["_rowid"] = row["_rowid"]
		}
		if score != "" {
			got[score] = float64(i + 1)
		}
		out = append(out, got)
	}
	return out, nil
}

func TestLanceFilteredSearchSurvivesEmptyIndexSide(t *testing.T) {
	table := &filteredIndexTable{}
	for i := range 5 {
		id := fmt.Sprintf("grok-box/grokbot/s/t%d", i)
		table.rows = append(table.rows, map[string]interface{}{
			"_rowid": float64(100 + i), "id": id + "#0", "message_id": id, "text": "tax question", "project": "legal",
		})
	}
	store := &cgoLanceStore{chunks: table, nprobes: 1}
	legal := eqFilters("project", "legal")
	for _, req := range []vectorSearchRequest{
		{Table: "chunks", Vector: []float32{1}, BM25: &bm25Query{Field: "text", Query: "tax"}, K: 50, Filter: legal, Collapse: 5},
		{Table: "chunks", Vector: []float32{1}, K: 50, Filter: legal, Collapse: 5},
	} {
		hits, err := store.Search(context.Background(), req)
		if err != nil {
			t.Fatalf("hybrid=%v: %v", req.BM25 != nil, err)
		}
		if len(hits) != 5 {
			t.Fatalf("hybrid=%v: %d hits, want the 5 legal chunks", req.BM25 != nil, len(hits))
		}
		for _, h := range hits {
			if h.Metadata["project"] != "legal" || h.Metadata["_rowid"] != nil {
				t.Fatalf("hit %s metadata %v", h.ID, h.Metadata)
			}
		}
	}
}
