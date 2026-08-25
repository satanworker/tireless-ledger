//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	lanceChunksTable      = "chunks"
	lanceMessagesTable    = "messages"
	lanceVectorIndexName  = "vector_ivf_flat"
	lanceVectorPartitions = uint32(64)
	lanceVectorNProbes    = 64
)

var lanceOutputColumns = []string{
	"id", "forward_content", "timestamp", "scope", "project_name", "file_path",
	"file_hash", "session_id", "host", "harness", "role", "record_kind", "parent_id", "chunk_index", "chunk_count",
}

type cgoLanceStore struct {
	conn      contracts.IConnection
	legacy    contracts.ITable
	chunks    contracts.ITable
	messages  contracts.ITable
	dims      int
	nprobes   int
	exact     bool
	split     bool
	dualWrite bool
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
	store := &cgoLanceStore{conn: conn, dims: cfg.Dimensions, nprobes: nprobes, exact: cfg.ExactVectorSearch, split: cfg.SplitTables, dualWrite: cfg.DualWriteSplit || cfg.MigrateSplit}
	if !store.split || store.dualWrite {
		store.legacy, err = store.openOrCreateTable(ctx, lanceTableName)
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	if store.split || store.dualWrite {
		store.chunks, err = store.openOrCreateTable(ctx, lanceChunksTable)
		if err == nil {
			store.messages, err = store.openOrCreateTable(ctx, lanceMessagesTable)
		}
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	for name, table := range store.activeTables() {
		if _, ok := table.(lanceFragmentCounter); !ok {
			store.Close()
			return nil, fmt.Errorf("patched lancedb-go FragmentCount capability is unavailable for %s", name)
		}
	}
	if err := store.ensureIndexes(ctx); err != nil {
		store.Close()
		return nil, err
	}
	store.warm(ctx)
	return store, nil
}

func (s *cgoLanceStore) openOrCreateTable(ctx context.Context, name string) (contracts.ITable, error) {
	table, err := s.conn.OpenTable(ctx, name)
	if err == nil {
		return table, nil
	}
	return s.createTable(ctx, name)
}

func (s *cgoLanceStore) createTable(ctx context.Context, name string) (contracts.ITable, error) {
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
		{Name: "record_kind", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "parent_id", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "chunk_index", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "chunk_count", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema, err := lancedb.NewSchema(arrow.NewSchema(fields, nil))
	if err != nil {
		return nil, err
	}
	return s.conn.CreateTable(ctx, name, schema)
}

func (s *cgoLanceStore) ensureIndexes(ctx context.Context) error {
	if s.split || s.dualWrite {
		if err := s.ensureTableIndexes(ctx, s.chunks, "chunks"); err != nil {
			return err
		}
		if err := s.ensureTableIndexes(ctx, s.messages, "messages"); err != nil {
			return err
		}
	}
	if !s.split || s.dualWrite {
		return s.ensureTableIndexes(ctx, s.legacy, "legacy")
	}
	return nil
}

func (s *cgoLanceStore) ensureTableIndexes(ctx context.Context, table contracts.ITable, kind string) error {
	indexes, err := table.GetAllIndexes(ctx)
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
	if kind != "messages" && !hasColumn("forward_content") {
		if err := table.CreateIndexWithParams(ctx, []string{"forward_content"}, contracts.IndexTypeFts,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "forward_content_fts", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create forward_content FTS index: %w", err)
		}
	}
	if kind != "chunks" && !hasColumn("session_id") {
		if err := table.CreateIndexWithParams(ctx, []string{"session_id"}, contracts.IndexTypeBTree,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "session_id_btree", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create session_id btree index: %w", err)
		}
	}
	if !hasColumn("id") {
		if err := table.CreateIndexWithParams(ctx, []string{"id"}, contracts.IndexTypeBTree,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "id_btree", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create %s id index: %w", kind, err)
		}
	}
	if kind == "legacy" && !hasColumn("record_kind") {
		if err := table.CreateIndexWithParams(ctx, []string{"record_kind"}, contracts.IndexTypeBitmap,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "record_kind_bitmap", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create record_kind index: %w", err)
		}
	}
	return nil
}

func (s *cgoLanceStore) warm(ctx context.Context) {
	t0 := time.Now()
	one := 1
	table := s.searchTable("chunks")
	if _, err := table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}, Limit: &one}); err != nil {
		slog.Warn("lance scan warmup failed", "err", err)
	}
	if _, err := table.Select(ctx, contracts.QueryConfig{
		Columns: []string{"id"}, Limit: &one,
		FTSSearch: &contracts.FTSSearch{Column: "forward_content", Query: "warmup"},
	}); err != nil {
		slog.Warn("lance FTS warmup failed", "err", err)
	}
	slog.Info("lance warmup complete", "elapsed", time.Since(t0))
}

func (s *cgoLanceStore) Close() error {
	var first error
	seen := map[contracts.ITable]bool{}
	for _, table := range []contracts.ITable{s.legacy, s.chunks, s.messages} {
		if table == nil || seen[table] {
			continue
		}
		seen[table] = true
		if err := table.Close(); first == nil {
			first = err
		}
	}
	if s.conn != nil {
		if err := s.conn.Close(); first == nil {
			first = err
		}
	}
	return first
}

func (s *cgoLanceStore) Optimize(ctx context.Context) error {
	for name, table := range s.activeTables() {
		if err := s.optimizeTable(ctx, name, table); err != nil {
			return err
		}
	}
	return nil
}

func (s *cgoLanceStore) optimizeTable(ctx context.Context, name string, table contracts.ITable) error {
	counter := table.(lanceFragmentCounter)
	before, err := counter.FragmentCount(ctx)
	if err != nil {
		return fmt.Errorf("count fragments before maintenance: %w", err)
	}
	compact, err := table.OptimizeWithAction(ctx, contracts.OptimizeAction{Kind: contracts.OptimizeCompact})
	if err != nil {
		return fmt.Errorf("compact lance fragments: %w", err)
	}
	if _, err := table.OptimizeWithAction(ctx, contracts.OptimizeAction{Kind: contracts.OptimizeIndex}); err != nil {
		return fmt.Errorf("refresh lance indexes: %w", err)
	}
	deleteUnverified := true
	if _, err := table.OptimizeWithAction(ctx, contracts.OptimizeAction{
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
	slog.Info("lance maintenance complete", "table", name, "fragments_before", before, "fragments_after", after, "compaction", compact)
	return nil
}

func (s *cgoLanceStore) CreateVectorIndex(ctx context.Context) error {
	table := s.searchTable("chunks")
	indexes, err := table.GetAllIndexes(ctx)
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
	if err := table.CreateIndexWithParams(ctx, []string{"vector"}, contracts.IndexTypeIvfFlat,
		contracts.IndexParams{NumPartitions: &partitions, DistanceType: contracts.DistanceTypeL2},
		&contracts.CreateIndexOptions{Name: lanceVectorIndexName, WaitTimeout: 10 * time.Minute}); err != nil {
		return fmt.Errorf("create IVF-Flat vector index: %w", err)
	}
	slog.Info("created IVF-Flat vector index", "name", lanceVectorIndexName, "partitions", partitions, "nprobes", s.nprobes)
	return nil
}

func (s *cgoLanceStore) DropVectorIndex(ctx context.Context) error {
	table := s.searchTable("chunks")
	indexes, err := table.GetAllIndexes(ctx)
	if err != nil {
		return fmt.Errorf("list lance indexes: %w", err)
	}
	for _, index := range indexes {
		if index.Name == lanceVectorIndexName {
			if err := table.DropIndex(ctx, index.Name); err != nil {
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
	counts := map[string]int{}
	rowsTotal := 0
	for name, table := range s.activeTables() {
		rows, err := table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}})
		if err != nil {
			return duplicateAudit{}, fmt.Errorf("scan %s IDs: %w", name, err)
		}
		rowsTotal += len(rows)
		for _, row := range rows {
			key := name + ":" + stringValue(row["id"])
			counts[key]++
		}
	}
	duplicates := make([]string, 0)
	for id, count := range counts {
		if count > 1 {
			duplicates = append(duplicates, id)
		}
	}
	sort.Strings(duplicates)
	return duplicateAudit{Rows: rowsTotal, UniqueIDs: len(counts), DuplicateIDs: duplicates}, nil
}

func (s *cgoLanceStore) DeleteHost(ctx context.Context, host string) (int, error) {
	filter := "host = " + lanceQuote(host)
	total := 0
	for name, table := range s.activeTables() {
		rows, err := table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}, Where: filter})
		if err != nil {
			return total, fmt.Errorf("select %s host rows: %w", name, err)
		}
		if len(rows) == 0 {
			continue
		}
		if err := table.Delete(ctx, filter); err != nil {
			return total, fmt.Errorf("delete %s host rows: %w", name, err)
		}
		total += len(rows)
	}
	return total, nil
}

func (s *cgoLanceStore) Upsert(ctx context.Context, items []MemoryItem) error {
	if !s.split || s.dualWrite {
		if err := s.upsertTable(ctx, "legacy", s.legacy, items); err != nil {
			return err
		}
	}
	if s.split || s.dualWrite {
		var chunks, messages []MemoryItem
		for _, item := range items {
			switch item.Metadata.RecordKind {
			case "chunk":
				chunks = append(chunks, item)
			case "message", "":
				messages = append(messages, item)
			}
		}
		if err := s.upsertTable(ctx, "chunks", s.chunks, chunks); err != nil {
			return err
		}
		if err := s.upsertTable(ctx, "messages", s.messages, messages); err != nil {
			return err
		}
	}
	return nil
}

func (s *cgoLanceStore) upsertTable(ctx context.Context, name string, table contracts.ITable, items []MemoryItem) error {
	if len(items) == 0 {
		return nil
	}
	rec := lanceRecord(items, s.dims)
	defer rec.Release()
	if _, err := table.MergeInsert([]string{"id"}).WhenMatchedUpdateAll(nil).WhenNotMatchedInsertAll().Execute(ctx, []arrow.Record{rec}); err != nil {
		return fmt.Errorf("merge insert %s: %w", name, err)
	}
	return nil
}

func (s *cgoLanceStore) activeTables() map[string]contracts.ITable {
	if s.split {
		return map[string]contracts.ITable{"chunks": s.chunks, "messages": s.messages}
	}
	if s.dualWrite {
		return map[string]contracts.ITable{"legacy": s.legacy, "chunks": s.chunks, "messages": s.messages}
	}
	return map[string]contracts.ITable{"legacy": s.legacy}
}

func (s *cgoLanceStore) searchTable(kind string) contracts.ITable {
	if s.split {
		if kind == "messages" {
			return s.messages
		}
		return s.chunks
	}
	return s.legacy
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
	kind := array.NewStringBuilder(pool)
	parent := array.NewStringBuilder(pool)
	chunkIndex := array.NewInt64Builder(pool)
	chunkCount := array.NewInt64Builder(pool)
	builders := []array.Builder{id, vector, content, scope, project, path, hash, timestamp, session, host, harness, role, kind, parent, chunkIndex, chunkCount}
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
		kind.Append(item.Metadata.RecordKind)
		parent.Append(item.Metadata.ParentID)
		chunkIndex.Append(item.Metadata.ChunkIndex)
		chunkCount.Append(item.Metadata.ChunkCount)
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
		{Name: "record_kind", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "parent_id", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "chunk_index", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "chunk_count", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}, nil)
	arrays := []arrow.Array{
		id.NewArray(), vector.NewArray(), content.NewArray(), scope.NewArray(), project.NewArray(), path.NewArray(),
		hash.NewArray(), timestamp.NewArray(), session.NewArray(), host.NewArray(), harness.NewArray(), role.NewArray(),
		kind.NewArray(), parent.NewArray(), chunkIndex.NewArray(), chunkCount.NewArray(),
	}
	record := array.NewRecord(schema, arrays, int64(len(items)))
	for _, values := range arrays {
		values.Release()
	}
	return record
}

func (s *cgoLanceStore) Search(ctx context.Context, req vectorSearchRequest) ([]queryResult, error) {
	table := s.searchTable(req.Table)
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
		config.VectorSearch = &contracts.VectorSearch{
			Column: "vector", Vector: req.Vector, K: limit, Nprobes: &nprobes,
			BypassVectorIndex: s.exact,
		}
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
	rows, err := table.Select(ctx, config)
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

type splitMigrationState struct {
	SourceVersion  uint64 `json:"source_version,omitempty"`
	SourceRows     int64  `json:"source_rows,omitempty"`
	MessagesOffset int64  `json:"messages_offset"`
	ChunksOffset   int64  `json:"chunks_offset"`
	Complete       bool   `json:"complete"`
}

func (s *cgoLanceStore) MigrateSplit(ctx context.Context, batchSize int, checkpointPath string) error {
	if s.chunks == nil || s.messages == nil {
		return fmt.Errorf("split tables are not open; enable PI_MEMORYD_DUAL_WRITE_SPLIT")
	}
	state := splitMigrationState{}
	if b, err := os.ReadFile(checkpointPath); err == nil {
		if err := json.Unmarshal(b, &state); err != nil {
			return fmt.Errorf("decode split migration checkpoint: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read split migration checkpoint: %w", err)
	}
	alreadyComplete := state.Complete
	if batchSize <= 0 {
		batchSize = 1024
	}
	if state.SourceVersion == 0 {
		version, err := s.legacy.Version(ctx)
		if err != nil {
			return fmt.Errorf("get legacy migration source version: %w", err)
		}
		state.SourceVersion = uint64(version)
	}
	timeTravel, ok := s.legacy.(contracts.ITableTimeTravel)
	if !ok {
		return fmt.Errorf("legacy table snapshot capability is unavailable")
	}
	if err := timeTravel.Checkout(ctx, state.SourceVersion); err != nil {
		return fmt.Errorf("pin legacy migration source version %d: %w", state.SourceVersion, err)
	}
	if state.SourceRows == 0 {
		rows, err := s.legacy.Count(ctx)
		if err != nil {
			return fmt.Errorf("count legacy migration source: %w", err)
		}
		state.SourceRows = rows
		if err := saveSplitMigrationState(checkpointPath, state); err != nil {
			return err
		}
	}
	if alreadyComplete {
		slog.Info("split migration bulk copy already complete; verifying IDs", "source_version", state.SourceVersion, "source_rows", state.SourceRows, "checkpoint", checkpointPath)
	} else {
		started := time.Now()
		initialRows := state.MessagesOffset + state.ChunksOffset
		slog.Info("split migration starting", "source_version", state.SourceVersion, "source_rows", state.SourceRows, "completed_rows", initialRows, "batch_size", batchSize, "checkpoint", checkpointPath)
		for _, part := range []struct {
			name   string
			filter string
			table  contracts.ITable
			offset *int64
		}{
			{name: "messages", filter: "record_kind = 'message'", table: s.messages, offset: &state.MessagesOffset},
			{name: "chunks", filter: "record_kind = 'chunk'", table: s.chunks, offset: &state.ChunksOffset},
		} {
			for {
				record, err := s.legacy.Query().Filter(part.filter).Limit(batchSize).Offset(int(*part.offset)).Execute(ctx)
				if err != nil {
					return fmt.Errorf("read %s migration batch at %d: %w", part.name, *part.offset, err)
				}
				if record == nil || record.NumRows() == 0 {
					if record != nil {
						record.Release()
					}
					break
				}
				rows := record.NumRows()
				_, err = part.table.MergeInsert([]string{"id"}).WhenMatchedUpdateAll(nil).WhenNotMatchedInsertAll().Execute(ctx, []arrow.Record{record})
				record.Release()
				if err != nil {
					return fmt.Errorf("write %s migration batch at %d: %w", part.name, *part.offset, err)
				}
				*part.offset += rows
				if err := saveSplitMigrationState(checkpointPath, state); err != nil {
					return err
				}
				done := state.MessagesOffset + state.ChunksOffset
				newRows := done - initialRows
				elapsed := time.Since(started)
				rate := float64(0)
				eta := time.Duration(0)
				if elapsed > 0 && newRows > 0 {
					rate = float64(newRows) / elapsed.Seconds()
					remaining := state.SourceRows - done
					if remaining > 0 {
						eta = time.Duration(float64(remaining)/rate) * time.Second
					}
				}
				percent := float64(0)
				if state.SourceRows > 0 {
					percent = 100 * float64(done) / float64(state.SourceRows)
				}
				slog.Info("split migration batch complete", "table", part.name, "rows", rows, "table_offset", *part.offset, "completed_rows", done, "source_rows", state.SourceRows, "percent", fmt.Sprintf("%.1f", percent), "rows_per_second", fmt.Sprintf("%.1f", rate), "eta", eta)
			}
		}
	}
	if err := s.reconcileSplitIDs(ctx, batchSize); err != nil {
		return err
	}
	state.Complete = true
	if err := saveSplitMigrationState(checkpointPath, state); err != nil {
		return err
	}
	messageRows, err := s.messages.Count(ctx)
	if err != nil {
		return fmt.Errorf("count migrated messages: %w", err)
	}
	chunkRows, err := s.chunks.Count(ctx)
	if err != nil {
		return fmt.Errorf("count migrated chunks: %w", err)
	}
	slog.Info("split migration complete", "messages", messageRows, "chunks", chunkRows, "checkpoint", checkpointPath)
	return nil
}

// reconcileSplitIDs is the correctness backstop for a resumed migration. The
// bulk copier is fast, while this final ID-only pass proves that every row in
// the pinned source snapshot exists in the appropriate destination table and
// copies only omissions. Newer production rows are already covered by
// dual-write and may legitimately exist only in the destinations.
func (s *cgoLanceStore) reconcileSplitIDs(ctx context.Context, batchSize int) error {
	sourceRows, err := s.legacy.Select(ctx, contracts.QueryConfig{Columns: []string{"id", "record_kind"}})
	if err != nil {
		return fmt.Errorf("scan split migration source IDs: %w", err)
	}
	source := map[string]map[string]struct{}{
		"messages": {},
		"chunks":   {},
	}
	for _, row := range sourceRows {
		id := stringValue(row["id"])
		switch stringValue(row["record_kind"]) {
		case "chunk":
			source["chunks"][id] = struct{}{}
		case "message", "":
			source["messages"][id] = struct{}{}
		default:
			return fmt.Errorf("unsupported legacy record_kind %q for id %q", stringValue(row["record_kind"]), id)
		}
	}
	copyBatch := batchSize
	if copyBatch <= 0 || copyBatch > 256 {
		copyBatch = 256
	}
	for _, part := range []struct {
		name  string
		table contracts.ITable
	}{
		{name: "messages", table: s.messages},
		{name: "chunks", table: s.chunks},
	} {
		destinationRows, err := part.table.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}})
		if err != nil {
			return fmt.Errorf("scan %s migration destination IDs: %w", part.name, err)
		}
		destination := make(map[string]struct{}, len(destinationRows))
		for _, row := range destinationRows {
			destination[stringValue(row["id"])] = struct{}{}
		}
		missing := make([]string, 0)
		for id := range source[part.name] {
			if _, ok := destination[id]; !ok {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		slog.Info("split migration reconciliation", "table", part.name, "source_ids", len(source[part.name]), "destination_ids", len(destination), "missing_ids", len(missing))
		for start := 0; start < len(missing); start += copyBatch {
			end := start + copyBatch
			if end > len(missing) {
				end = len(missing)
			}
			quoted := make([]string, 0, end-start)
			for _, id := range missing[start:end] {
				quoted = append(quoted, lanceQuote(id))
			}
			record, err := s.legacy.Query().Filter("id IN (" + strings.Join(quoted, ",") + ")").Execute(ctx)
			if err != nil {
				return fmt.Errorf("read %s reconciliation batch: %w", part.name, err)
			}
			rows := record.NumRows()
			_, mergeErr := part.table.MergeInsert([]string{"id"}).WhenMatchedUpdateAll(nil).WhenNotMatchedInsertAll().Execute(ctx, []arrow.Record{record})
			record.Release()
			if mergeErr != nil {
				return fmt.Errorf("write %s reconciliation batch: %w", part.name, mergeErr)
			}
			if rows != int64(end-start) {
				return fmt.Errorf("read %s reconciliation rows=%d want=%d", part.name, rows, end-start)
			}
		}
	}
	return nil
}

func saveSplitMigrationState(path string, state splitMigrationState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".split-migration-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *cgoLanceStore) FragmentCounts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for name, table := range s.activeTables() {
		counter, ok := table.(lanceFragmentCounter)
		if !ok {
			return nil, fmt.Errorf("%s fragment count unavailable", name)
		}
		count, err := counter.FragmentCount(ctx)
		if err != nil {
			return nil, fmt.Errorf("count %s fragments: %w", name, err)
		}
		out[name] = count
	}
	return out, nil
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
