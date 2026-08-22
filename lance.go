//go:build cgo

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/lancedb/lancedb-go/pkg/contracts"
	"github.com/lancedb/lancedb-go/pkg/lancedb"
)

const (
	lanceTableName        = "turns"
	lanceCompactFragments = 16
	lanceVectorIndexName  = "vector_ivf_flat"
	lanceVectorPartitions = uint32(64)
	lanceVectorNProbes    = 64
)

var lanceOutputColumns = []string{
	"id", "forward_content", "timestamp", "scope", "project_name", "file_path",
	"file_hash", "session_id", "host", "harness", "role",
}

type cgoLanceStore struct {
	conn    contracts.IConnection
	table   contracts.ITable
	dims    int
	nprobes int
}

type lanceFragmentCounter interface {
	FragmentCount(context.Context) (int, error)
}

func openLanceStore(ctx context.Context, cfg runtimeConfig) (lanceStore, error) {
	opts := map[string]string{
		contracts.StorageRegion:                    cfg.AWSRegion,
		contracts.StorageVirtualHostedStyleRequest: "false",
	}
	if key := os.Getenv("AWS_ACCESS_KEY_ID"); key != "" {
		opts[contracts.StorageAccessKeyID] = key
	}
	if secret := os.Getenv("AWS_SECRET_ACCESS_KEY"); secret != "" {
		opts[contracts.StorageSecretAccessKey] = secret
	}
	if token := os.Getenv("AWS_SESSION_TOKEN"); token != "" {
		opts[contracts.StorageSessionToken] = token
	}
	if cfg.S3Endpoint != "" {
		opts[contracts.StorageAWSEndpoint] = cfg.S3Endpoint
		if strings.HasPrefix(cfg.S3Endpoint, "http://") {
			opts["allow_http"] = "true"
		}
	}

	slog.Info("lance session cache", "index_mb", 256, "meta_mb", 64)
	conn, err := lancedb.Connect(ctx, cfg.StorageURL, &contracts.ConnectionOptions{StorageOptions: opts})
	if err != nil {
		return nil, err
	}
	nprobes := cfg.VectorNProbes
	if nprobes <= 0 {
		nprobes = lanceVectorNProbes
	}
	store := &cgoLanceStore{conn: conn, dims: cfg.Dimensions, nprobes: nprobes}
	table, err := conn.OpenTable(ctx, lanceTableName)
	if err != nil {
		table, err = store.createTable(ctx)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open or create %s: %w", lanceTableName, err)
	}
	store.table = table
	if _, ok := table.(lanceFragmentCounter); !ok {
		store.Close()
		return nil, fmt.Errorf("patched lancedb-go FragmentCount capability is unavailable")
	}
	if err := store.ensureIndexes(ctx); err != nil {
		store.Close()
		return nil, err
	}
	store.warm(ctx)
	return store, nil
}

func (s *cgoLanceStore) createTable(ctx context.Context) (contracts.ITable, error) {
	fields := []arrow.Field{
		{Name: "id", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "vector", Type: arrow.FixedSizeListOf(int32(s.dims), arrow.PrimitiveTypes.Float32), Nullable: true},
		{Name: "forward_content", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "scope", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "project_name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "file_path", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "file_hash", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "timestamp", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "session_id", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "host", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "harness", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "role", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema, err := lancedb.NewSchema(arrow.NewSchema(fields, nil))
	if err != nil {
		return nil, err
	}
	return s.conn.CreateTable(ctx, lanceTableName, schema)
}

func (s *cgoLanceStore) ensureIndexes(ctx context.Context) error {
	indexes, err := s.table.GetAllIndexes(ctx)
	if err != nil {
		return fmt.Errorf("list lance indexes: %w", err)
	}
	hasColumn := func(column string) bool {
		for _, index := range indexes {
			for _, got := range index.Columns {
				if got == column {
					return true
				}
			}
		}
		return false
	}
	if !hasColumn("forward_content") {
		if err := s.table.CreateIndexWithParams(ctx, []string{"forward_content"}, contracts.IndexTypeFts,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "forward_content_fts", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create forward_content FTS index: %w", err)
		}
	}
	if !hasColumn("session_id") {
		if err := s.table.CreateIndexWithParams(ctx, []string{"session_id"}, contracts.IndexTypeBTree,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "session_id_btree", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create session_id btree index: %w", err)
		}
	}
	return nil
}

func (s *cgoLanceStore) warm(ctx context.Context) {
	t0 := time.Now()
	one := 1
	if _, err := s.table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}, Limit: &one}); err != nil {
		slog.Warn("lance scan warmup failed", "err", err)
	}
	if _, err := s.table.Select(ctx, contracts.QueryConfig{
		Columns: []string{"id"}, Limit: &one,
		FTSSearch: &contracts.FTSSearch{Column: "forward_content", Query: "warmup"},
	}); err != nil {
		slog.Warn("lance FTS warmup failed", "err", err)
	}
	slog.Info("lance warmup complete", "elapsed", time.Since(t0))
}

func (s *cgoLanceStore) Close() error {
	var first error
	if s.table != nil {
		first = s.table.Close()
	}
	if s.conn != nil {
		if err := s.conn.Close(); first == nil {
			first = err
		}
	}
	return first
}

func (s *cgoLanceStore) Optimize(ctx context.Context) error {
	counter := s.table.(lanceFragmentCounter)
	before, err := counter.FragmentCount(ctx)
	if err != nil {
		return fmt.Errorf("count fragments before maintenance: %w", err)
	}
	compact, err := s.table.OptimizeWithAction(ctx, contracts.OptimizeAction{Kind: contracts.OptimizeCompact})
	if err != nil {
		return fmt.Errorf("compact lance fragments: %w", err)
	}
	if _, err := s.table.OptimizeWithAction(ctx, contracts.OptimizeAction{Kind: contracts.OptimizeIndex}); err != nil {
		return fmt.Errorf("refresh lance indexes: %w", err)
	}
	deleteUnverified := true
	if _, err := s.table.OptimizeWithAction(ctx, contracts.OptimizeAction{
		Kind: contracts.OptimizePrune,
		Prune: contracts.PruneParams{
			OlderThan:        time.Nanosecond,
			DeleteUnverified: &deleteUnverified,
		},
	}); err != nil {
		return fmt.Errorf("prune lance versions: %w", err)
	}
	after, err := counter.FragmentCount(ctx)
	if err != nil {
		return fmt.Errorf("count fragments after maintenance: %w", err)
	}
	slog.Info("lance maintenance complete", "fragments_before", before, "fragments_after", after, "compaction", compact)
	return nil
}

func (s *cgoLanceStore) CreateVectorIndex(ctx context.Context) error {
	indexes, err := s.table.GetAllIndexes(ctx)
	if err != nil {
		return fmt.Errorf("list lance indexes: %w", err)
	}
	for _, index := range indexes {
		for _, column := range index.Columns {
			if column == "vector" {
				slog.Info("vector index already exists", "name", index.Name, "type", index.IndexType)
				return nil
			}
		}
	}
	partitions := lanceVectorPartitions
	if err := s.table.CreateIndexWithParams(ctx, []string{"vector"}, contracts.IndexTypeIvfFlat,
		contracts.IndexParams{NumPartitions: &partitions, DistanceType: contracts.DistanceTypeL2},
		&contracts.CreateIndexOptions{Name: lanceVectorIndexName, WaitTimeout: 10 * time.Minute}); err != nil {
		return fmt.Errorf("create IVF-Flat vector index: %w", err)
	}
	slog.Info("created IVF-Flat vector index", "name", lanceVectorIndexName, "partitions", partitions, "nprobes", s.nprobes)
	return nil
}

func (s *cgoLanceStore) DropVectorIndex(ctx context.Context) error {
	indexes, err := s.table.GetAllIndexes(ctx)
	if err != nil {
		return fmt.Errorf("list lance indexes: %w", err)
	}
	for _, index := range indexes {
		if index.Name == lanceVectorIndexName {
			if err := s.table.DropIndex(ctx, index.Name); err != nil {
				return fmt.Errorf("drop IVF-Flat vector index: %w", err)
			}
			slog.Info("dropped IVF-Flat vector index", "name", index.Name)
			return nil
		}
	}
	slog.Info("IVF-Flat vector index is absent", "name", lanceVectorIndexName)
	return nil
}

func (s *cgoLanceStore) AuditDuplicates(ctx context.Context) (duplicateAudit, error) {
	rows, err := s.table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}})
	if err != nil {
		return duplicateAudit{}, fmt.Errorf("scan lance IDs: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[stringValue(row["id"])]++
	}
	duplicates := make([]string, 0)
	for id, count := range counts {
		if count > 1 {
			duplicates = append(duplicates, id)
		}
	}
	sort.Strings(duplicates)
	return duplicateAudit{Rows: len(rows), UniqueIDs: len(counts), DuplicateIDs: duplicates}, nil
}

func (s *cgoLanceStore) DeleteHost(ctx context.Context, host string) (int, error) {
	filter := "host = " + lanceQuote(host)
	rows, err := s.table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}, Where: filter})
	if err != nil {
		return 0, fmt.Errorf("select host rows: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := s.table.Delete(ctx, filter); err != nil {
		return 0, fmt.Errorf("delete host rows: %w", err)
	}
	return len(rows), nil
}

func (s *cgoLanceStore) Upsert(ctx context.Context, items []MemoryItem) error {
	counter := s.table.(lanceFragmentCounter)
	_, err := counter.FragmentCount(ctx)
	if err != nil {
		return fmt.Errorf("count fragments before write: %w", err)
	}
	rec := lanceRecord(items, s.dims)
	defer rec.Release()
	if _, err := s.table.MergeInsert([]string{"id"}).WhenMatchedUpdateAll(nil).WhenNotMatchedInsertAll().Execute(ctx, []arrow.Record{rec}); err != nil {
		return fmt.Errorf("merge insert: %w", err)
	}
	fragments, err := counter.FragmentCount(ctx)
	if err != nil {
		slog.Warn("lance fragment count failed; write is persisted", "err", err)
		return nil
	}
	if fragments < lanceCompactFragments {
		return nil
	}
	stats, err := s.table.OptimizeWithAction(ctx, contracts.OptimizeAction{Kind: contracts.OptimizeCompact})
	if err != nil {
		slog.Warn("lance compaction failed; write is persisted", "fragments", fragments, "err", err)
		return nil
	}
	if _, err := s.table.OptimizeWithAction(ctx, contracts.OptimizeAction{Kind: contracts.OptimizeIndex}); err != nil {
		slog.Warn("lance index refresh failed; write is persisted", "err", err)
		return nil
	}
	deleteUnverified := true
	if _, err := s.table.OptimizeWithAction(ctx, contracts.OptimizeAction{
		Kind:  contracts.OptimizePrune,
		Prune: contracts.PruneParams{OlderThan: 0, DeleteUnverified: &deleteUnverified},
	}); err != nil {
		slog.Warn("lance prune failed; write is persisted", "err", err)
		return nil
	}
	slog.Info("compacted lance fragments", "before", fragments, "stats", stats)
	return nil
}

func lanceRecord(items []MemoryItem, dims int) arrow.Record {
	pool := memory.NewGoAllocator()
	id := array.NewStringBuilder(pool)
	vector := array.NewFixedSizeListBuilder(pool, int32(dims), arrow.PrimitiveTypes.Float32)
	vectorValues := vector.ValueBuilder().(*array.Float32Builder)
	content := array.NewStringBuilder(pool)
	scope := array.NewStringBuilder(pool)
	project := array.NewStringBuilder(pool)
	path := array.NewStringBuilder(pool)
	hash := array.NewStringBuilder(pool)
	timestamp := array.NewInt64Builder(pool)
	session := array.NewStringBuilder(pool)
	host := array.NewStringBuilder(pool)
	harness := array.NewStringBuilder(pool)
	role := array.NewStringBuilder(pool)
	builders := []array.Builder{id, vector, content, scope, project, path, hash, timestamp, session, host, harness, role}
	defer func() {
		for _, builder := range builders {
			builder.Release()
		}
	}()
	for _, item := range items {
		id.Append(safeID(item.ID))
		vector.Append(true)
		vectorValues.AppendValues(item.Vector, nil)
		content.Append(item.ForwardContent)
		scope.Append(string(item.Metadata.Scope))
		project.Append(item.Metadata.ProjectName)
		path.Append(item.Metadata.FilePath)
		hash.Append(item.Metadata.FileHash)
		timestamp.Append(item.Metadata.Timestamp)
		session.Append(item.Metadata.SessionID)
		host.Append(item.Metadata.Host)
		harness.Append(item.Metadata.Harness)
		role.Append(item.Metadata.Role)
	}
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "vector", Type: arrow.FixedSizeListOf(int32(dims), arrow.PrimitiveTypes.Float32), Nullable: true},
		{Name: "forward_content", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "scope", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "project_name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "file_path", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "file_hash", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "timestamp", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "session_id", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "host", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "harness", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "role", Type: arrow.BinaryTypes.String, Nullable: true},
	}, nil)
	arrays := []arrow.Array{
		id.NewArray(), vector.NewArray(), content.NewArray(), scope.NewArray(), project.NewArray(), path.NewArray(),
		hash.NewArray(), timestamp.NewArray(), session.NewArray(), host.NewArray(), harness.NewArray(), role.NewArray(),
	}
	record := array.NewRecord(schema, arrays, int64(len(items)))
	for _, values := range arrays {
		values.Release()
	}
	return record
}

func (s *cgoLanceStore) Search(ctx context.Context, req vectorSearchRequest) ([]queryResult, error) {
	limit := req.K
	if limit <= 0 {
		limit = 10
	}
	where := lanceCombineSQL(lanceFilterSQL(req.Filter), lanceCursorSQL(req.AfterTS, req.AfterID))
	config := contracts.QueryConfig{Columns: lanceOutputColumns, Where: where}
	hasVector := len(req.Vector) > 0
	hasText := req.BM25 != nil && strings.TrimSpace(req.BM25.Query) != ""
	switch {
	case hasVector:
		config.Limit = &limit
		nprobes := s.nprobes
		config.VectorSearch = &contracts.VectorSearch{Column: "vector", Vector: req.Vector, K: limit, Nprobes: &nprobes}
		if hasText {
			if req.BM25.Field != "" && req.BM25.Field != "forward_content" {
				return nil, fmt.Errorf("unsupported BM25 field %q", req.BM25.Field)
			}
			config.VectorSearch.FullTextQuery = req.BM25.Query
			config.VectorSearch.FullTextColumn = "forward_content"
			config.Reranker = &contracts.RerankerConfig{Kind: contracts.RerankerRRF, RRFK: 60, Norm: contracts.NormalizeRank}
		}
	case hasText:
		config.Limit = &limit
		if req.BM25.Field != "" && req.BM25.Field != "forward_content" {
			return nil, fmt.Errorf("unsupported BM25 field %q", req.BM25.Field)
		}
		config.FTSSearch = &contracts.FTSSearch{Column: "forward_content", Query: req.BM25.Query}
	}
	rows, err := s.table.Select(ctx, config)
	if err != nil {
		return nil, err
	}
	results := make([]queryResult, 0, len(rows))
	for _, row := range rows {
		id := stringValue(row["id"])
		content := stringValue(row["forward_content"])
		score := numberValue(row["_relevance_score"])
		if score == 0 {
			score = numberValue(row["_score"])
		}
		if score == 0 {
			distance := numberValue(row["_distance"])
			if hasVector {
				score = 1 / (1 + distance)
			} else {
				score = numberValue(row["timestamp"])
			}
		}
		delete(row, "id")
		delete(row, "forward_content")
		delete(row, "_relevance_score")
		delete(row, "_score")
		delete(row, "_distance")
		results = append(results, queryResult{ID: id, Score: score, ForwardContent: content, Metadata: row})
	}
	if !hasVector && !hasText {
		sort.Slice(results, func(i, j int) bool {
			ti, tj := metaTS(results[i]), metaTS(results[j])
			if ti == tj {
				return results[i].ID < results[j].ID
			}
			return ti < tj
		})
		if len(results) > limit {
			results = results[:limit]
		}
	}
	return results, nil
}

func stringValue(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func numberValue(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}
