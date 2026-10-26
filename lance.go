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
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/lancedb/lancedb-go/pkg/contracts"
	"github.com/lancedb/lancedb-go/pkg/lancedb"
)

const (
	lanceChunksTable     = "chunks"
	lanceMessagesTable   = "messages"
	lanceVectorIndexName = "vector_ivf_pq"
	// One partition: a cold search reads the whole PQ index (~100 B/chunk) in a few large
	// requests instead of two small requests per probed partition, and recall is exact
	// after refine. ponytail: full PQ scan; past ~1M chunks use 64+ partitions, probe ~50%.
	lanceVectorPartitions   = uint32(1)
	lanceVectorSubVectors   = uint32(96)
	lanceVectorRefineFactor = uint32(4)
	lanceVectorNProbes      = 1
)

// Columns shared by messages and chunks, in schema order. Chunks add lanceChunkColumns
// and the vector.
var (
	lanceStringColumns = []string{
		"id", "text", "source", "host", "container_id", "container_kind", "container_title", "project",
		"author_id", "author_name", "kind", "reply_to", "attrs", "content_hash",
	}
	lanceChunkStringColumns = []string{"message_id"}
	lanceChunkIntColumns    = []string{"chunk_index", "chunk_count"}
	lanceMessageColumns     = append(append([]string{}, lanceStringColumns...), "timestamp")
	lanceChunkColumns       = append(append(append([]string{}, lanceMessageColumns...), lanceChunkStringColumns...), lanceChunkIntColumns...)
)

type cgoLanceStore struct {
	conn     contracts.IConnection
	opts     map[string]string
	chunks   contracts.ITable
	messages contracts.ITable
	dims     int
	nprobes  int
	exact    bool
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
	store := &cgoLanceStore{conn: conn, opts: opts, dims: cfg.Dimensions, nprobes: nprobes, exact: cfg.ExactVectorSearch}
	store.chunks, err = store.openOrCreateTable(ctx, lanceChunksTable, true)
	if err == nil {
		store.messages, err = store.openOrCreateTable(ctx, lanceMessagesTable, false)
	}
	if err != nil {
		store.Close()
		return nil, err
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

func (s *cgoLanceStore) openOrCreateTable(ctx context.Context, name string, chunk bool) (contracts.ITable, error) {
	table, err := s.conn.OpenTable(ctx, name)
	if err == nil {
		return table, nil
	}
	schema, err := lancedb.NewSchema(arrow.NewSchema(lanceFields(s.dims, chunk, true), nil))
	if err != nil {
		return nil, err
	}
	return s.conn.CreateTable(ctx, name, schema)
}

// A search's first read of a column loads every page's dictionary (high-cardinality ids
// and hashes cost ~20 MB cold at 330k rows) and every full-zip page's repetition index
// (text, ~1.3 MB). Plain miniblock pages cost KBs.
var (
	lanceNoDictionary = arrow.NewMetadata([]string{"lance-encoding:dict-divisor"}, []string{"1000000000"})
	lanceMiniblock    = arrow.NewMetadata([]string{"lance-encoding:structural-encoding"}, []string{"miniblock"})
	lanceHighCardinal = map[string]bool{"id": true, "container_id": true, "reply_to": true, "attrs": true, "content_hash": true, "message_id": true}
)

// lanceFields is the table schema; encodings only matter when creating the table.
func lanceFields(dims int, chunk, encodings bool) []arrow.Field {
	field := func(name string, typ arrow.DataType) arrow.Field {
		f := arrow.Field{Name: name, Type: typ, Nullable: true}
		if encodings && name == "text" {
			f.Metadata = lanceMiniblock
		} else if encodings && lanceHighCardinal[name] {
			f.Metadata = lanceNoDictionary
		}
		return f
	}
	fields := make([]arrow.Field, 0, len(lanceChunkColumns)+1)
	for _, name := range lanceStringColumns {
		fields = append(fields, field(name, arrow.BinaryTypes.String))
	}
	fields = append(fields, field("timestamp", arrow.PrimitiveTypes.Int64))
	if chunk {
		for _, name := range lanceChunkStringColumns {
			fields = append(fields, field(name, arrow.BinaryTypes.String))
		}
		for _, name := range lanceChunkIntColumns {
			fields = append(fields, field(name, arrow.PrimitiveTypes.Int64))
		}
		fields = append(fields, field("vector", arrow.FixedSizeListOf(int32(dims), arrow.PrimitiveTypes.Float32)))
	}
	return fields
}

func itemString(item MemoryItem, column string) string {
	m := item.Metadata
	switch column {
	case "id":
		return item.ID
	case "text":
		return item.Text
	case "source":
		return m.Source
	case "host":
		return m.Host
	case "container_id":
		return m.ContainerID
	case "container_kind":
		return m.ContainerKind
	case "container_title":
		return m.ContainerTitle
	case "project":
		return m.Project
	case "author_id":
		return m.AuthorID
	case "author_name":
		return m.AuthorName
	case "kind":
		return m.Kind
	case "reply_to":
		return m.ReplyTo
	case "attrs":
		return m.Attrs
	case "content_hash":
		return m.ContentHash
	case "message_id":
		return m.MessageID
	}
	panic("unknown lance string column " + column)
}

func (s *cgoLanceStore) ensureIndexes(ctx context.Context) error {
	if err := s.ensureTableIndexes(ctx, s.chunks, "chunks"); err != nil {
		return err
	}
	return s.ensureTableIndexes(ctx, s.messages, "messages")
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
	if kind == "chunks" && !hasColumn("text") {
		if err := table.CreateIndexWithParams(ctx, []string{"text"}, contracts.IndexTypeFts,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "text_fts", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create text FTS index: %w", err)
		}
	}
	if kind == "messages" && !hasColumn("container_id") {
		if err := table.CreateIndexWithParams(ctx, []string{"container_id"}, contracts.IndexTypeBTree,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "container_id_btree", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create container_id btree index: %w", err)
		}
	}
	if !hasColumn("id") {
		if err := table.CreateIndexWithParams(ctx, []string{"id"}, contracts.IndexTypeBTree,
			contracts.IndexParams{}, &contracts.CreateIndexOptions{Name: "id_btree", WaitTimeout: 2 * time.Minute}); err != nil {
			return fmt.Errorf("create %s id index: %w", kind, err)
		}
	}
	return nil
}

func (s *cgoLanceStore) warm(ctx context.Context) {
	t0 := time.Now()
	one := 1
	if _, err := s.chunks.Select(ctx, contracts.QueryConfig{Columns: []string{"id"}, Limit: &one}); err != nil {
		slog.Warn("lance scan warmup failed", "err", err)
	}
	if _, err := s.chunks.Select(ctx, contracts.QueryConfig{
		Columns: []string{"id"}, Limit: &one,
		FTSSearch: &contracts.FTSSearch{Column: "text", Query: "warmup"},
	}); err != nil {
		slog.Warn("lance FTS warmup failed", "err", err)
	}
	slog.Info("lance warmup complete", "elapsed", time.Since(t0))
}

func (s *cgoLanceStore) Close() error {
	var first error
	for _, table := range []contracts.ITable{s.chunks, s.messages} {
		if table == nil {
			continue
		}
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
	indexes, err := s.chunks.GetAllIndexes(ctx)
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
	partitions, subVectors := lanceVectorPartitions, lanceVectorSubVectors
	if err := s.chunks.CreateIndexWithParams(ctx, []string{"vector"}, contracts.IndexTypeIvfPq,
		contracts.IndexParams{NumPartitions: &partitions, NumSubVectors: &subVectors, DistanceType: contracts.DistanceTypeL2},
		&contracts.CreateIndexOptions{Name: lanceVectorIndexName, WaitTimeout: 10 * time.Minute}); err != nil {
		return fmt.Errorf("create IVF-PQ vector index: %w", err)
	}
	slog.Info("created IVF-PQ vector index", "name", lanceVectorIndexName, "partitions", partitions, "sub_vectors", subVectors, "nprobes", s.nprobes)
	return nil
}

func (s *cgoLanceStore) DropVectorIndex(ctx context.Context) error {
	indexes, err := s.chunks.GetAllIndexes(ctx)
	if err != nil {
		return fmt.Errorf("list lance indexes: %w", err)
	}
	// By column, not name: the IVF-Flat index this replaces is named vector_ivf_flat.
	for _, index := range indexes {
		if len(index.Columns) == 1 && index.Columns[0] == "vector" {
			if err := s.chunks.DropIndex(ctx, index.Name); err != nil {
				return fmt.Errorf("drop vector index %s: %w", index.Name, err)
			}
			slog.Info("dropped vector index", "name", index.Name, "type", index.IndexType)
			return nil
		}
	}
	slog.Info("vector index is absent")
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

func (s *cgoLanceStore) Upsert(ctx context.Context, messages, chunks []MemoryItem) error {
	if err := s.upsertTable(ctx, "chunks", s.chunks, chunks, true); err != nil {
		return err
	}
	return s.upsertTable(ctx, "messages", s.messages, messages, false)
}

func (s *cgoLanceStore) upsertTable(ctx context.Context, name string, table contracts.ITable, items []MemoryItem, chunk bool) error {
	if len(items) == 0 {
		return nil
	}
	rec := lanceRecord(items, s.dims, chunk)
	defer rec.Release()
	if _, err := table.MergeInsert([]string{"id"}).WhenMatchedUpdateAll(nil).WhenNotMatchedInsertAll().Execute(ctx, []arrow.Record{rec}); err != nil {
		return fmt.Errorf("merge insert %s: %w", name, err)
	}
	return nil
}

func (s *cgoLanceStore) activeTables() map[string]contracts.ITable {
	return map[string]contracts.ITable{"chunks": s.chunks, "messages": s.messages}
}

func (s *cgoLanceStore) searchTable(kind string) (contracts.ITable, []string) {
	if kind == "messages" {
		return s.messages, lanceMessageColumns
	}
	return s.chunks, lanceChunkColumns
}

func lanceRecord(items []MemoryItem, dims int, chunk bool) arrow.Record {
	pool := memory.NewGoAllocator()
	fields := lanceFields(dims, chunk, false)
	arrays := make([]arrow.Array, 0, len(fields))
	for _, f := range fields {
		switch f.Name {
		case "vector":
			b := array.NewFixedSizeListBuilder(pool, int32(dims), arrow.PrimitiveTypes.Float32)
			values := b.ValueBuilder().(*array.Float32Builder)
			for _, item := range items {
				b.Append(true)
				values.AppendValues(item.Vector, nil)
			}
			arrays = append(arrays, b.NewArray())
			b.Release()
		case "timestamp", "chunk_index", "chunk_count":
			b := array.NewInt64Builder(pool)
			for _, item := range items {
				switch f.Name {
				case "timestamp":
					b.Append(item.Metadata.Timestamp)
				case "chunk_index":
					b.Append(item.Metadata.ChunkIndex)
				default:
					b.Append(item.Metadata.ChunkCount)
				}
			}
			arrays = append(arrays, b.NewArray())
			b.Release()
		default:
			b := array.NewStringBuilder(pool)
			for _, item := range items {
				b.Append(itemString(item, f.Name))
			}
			arrays = append(arrays, b.NewArray())
			b.Release()
		}
	}
	record := array.NewRecord(arrow.NewSchema(fields, nil), arrays, int64(len(items)))
	for _, values := range arrays {
		values.Release()
	}
	return record
}

func (s *cgoLanceStore) Search(ctx context.Context, req vectorSearchRequest) ([]queryResult, error) {
	table, columns := s.searchTable(req.Table)
	limit := req.K
	if limit <= 0 {
		limit = 10
	}
	where := lanceCombineSQL(lanceFilterSQL(req.Filter), lanceCursorSQL(req.AfterTS, req.AfterID))
	config := contracts.QueryConfig{Columns: columns, Where: where}
	hasVector := len(req.Vector) > 0
	hasText := req.BM25 != nil && strings.TrimSpace(req.BM25.Query) != ""
	if hasText && req.BM25.Field != "" && req.BM25.Field != "text" {
		return nil, fmt.Errorf("unsupported BM25 field %q", req.BM25.Field)
	}
	switch {
	case hasVector:
		config.Limit = &limit
		nprobes := s.nprobes
		refine := lanceVectorRefineFactor
		config.VectorSearch = &contracts.VectorSearch{
			Column: "vector", Vector: req.Vector, K: limit, Nprobes: &nprobes, RefineFactor: &refine,
			BypassVectorIndex: s.exact,
		}
		if hasText {
			config.VectorSearch.FullTextQuery = req.BM25.Query
			config.VectorSearch.FullTextColumn = "text"
			config.Reranker = &contracts.RerankerConfig{Kind: contracts.RerankerRRF, RRFK: 60, Norm: contracts.NormalizeRank}
		}
	case hasText:
		config.Limit = &limit
		config.FTSSearch = &contracts.FTSSearch{Column: "text", Query: req.BM25.Query}
	}
	var rows []map[string]interface{}
	var err error
	if req.Collapse > 0 && (hasVector || hasText) {
		rows, err = selectCollapsed(ctx, table, config, req.Collapse)
	} else {
		rows, err = table.Select(ctx, config)
	}
	if err != nil {
		return nil, err
	}
	results := make([]queryResult, 0, len(rows))
	for _, row := range rows {
		id := stringValue(row["id"])
		content := stringValue(row["text"])
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
		delete(row, "text")
		delete(row, "_relevance_score")
		delete(row, "_score")
		delete(row, "_distance")
		results = append(results, queryResult{ID: id, Score: score, Text: content, Metadata: row})
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

// selectCollapsed ranks candidates reading only message_id, keeps the best row of each of
// the first `messages` messages, then reads the output columns for just those rows. A cold
// search reads ~5 rows of the wide columns instead of fetchK per channel.
func selectCollapsed(ctx context.Context, table contracts.ITable, config contracts.QueryConfig, messages int) ([]map[string]interface{}, error) {
	rank := config
	rank.Columns = []string{"message_id"}
	rank.WithRowID = true
	hits, err := table.Select(ctx, rank)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, messages)
	kept := make([]map[string]interface{}, 0, messages)
	rowIDs := make([]string, 0, messages)
	for _, hit := range hits {
		rowID := strconv.FormatUint(uint64(numberValue(hit["_rowid"])), 10)
		message := stringValue(hit["message_id"])
		if message == "" {
			message = rowID
		}
		if seen[message] {
			continue
		}
		seen[message] = true
		kept = append(kept, hit)
		rowIDs = append(rowIDs, rowID)
		if len(kept) == messages {
			break
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	// No Limit: Lance 10 applies a limit on a `_rowid IN` scan as a row range before the
	// filter and returns nothing.
	full, err := table.Select(ctx, contracts.QueryConfig{Columns: config.Columns, Where: "_rowid IN (" + strings.Join(rowIDs, ",") + ")", WithRowID: true})
	if err != nil {
		return nil, err
	}
	if len(full) != len(kept) {
		// A concurrent merge-insert rewrote a hit between the two reads (row ids are row
		// addresses); rank and fetch in one pass instead.
		return table.Select(ctx, config)
	}
	byRowID := make(map[float64]map[string]interface{}, len(full))
	for _, row := range full {
		byRowID[numberValue(row["_rowid"])] = row
	}
	for i, hit := range kept {
		row := byRowID[numberValue(hit["_rowid"])]
		if row == nil {
			return table.Select(ctx, config)
		}
		for _, k := range []string{"_relevance_score", "_score", "_distance"} {
			if v, ok := hit[k]; ok {
				row[k] = v
			}
		}
		delete(row, "_rowid")
		kept[i] = row
	}
	return kept, nil
}

type genericMigrationState struct {
	SourceVersions map[string]uint64 `json:"source_versions"`
	Offsets        map[string]int64  `json:"offsets"`
	Complete       bool              `json:"complete"`
}

// MigrateFrom copies the token-chunks-v4 messages and chunks tables at sourceURL into
// the generic tables, keeping every vector. The v4 file_path maps to the readable id
// the raw parser emits (see v4MessageID), so raw re-ingestion after the cutover finds
// every message already present and embeds nothing. v4 often stored one message twice,
// as <session>/message/<turn> and <session>/<turn>; both map to one id and the
// /message/ row (the newer derivation) wins. markMessages seeds the dedup state for
// each copied batch of messages.
func (s *cgoLanceStore) MigrateFrom(ctx context.Context, sourceURL string, batchSize int, checkpointPath string, markMessages func([]MemoryItem) error) error {
	state := genericMigrationState{SourceVersions: map[string]uint64{}, Offsets: map[string]int64{}}
	if b, err := os.ReadFile(checkpointPath); err == nil {
		if err := json.Unmarshal(b, &state); err != nil {
			return fmt.Errorf("decode migration checkpoint: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read migration checkpoint: %w", err)
	}
	if state.Complete {
		slog.Info("migration already complete", "checkpoint", checkpointPath)
		return nil
	}
	if batchSize <= 0 {
		batchSize = 1024
	}
	src, err := lancedb.Connect(ctx, sourceURL, &contracts.ConnectionOptions{StorageOptions: s.opts})
	if err != nil {
		return fmt.Errorf("connect migration source: %w", err)
	}
	defer src.Close()
	// shadowed holds the plain v4 paths whose /message/ twin is copied instead.
	shadowed := map[string]bool{}
	for _, part := range []struct {
		name  string
		chunk bool
		dest  contracts.ITable
	}{{"messages", false, s.messages}, {"chunks", true, s.chunks}} {
		table, err := src.OpenTable(ctx, part.name)
		if err != nil {
			return fmt.Errorf("open source %s: %w", part.name, err)
		}
		defer table.Close()
		if state.SourceVersions[part.name] == 0 {
			version, err := table.Version(ctx)
			if err != nil {
				return fmt.Errorf("get source %s version: %w", part.name, err)
			}
			state.SourceVersions[part.name] = uint64(version)
		}
		timeTravel, ok := table.(contracts.ITableTimeTravel)
		if !ok {
			return fmt.Errorf("source %s snapshot capability is unavailable", part.name)
		}
		if err := timeTravel.Checkout(ctx, state.SourceVersions[part.name]); err != nil {
			return fmt.Errorf("pin source %s version %d: %w", part.name, state.SourceVersions[part.name], err)
		}
		sourceRows, err := table.Count(ctx)
		if err != nil {
			return fmt.Errorf("count source %s: %w", part.name, err)
		}
		paths, err := table.Select(ctx, contracts.QueryConfig{Columns: []string{"file_path"}})
		if err != nil {
			return fmt.Errorf("scan source %s paths: %w", part.name, err)
		}
		if !part.chunk {
			for _, row := range paths {
				path, _ := row["file_path"].(string)
				if parts := strings.Split(path, "/"); len(parts) == 5 && parts[3] == "message" {
					shadowed[parts[0]+"/"+parts[1]+"/"+parts[2]+"/"+parts[4]] = true
				}
			}
		}
		expectedRows := sourceRows
		for _, row := range paths {
			if path, _ := row["file_path"].(string); shadowed[path] {
				expectedRows--
			}
		}
		slog.Info("migration table starting", "table", part.name, "source_version", state.SourceVersions[part.name], "source_rows", sourceRows, "expected_rows", expectedRows, "offset", state.Offsets[part.name])
		for {
			record, err := table.Query().Limit(batchSize).Offset(int(state.Offsets[part.name])).Execute(ctx)
			if err != nil {
				return fmt.Errorf("read %s batch at %d: %w", part.name, state.Offsets[part.name], err)
			}
			if record == nil || record.NumRows() == 0 {
				if record != nil {
					record.Release()
				}
				break
			}
			sourceBatch := record.NumRows()
			items, err := v4Items(record, part.chunk, shadowed)
			record.Release()
			if err != nil {
				return fmt.Errorf("convert %s batch at %d: %w", part.name, state.Offsets[part.name], err)
			}
			if err := s.upsertTable(ctx, part.name, part.dest, items, part.chunk); err != nil {
				return err
			}
			if !part.chunk {
				if err := markMessages(items); err != nil {
					return err
				}
			}
			state.Offsets[part.name] += sourceBatch
			if err := saveMigrationState(checkpointPath, state); err != nil {
				return err
			}
			slog.Info("migration batch complete", "table", part.name, "rows", len(items), "offset", state.Offsets[part.name], "source_rows", sourceRows)
		}
		destRows, err := part.dest.Count(ctx)
		if err != nil {
			return fmt.Errorf("count migrated %s: %w", part.name, err)
		}
		// Every unshadowed v4 row must stay distinct: fewer destination rows means two
		// of them mapped to one readable id.
		if destRows < expectedRows {
			return fmt.Errorf("migrated %s has %d rows, expected %d: readable ids collided", part.name, destRows, expectedRows)
		}
		slog.Info("migration table complete", "table", part.name, "expected_rows", expectedRows, "destination_rows", destRows)
	}
	state.Complete = true
	return saveMigrationState(checkpointPath, state)
}

// v4Items converts a token-chunks-v4 messages or chunks batch into generic rows,
// dropping rows whose file_path is shadowed by a /message/ twin.
func v4Items(record arrow.Record, chunk bool, shadowed map[string]bool) ([]MemoryItem, error) {
	str := func(name string) (func(int) string, error) {
		idx := record.Schema().FieldIndices(name)
		if len(idx) == 0 {
			return nil, fmt.Errorf("column %s missing", name)
		}
		col := record.Column(idx[0])
		return func(i int) string {
			if col.IsNull(i) {
				return ""
			}
			return col.ValueStr(i)
		}, nil
	}
	i64 := func(name string) (func(int) int64, error) {
		idx := record.Schema().FieldIndices(name)
		if len(idx) == 0 {
			return nil, fmt.Errorf("column %s missing", name)
		}
		col, ok := record.Column(idx[0]).(*array.Int64)
		if !ok {
			return nil, fmt.Errorf("column %s is %s, want int64", name, record.Column(idx[0]).DataType())
		}
		return func(i int) int64 {
			if col.IsNull(i) {
				return 0
			}
			return col.Value(i)
		}, nil
	}
	cols := map[string]func(int) string{}
	for _, name := range []string{"id", "forward_content", "project_name", "file_path", "file_hash", "session_id", "host", "harness", "role"} {
		f, err := str(name)
		if err != nil {
			return nil, err
		}
		cols[name] = f
	}
	ts, err := i64("timestamp")
	if err != nil {
		return nil, err
	}
	chunkCount, err := i64("chunk_count")
	if err != nil {
		return nil, err
	}
	var chunkIndex func(int) int64
	var vectors *array.FixedSizeList
	var vectorValues []float32
	if chunk {
		if chunkIndex, err = i64("chunk_index"); err != nil {
			return nil, err
		}
		idx := record.Schema().FieldIndices("vector")
		if len(idx) == 0 {
			return nil, fmt.Errorf("column vector missing")
		}
		var ok bool
		if vectors, ok = record.Column(idx[0]).(*array.FixedSizeList); !ok {
			return nil, fmt.Errorf("column vector is %s", record.Column(idx[0]).DataType())
		}
		values, ok := vectors.ListValues().(*array.Float32)
		if !ok {
			return nil, fmt.Errorf("vector values are %s", vectors.ListValues().DataType())
		}
		vectorValues = values.Float32Values()
	}
	items := make([]MemoryItem, 0, int(record.NumRows()))
	for i := 0; i < int(record.NumRows()); i++ {
		if shadowed[cols["file_path"](i)] {
			continue
		}
		messageID, kind := v4MessageID(cols["file_path"](i), cols["id"](i))
		item := MemoryItem{
			ID:   messageID,
			Text: cols["forward_content"](i),
			Metadata: Metadata{
				Source: cols["harness"](i), Host: cols["host"](i), ContainerID: cols["session_id"](i),
				ContainerKind: "agent_session", Project: cols["project_name"](i), AuthorID: cols["role"](i),
				Kind: kind, Timestamp: ts(i), ContentHash: cols["file_hash"](i), ChunkCount: chunkCount(i),
			},
		}
		if chunk {
			item.ID = chunkID(messageID, int(chunkIndex(i)))
			item.Metadata.MessageID = messageID
			item.Metadata.ChunkIndex = chunkIndex(i)
			start, end := vectors.ValueOffsets(i)
			item.Vector = append([]float32(nil), vectorValues[start:end]...)
		}
		items = append(items, item)
	}
	return items, nil
}

func saveMigrationState(path string, state genericMigrationState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".migration-*")
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
