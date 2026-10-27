package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	defaultListenAddr = ":8090"
	defaultRegion     = "eu-west-1"
	defaultStorageURL = "s3://YOUR_PRODUCTION_BUCKET_NAME/vector-index"
)

// Metadata is the generic record shape shared by every source (agent sessions,
// Telegram, ...). Source-specific extras go in Attrs as a JSON object string.
type Metadata struct {
	Source         string `json:"source"`
	Host           string `json:"host"`
	ContainerID    string `json:"container_id"`
	ContainerKind  string `json:"container_kind,omitempty"`
	ContainerTitle string `json:"container_title,omitempty"`
	Project        string `json:"project,omitempty"`
	AuthorID       string `json:"author_id,omitempty"`
	AuthorName     string `json:"author_name,omitempty"`
	Kind           string `json:"kind,omitempty"`
	ReplyTo        string `json:"reply_to,omitempty"`
	Timestamp      int64  `json:"timestamp"`
	Attrs          string `json:"attrs,omitempty"`
	ContentHash    string `json:"content_hash,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	ChunkIndex     int64  `json:"chunk_index,omitempty"`
	ChunkCount     int64  `json:"chunk_count,omitempty"`
}

func metadataMap(m Metadata) map[string]interface{} {
	return map[string]interface{}{
		"source": m.Source, "host": m.Host, "container_id": m.ContainerID, "container_kind": m.ContainerKind,
		"container_title": m.ContainerTitle, "project": m.Project, "author_id": m.AuthorID, "author_name": m.AuthorName,
		"kind": m.Kind, "reply_to": m.ReplyTo, "timestamp": m.Timestamp, "attrs": m.Attrs, "content_hash": m.ContentHash,
		"message_id": m.MessageID, "chunk_index": m.ChunkIndex, "chunk_count": m.ChunkCount,
	}
}

// MemoryItem is one messages row (MessageID empty) or one chunks row.
type MemoryItem struct {
	ID       string    `json:"id"`
	Vector   []float32 `json:"vector,omitempty"`
	Text     string    `json:"text"`
	Metadata Metadata  `json:"metadata"`
}

// chunkID is the id of chunk i of message id: readable, so an ingesting host can
// address its own rows.
func chunkID(messageID string, i int) string {
	return messageID + "#" + strconv.Itoa(i)
}

type ingestRequest struct {
	Records []MemoryItem `json:"records"`
}

type ingestResponse struct {
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

type queryRequest struct {
	QueryVector   []float32 `json:"query_vector,omitempty"`
	QueryText     string    `json:"query_text,omitempty"`
	Source        string    `json:"source,omitempty"`
	Host          string    `json:"host,omitempty"`
	ContainerID   string    `json:"container_id,omitempty"`
	ContainerKind string    `json:"container_kind,omitempty"`
	Project       string    `json:"project,omitempty"`
	AuthorID      string    `json:"author_id,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	Limit         int       `json:"limit"`
}

type queryResult struct {
	ID       string                 `json:"id"`
	Score    float64                `json:"score"`
	Text     string                 `json:"text"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type queryResponse struct {
	Results []queryResult `json:"results"`
}

type bm25Query struct {
	Field string `json:"field"`
	Query string `json:"query"`
}

type vectorSearchRequest struct {
	Table   string        `json:"-"`
	Vector  []float32     `json:"vector,omitempty"`
	BM25    *bm25Query    `json:"bm25,omitempty"`
	K       int           `json:"k"`
	Filter  *vectorFilter `json:"filter,omitempty"`
	AfterTS int64         `json:"after_ts,omitempty"`
	AfterID string        `json:"after_id,omitempty"`
	// BeforeTS > 0 keeps rows with timestamp < BeforeTS.
	BeforeTS int64 `json:"-"`
	// Collapse > 0 returns at most this many rows with distinct message_id, reading the
	// output columns only for them (vector/text searches).
	Collapse int `json:"-"`
}

type vectorFilter struct {
	Eq  *comparisonFilter `json:"eq,omitempty"`
	And []vectorFilter    `json:"and,omitempty"`
}

type comparisonFilter struct {
	Field string      `json:"field"`
	Value interface{} `json:"value"`
}

type dedupState struct {
	mu    sync.RWMutex
	Path  string            `json:"-"`
	Files map[string]string `json:"files"` // dedup key (message id, plus parser version for raw) -> content_hash
}

type server struct {
	cfg        runtimeConfig
	dedup      *dedupState
	writeMu    sync.Mutex
	querySlots chan struct{}
	mem        *memStore
	lance      lanceStore
	readyCheck func(context.Context) error
	raw        *rawIngestor
}

type runtimeConfig struct {
	ListenAddr        string
	StorageURL        string
	AWSRegion         string
	S3Endpoint        string
	Dimensions        int
	VectorNProbes     int
	ExactVectorSearch bool
	MigrateFrom       string
	PrintFragments    bool
	MigrationBatch    int
	MaxConcurrent     int
	QueryTimeout      time.Duration
	StatePath         string
	DryRunS3          bool
	Optimize          bool
	CreateVectorIndex bool
	DropVectorIndex   bool
	AuditDuplicates   bool
	DeleteHost        string
	RawURL            string
	RawPollInterval   time.Duration
	RawWriteInterval  time.Duration
	RawMaxObjectBytes int64
	EmbedURL          string
	EmbedBatchSize    int
}

type duplicateAudit struct {
	Rows         int      `json:"rows"`
	UniqueIDs    int      `json:"unique_ids"`
	DuplicateIDs []string `json:"duplicate_ids"`
}

type lanceStore interface {
	Close() error
	Upsert(ctx context.Context, messages, chunks []MemoryItem) error
	Search(context.Context, vectorSearchRequest) ([]queryResult, error)
	Optimize(context.Context) error
	CreateVectorIndex(context.Context) error
	DropVectorIndex(context.Context) error
	AuditDuplicates(context.Context) (duplicateAudit, error)
	DeleteHost(context.Context, string) (int, error)
	MigrateFrom(ctx context.Context, sourceURL string, batch int, checkpoint string, markMessages func([]MemoryItem) error) error
	FragmentCounts(context.Context) (map[string]int, error)
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	local := isMemoryURL(cfg.StorageURL)
	if local {
		cfg.DryRunS3 = true
	}
	if err := validateS3(context.Background(), cfg); err != nil {
		slog.Error("s3 validation failed", "err", err)
		os.Exit(1)
	}
	state, err := loadDedupState(cfg.StatePath)
	if err != nil {
		slog.Error("load dedup state", "err", err)
		os.Exit(1)
	}

	s := &server{
		cfg:        cfg,
		dedup:      state,
		querySlots: make(chan struct{}, cfg.MaxConcurrent),
	}
	if local {
		s.mem = newMemStore(cfg.Dimensions)
		slog.Info("using in-memory vector store (memory://)")
	}

	if !local {
		s.lance, err = openLanceStore(context.Background(), cfg)
		if err != nil {
			slog.Error("open lance store", "err", err)
			os.Exit(1)
		}
		defer s.lance.Close()
		s.readyCheck = func(ctx context.Context) error {
			_, err := s.runSearch(ctx, vectorSearchRequest{
				Table: "chunks",
				BM25:  &bm25Query{Field: "text", Query: "warmup"},
				K:     1,
			})
			return err
		}
		if cfg.CreateVectorIndex && cfg.DropVectorIndex {
			slog.Error("--create-vector-index and --drop-vector-index are mutually exclusive")
			os.Exit(1)
		}
		if cfg.CreateVectorIndex {
			if err := s.lance.CreateVectorIndex(context.Background()); err != nil {
				slog.Error("create vector index", "err", err)
				os.Exit(1)
			}
		}
		if cfg.DropVectorIndex {
			if err := s.lance.DropVectorIndex(context.Background()); err != nil {
				slog.Error("drop vector index", "err", err)
				os.Exit(1)
			}
		}
		if cfg.Optimize {
			if err := s.lance.Optimize(context.Background()); err != nil {
				slog.Error("optimize lance store", "err", err)
				os.Exit(1)
			}
		}
		if cfg.AuditDuplicates {
			audit, err := s.lance.AuditDuplicates(context.Background())
			if err != nil {
				slog.Error("audit duplicate IDs", "err", err)
				os.Exit(1)
			}
			if err := json.NewEncoder(os.Stdout).Encode(audit); err != nil {
				slog.Error("encode duplicate audit", "err", err)
				os.Exit(1)
			}
			if len(audit.DuplicateIDs) != 0 {
				os.Exit(2)
			}
		}
		if cfg.DeleteHost != "" {
			if strings.Contains(cfg.DeleteHost, "/") {
				slog.Error("delete host", "err", "host must not contain a slash")
				os.Exit(1)
			}
			rows, err := s.lance.DeleteHost(context.Background(), cfg.DeleteHost)
			if err != nil {
				slog.Error("delete host rows", "host", cfg.DeleteHost, "err", err)
				os.Exit(1)
			}
			registryEntries := s.dedup.RemoveHost(cfg.DeleteHost)
			if err := s.dedup.Save(); err != nil {
				slog.Error("save dedup state after host deletion", "host", cfg.DeleteHost, "err", err)
				os.Exit(1)
			}
			rawRegistry, err := loadRawRegistry(filepath.Join(filepath.Dir(cfg.StatePath), "raw_registry.json"))
			if err != nil {
				slog.Error("load raw registry for host deletion", "host", cfg.DeleteHost, "err", err)
				os.Exit(1)
			}
			rawRegistryEntries := rawRegistry.RemoveHost(cfg.DeleteHost)
			if rawRegistryEntries > 0 {
				if err := rawRegistry.Save(); err != nil {
					slog.Error("save raw registry after host deletion", "host", cfg.DeleteHost, "err", err)
					os.Exit(1)
				}
			}
			if err := json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"deleted_host": cfg.DeleteHost, "rows": rows, "registry_entries": registryEntries, "raw_registry_entries": rawRegistryEntries,
			}); err != nil {
				slog.Error("encode host deletion", "err", err)
				os.Exit(1)
			}
		}
		if cfg.MigrateFrom != "" {
			checkpoint := filepath.Join(filepath.Dir(cfg.StatePath), "generic_migration.json")
			if err := s.lance.MigrateFrom(context.Background(), cfg.MigrateFrom, cfg.MigrationBatch, checkpoint, func(messages []MemoryItem) error {
				for _, message := range messages {
					s.dedup.Mark(rawDerivedKey(message.ID), message.Metadata.ContentHash)
				}
				return s.dedup.Save()
			}); err != nil {
				slog.Error("migrate from token-chunks-v4 store", "source", cfg.MigrateFrom, "err", err)
				os.Exit(1)
			}
		}
		if cfg.PrintFragments {
			counts, err := s.lance.FragmentCounts(context.Background())
			if err != nil {
				slog.Error("count fragments", "err", err)
				os.Exit(1)
			}
			if err := json.NewEncoder(os.Stdout).Encode(counts); err != nil {
				slog.Error("encode fragment counts", "err", err)
				os.Exit(1)
			}
		}
		if cfg.Optimize || cfg.CreateVectorIndex || cfg.DropVectorIndex || cfg.AuditDuplicates || cfg.DeleteHost != "" || cfg.MigrateFrom != "" || cfg.PrintFragments {
			return
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/ready", s.ready)
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)
	mux.HandleFunc("GET /v1/memory/container", s.containerWalk)
	mux.HandleFunc("GET /v1/memory/sessions", s.sessions)
	mux.HandleFunc("GET /v1/raw/status", s.rawStatus)

	if cfg.RawURL != "" {
		raw, err := newRawIngestor(context.Background(), s)
		if err != nil {
			slog.Error("initialize raw ingestion", "err", err)
			os.Exit(1)
		}
		s.raw = raw
		go raw.Run(context.Background())
	}

	slog.Info("pi-memoryd listening", "addr", cfg.ListenAddr, "storage_url", cfg.StorageURL)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		slog.Error("http server stopped", "err", err)
		os.Exit(1)
	}
}

func loadConfig() runtimeConfig {
	cfg := runtimeConfig{}
	flag.StringVar(&cfg.ListenAddr, "listen", env("PI_MEMORYD_LISTEN", defaultListenAddr), "HTTP listen address")
	flag.StringVar(&cfg.StorageURL, "storage-url", env("PI_MEMORYD_STORAGE_URL", defaultStorageURL), "S3 URL, e.g. s3://bucket/vector-index")
	flag.StringVar(&cfg.AWSRegion, "aws-region", env("AWS_REGION", env("AWS_DEFAULT_REGION", defaultRegion)), "AWS region")
	flag.StringVar(&cfg.S3Endpoint, "s3-endpoint", env("PI_MEMORYD_S3_ENDPOINT", env("AWS_ENDPOINT_URL", env("AWS_ENDPOINT", ""))), "S3-compatible endpoint URL")
	flag.IntVar(&cfg.Dimensions, "dimensions", envInt("PI_MEMORYD_DIMENSIONS", 384), "vector dimensions")
	flag.IntVar(&cfg.VectorNProbes, "vector-nprobes", envInt("PI_MEMORYD_VECTOR_NPROBES", 1), "IVF partitions scanned per vector query")
	flag.BoolVar(&cfg.ExactVectorSearch, "exact-vector-search", envBool("PI_MEMORYD_EXACT_VECTOR_SEARCH", true), "bypass ANN and exhaustively scan vectors for exact recall")
	flag.StringVar(&cfg.MigrateFrom, "migrate-from", "", "copy messages and chunks (vectors kept) from this token-chunks-v4 storage URL into the generic tables, then exit")
	flag.BoolVar(&cfg.PrintFragments, "fragment-counts", false, "print active table fragment counts and exit")
	flag.IntVar(&cfg.MigrationBatch, "migration-batch", envInt("PI_MEMORYD_MIGRATION_BATCH", 1024), "rows per resumable migration batch")
	flag.IntVar(&cfg.MaxConcurrent, "max-concurrent-queries", envInt("PI_MEMORYD_MAX_CONCURRENT_QUERIES", 2), "maximum in-flight Lance searches")
	flag.DurationVar(&cfg.QueryTimeout, "query-timeout", envDuration("PI_MEMORYD_QUERY_TIMEOUT", 30*time.Second), "server-side recall query timeout")
	flag.StringVar(&cfg.StatePath, "state", env("PI_MEMORYD_STATE", "./data/dedup_state.json"), "dedup state path")
	flag.BoolVar(&cfg.DryRunS3, "dry-run-s3", envBool("PI_MEMORYD_DRY_RUN_S3", false), "skip AWS SDK S3 validation")
	flag.BoolVar(&cfg.Optimize, "optimize", false, "compact data, refresh indexes, prune old versions, and exit")
	flag.BoolVar(&cfg.CreateVectorIndex, "create-vector-index", false, "create the single-partition IVF-PQ vector index and exit")
	flag.BoolVar(&cfg.DropVectorIndex, "drop-vector-index", false, "drop the vector index and exit")
	flag.BoolVar(&cfg.AuditDuplicates, "audit-duplicates", false, "scan all rows for duplicate IDs and exit non-zero if any exist")
	flag.StringVar(&cfg.DeleteHost, "delete-host", "", "delete every row and dedup entry for exactly this host, then exit")
	flag.StringVar(&cfg.RawURL, "raw-url", env("PI_MEMORYD_RAW_URL", ""), "optional S3 prefix containing raw session files")
	flag.DurationVar(&cfg.RawPollInterval, "raw-poll-interval", envDuration("PI_MEMORYD_RAW_POLL_INTERVAL", 30*time.Second), "raw S3 polling interval")
	flag.DurationVar(&cfg.RawWriteInterval, "raw-write-interval", envDuration("PI_MEMORYD_RAW_WRITE_INTERVAL", 5*time.Minute), "minimum interval between reindexing successive versions of a growing raw object")
	flag.Int64Var(&cfg.RawMaxObjectBytes, "raw-max-object-bytes", envInt64("PI_MEMORYD_RAW_MAX_OBJECT_BYTES", 128<<20), "largest raw object accepted")
	flag.StringVar(&cfg.EmbedURL, "embed-url", env("PI_MEMORYD_EMBED_URL", "http://llama-embed:8091"), "OpenAI-compatible embedding server")
	flag.IntVar(&cfg.EmbedBatchSize, "embed-batch-size", envInt("PI_MEMORYD_EMBED_BATCH_SIZE", 32), "texts per embedding request")
	flag.Parse()
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.MigrationBatch <= 0 {
		cfg.MigrationBatch = 1024
	}
	return cfg
}

func isMemoryURL(raw string) bool {
	return strings.HasPrefix(raw, "memory:")
}

func validateS3(ctx context.Context, cfg runtimeConfig) error {
	if isMemoryURL(cfg.StorageURL) || cfg.DryRunS3 {
		return nil
	}
	bucket, prefix, err := parseS3URL(cfg.StorageURL)
	if err != nil {
		return err
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.AWSRegion))
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
			// Backblaze B2 and most S3-compatible providers expect path-style buckets.
			o.UsePathStyle = true
		}
	})
	_, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix, MaxKeys: int32Ptr(1)})
	return err
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	if s.readyCheck != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.readyCheck(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *server) ingest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := decodeJSON(r, &req); err != nil {
		errorJSON(w, http.StatusBadRequest, err)
		return
	}
	resp := ingestResponse{}
	messages := make([]MemoryItem, 0, len(req.Records))
	chunks := make([]MemoryItem, 0, len(req.Records))
	pending := make(map[string]string, len(req.Records))
	for i, rec := range req.Records {
		if err := validateMemoryItem(rec, s.cfg.Dimensions); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("records[%d]: %v", i, err))
			continue
		}
		rec.Metadata.ContentHash = hashText(rec.Text)
		if rec.Metadata.Kind == "" {
			rec.Metadata.Kind = "message"
		}
		rec.Metadata.MessageID, rec.Metadata.ChunkIndex = "", 0
		if s.dedup.Seen(rec.ID, rec.Metadata.ContentHash) {
			resp.Skipped++
			continue
		}
		if hash, ok := pending[rec.ID]; ok {
			if hash != rec.Metadata.ContentHash {
				resp.Errors = append(resp.Errors, fmt.Sprintf("records[%d]: id %q has conflicting content", i, rec.ID))
				continue
			}
			resp.Skipped++
			continue
		}
		pending[rec.ID] = rec.Metadata.ContentHash
		chunk := rec
		chunk.ID = chunkID(rec.ID, 0)
		chunk.Metadata.MessageID = rec.ID
		chunk.Metadata.ChunkCount = 1
		rec.Vector = nil
		rec.Metadata.ChunkCount = 1
		messages = append(messages, rec)
		chunks = append(chunks, chunk)
	}
	if err := s.flush(messages, chunks, func(m MemoryItem) string { return m.ID }); err != nil {
		resp.Errors = append(resp.Errors, err.Error())
		writeJSON(w, http.StatusBadGateway, resp)
		return
	}
	resp.Accepted = len(messages)
	status := http.StatusAccepted
	if len(resp.Errors) > 0 && resp.Accepted == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
}

func (s *server) query(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := decodeJSON(r, &req); err != nil {
		errorJSON(w, http.StatusBadRequest, err)
		return
	}
	hasVec := len(req.QueryVector) > 0
	hasText := strings.TrimSpace(req.QueryText) != ""
	if !hasVec && !hasText {
		errorJSON(w, http.StatusBadRequest, errors.New("query_vector or query_text required"))
		return
	}
	if hasVec && len(req.QueryVector) != s.cfg.Dimensions {
		errorJSON(w, http.StatusBadRequest, fmt.Errorf("query_vector dimensions=%d want=%d", len(req.QueryVector), s.cfg.Dimensions))
		return
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}
	filter := queryFilter(req)
	fetchK := req.Limit * 8
	if fetchK < 50 {
		fetchK = 50
	}
	if hasVec && hasText && fetchK < 20 {
		fetchK = 20
	}
	if s.mem != nil {
		var lists [][]queryResult
		if hasVec {
			lists = append(lists, s.mem.search(vectorSearchRequest{Vector: req.QueryVector, K: fetchK, Filter: filter}))
		}
		if hasText {
			lists = append(lists, s.mem.search(vectorSearchRequest{BM25: &bm25Query{Field: "text", Query: req.QueryText}, K: fetchK, Filter: filter}))
		}
		out := queryResponse{Results: lists[0]}
		if len(lists) == 2 {
			out.Results = rrfMerge(lists[0], lists[1], fetchK)
		}
		out.Results = collapseChunkHits(out.Results, req.Limit)
		writeJSON(w, http.StatusOK, out)
		return
	}
	searchReq := vectorSearchRequest{Table: "chunks", K: fetchK, Filter: filter, Collapse: req.Limit}
	if hasVec {
		searchReq.Vector = req.QueryVector
	}
	if hasText {
		searchReq.BM25 = &bm25Query{Field: "text", Query: req.QueryText}
	}
	hits, err := s.runSearch(r.Context(), searchReq)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		errorJSON(w, status, err)
		return
	}
	out := queryResponse{Results: collapseChunkHits(hits, req.Limit)}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) containerWalk(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cid := strings.TrimSpace(q.Get("container_id"))
	if cid == "" {
		errorJSON(w, http.StatusBadRequest, errors.New("container_id required"))
		return
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	if limit <= 0 {
		limit = 50
	}
	var afterTS int64
	if v := q.Get("after_ts"); v != "" {
		fmt.Sscanf(v, "%d", &afterTS)
	}
	afterID := q.Get("after_id")
	host := strings.TrimSpace(q.Get("host"))
	source := strings.TrimSpace(q.Get("source"))
	includeKinds, includeState := containerIncludeKinds(q.Get("include"))
	if s.raw != nil {
		if includeState {
			results, ok, err := s.raw.containerStateWalk(r.Context(), cid, host, source, afterTS, afterID, limit, includeKinds)
			if err != nil {
				errorJSON(w, http.StatusBadGateway, err)
				return
			}
			if ok {
				writeJSON(w, http.StatusOK, queryResponse{Results: results})
				return
			}
		}
		results, ok, err := s.raw.containerWalk(r.Context(), cid, host, source, afterTS, afterID, limit)
		if err != nil {
			errorJSON(w, http.StatusBadGateway, err)
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, queryResponse{Results: results})
			return
		}
	}
	if s.mem != nil {
		writeJSON(w, http.StatusOK, queryResponse{Results: s.mem.byContainer(cid, host, source, includeKinds, afterTS, afterID, limit)})
		return
	}
	filter := containerWalkFilter(cid, host, source)
	if kindFilter := containerKindFilter(includeKinds); kindFilter != nil {
		filter = appendFilter(filter, *kindFilter)
	}
	hits, err := s.runSearch(r.Context(), vectorSearchRequest{
		Table:   "messages",
		K:       limit,
		Filter:  filter,
		AfterTS: afterTS,
		AfterID: afterID,
	})
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		errorJSON(w, status, err)
		return
	}
	sort.Slice(hits, func(i, j int) bool {
		ti, tj := metaTS(hits[i]), metaTS(hits[j])
		if ti == tj {
			return hits[i].ID < hits[j].ID
		}
		return ti < tj
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	writeJSON(w, http.StatusOK, queryResponse{Results: hits})
}

type sessionSummary struct {
	ContainerID    string `json:"container_id"`
	ContainerTitle string `json:"container_title,omitempty"`
	Host           string `json:"host"`
	Source         string `json:"source"`
	Project        string `json:"project,omitempty"`
	FirstTS        int64  `json:"first_ts"`
	LastTS         int64  `json:"last_ts"`
	Messages       int    `json:"messages"`
	FirstPrompt    string `json:"first_prompt"`
}

// sessions lists every container with messages in [since, until), oldest first, with its
// message count in the window and its first user message there (first message for chats).
// ponytail: reads every message row of the window, text included; for multi-week windows
// read text only for the chosen first prompts.
func (s *server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	until, _ := strconv.ParseInt(q.Get("until"), 10, 64)
	if since <= 0 {
		errorJSON(w, http.StatusBadRequest, errors.New("since (unix seconds) required"))
		return
	}
	hits, err := s.runSearch(r.Context(), vectorSearchRequest{
		Table:    "messages",
		K:        math.MaxInt32,
		Filter:   eqFilters("kind", "message", "host", q.Get("host"), "source", q.Get("source"), "project", q.Get("project")),
		AfterTS:  since,
		BeforeTS: until,
	})
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		errorJSON(w, status, err)
		return
	}
	out := make([]*sessionSummary, 0)
	byKey := map[string]*sessionSummary{}
	userPrompt := map[*sessionSummary]bool{}
	for _, h := range hits { // timestamp order
		str := func(k string) string { v, _ := h.Metadata[k].(string); return v }
		key := str("host") + "/" + str("source") + "/" + str("container_id")
		sum := byKey[key]
		if sum == nil {
			sum = &sessionSummary{ContainerID: str("container_id"), ContainerTitle: str("container_title"), Host: str("host"),
				Source: str("source"), Project: str("project"), FirstTS: metaTS(h)}
			byKey[key] = sum
			out = append(out, sum)
		}
		sum.LastTS = metaTS(h)
		sum.Messages++
		if isUser := str("author_id") == "user"; sum.FirstPrompt == "" || isUser && !userPrompt[sum] {
			sum.FirstPrompt = clipRunes(h.Text, 300)
			userPrompt[sum] = isUser
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func containerIncludeKinds(include string) (map[string]bool, bool) {
	switch strings.ToLower(strings.TrimSpace(include)) {
	case "todo", "todos":
		return map[string]bool{"todo": true}, true
	case "task", "tasks":
		return map[string]bool{"task": true}, true
	case "title", "titles":
		return map[string]bool{"title": true}, true
	case "state", "important":
		return map[string]bool{"todo": true, "task": true, "title": true}, true
	default:
		return map[string]bool{"message": true}, false
	}
}

// containerKindFilter narrows a walk to one kind; several kinds (include=state) read
// every kind of the container.
func containerKindFilter(kinds map[string]bool) *vectorFilter {
	if len(kinds) != 1 {
		return nil
	}
	for kind := range kinds {
		return &vectorFilter{Eq: &comparisonFilter{Field: "kind", Value: kind}}
	}
	return nil
}

func containerWalkFilter(containerID, host, source string) *vectorFilter {
	return eqFilters("container_id", containerID, "host", host, "source", source)
}

// eqFilters ANDs field = value for each non-empty value of the (field, value) pairs.
func eqFilters(pairs ...string) *vectorFilter {
	var parts []vectorFilter
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: pairs[i], Value: pairs[i+1]}})
		}
	}
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return &parts[0]
	default:
		return &vectorFilter{And: parts}
	}
}

func metaTS(r queryResult) int64 {
	switch v := r.Metadata["timestamp"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func (s *server) vectorSearch(ctx context.Context, searchReq vectorSearchRequest) ([]queryResult, error) {
	if s.mem != nil {
		return s.mem.search(searchReq), nil
	}
	if s.lance == nil {
		return nil, errors.New("lance store is not initialized")
	}
	return s.lance.Search(ctx, searchReq)
}

type searchOutcome struct {
	hits []queryResult
	err  error
}

// runSearch bounds abandoned CGO searches. The current lancedb-go FFI does
// not propagate Context cancellation into Rust, so a timed-out request keeps
// its slot until the native operation actually returns instead of allowing an
// unbounded pile-up of orphaned queries.
func (s *server) runSearch(parent context.Context, req vectorSearchRequest) ([]queryResult, error) {
	if s.querySlots == nil {
		return s.vectorSearch(parent, req)
	}
	ctx := parent
	cancel := func() {}
	if s.cfg.QueryTimeout > 0 {
		ctx, cancel = context.WithTimeout(parent, s.cfg.QueryTimeout)
	}
	defer cancel()
	select {
	case s.querySlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	started := time.Now()
	done := make(chan searchOutcome, 1)
	go func() {
		hits, err := s.vectorSearch(ctx, req)
		<-s.querySlots
		done <- searchOutcome{hits: hits, err: err}
	}()
	select {
	case out := <-done:
		slog.Info("recall query complete", "table", req.Table, "elapsed", time.Since(started), "fetch_k", req.K, "results", len(out.hits), "err", out.err)
		return out.hits, out.err
	case <-ctx.Done():
		slog.Warn("recall query timed out; native work remains bounded", "table", req.Table, "elapsed", time.Since(started), "fetch_k", req.K)
		return nil, ctx.Err()
	}
}

func queryFilter(req queryRequest) *vectorFilter {
	return eqFilters("source", req.Source, "host", req.Host, "container_id", req.ContainerID,
		"container_kind", req.ContainerKind, "project", req.Project, "author_id", req.AuthorID, "kind", req.Kind)
}

func appendFilter(base *vectorFilter, extra vectorFilter) *vectorFilter {
	if base == nil {
		return &extra
	}
	if len(base.And) > 0 {
		parts := append([]vectorFilter{}, base.And...)
		parts = append(parts, extra)
		return &vectorFilter{And: parts}
	}
	return &vectorFilter{And: []vectorFilter{*base, extra}}
}

func collapseChunkHits(hits []queryResult, limit int) []queryResult {
	seen := make(map[string]bool, len(hits))
	out := make([]queryResult, 0, len(hits))
	for _, hit := range hits {
		parent, _ := hit.Metadata["message_id"].(string)
		if parent == "" {
			parent = hit.ID
		}
		if seen[parent] {
			continue
		}
		seen[parent] = true
		hit.Metadata["matched_chunk_id"] = hit.ID
		hit.ID = parent
		out = append(out, hit)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func rrfMerge(a, b []queryResult, limit int) []queryResult {
	const k = 60
	score := map[string]float64{}
	best := map[string]queryResult{}
	add := func(list []queryResult) {
		for i, hit := range list {
			score[hit.ID] += 1.0 / float64(k+i+1)
			if _, ok := best[hit.ID]; !ok {
				best[hit.ID] = hit
			}
		}
	}
	add(a)
	add(b)
	out := make([]queryResult, 0, len(best))
	for id, hit := range best {
		hit.Score = score[id]
		out = append(out, hit)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ID < out[j].ID
		}
		return out[i].Score > out[j].Score
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// flush writes messages and chunks, then records each message's content hash under
// dedupKey so unchanged messages are not re-embedded.
func (s *server) flush(messages, chunks []MemoryItem, dedupKey func(MemoryItem) string) error {
	if len(messages) == 0 && len(chunks) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.mem != nil {
		s.mem.upsert(messages, chunks)
	} else {
		if s.lance == nil {
			return errors.New("lance store is not initialized")
		}
		if err := s.lance.Upsert(context.Background(), messages, chunks); err != nil {
			slog.Error("lance write failed", "messages", len(messages), "chunks", len(chunks), "err", err)
			return err
		}
	}
	for _, message := range messages {
		s.dedup.Mark(dedupKey(message), message.Metadata.ContentHash)
	}
	if err := s.dedup.Save(); err != nil {
		return err
	}
	slog.Info("flushed lance batch", "messages", len(messages), "chunks", len(chunks))
	return nil
}

func validateMemoryItem(item MemoryItem, dims int) error {
	if item.ID == "" {
		return errors.New("id required")
	}
	if len(item.Vector) != dims {
		return fmt.Errorf("vector dimensions=%d want=%d", len(item.Vector), dims)
	}
	if item.Text == "" {
		return errors.New("text required")
	}
	if item.Metadata.Source == "" {
		return errors.New("metadata.source required")
	}
	if item.Metadata.ContainerID == "" {
		return errors.New("metadata.container_id required")
	}
	if item.Metadata.Timestamp == 0 {
		return errors.New("metadata.timestamp required")
	}
	return nil
}

func loadDedupState(path string) (*dedupState, error) {
	st := &dedupState{Path: path, Files: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Files == nil {
		st.Files = map[string]string{}
	}
	return st, nil
}

func (d *dedupState) Seen(filePath, fileHash string) bool {
	if filePath == "" || fileHash == "" {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.Files[filePath] == fileHash
}

func (d *dedupState) Mark(filePath, fileHash string) {
	if filePath == "" || fileHash == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Files[filePath] = fileHash
}

func (d *dedupState) RemoveHost(host string) int {
	prefix := host + "/"
	d.mu.Lock()
	defer d.mu.Unlock()
	removed := 0
	for filePath := range d.Files {
		if strings.HasPrefix(filePath, prefix) {
			delete(d.Files, filePath)
			removed++
		}
	}
	return removed
}

func (d *dedupState) Save() error {
	d.mu.RLock()
	if d.Path == "" {
		d.mu.RUnlock()
		return nil
	}
	b, err := json.MarshalIndent(d, "", "  ")
	d.mu.RUnlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(d.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".dedup-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, d.Path)
}

func parseS3URL(raw string) (bucket, prefix string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "s3" || u.Host == "" {
		return "", "", fmt.Errorf("invalid s3 url %q", raw)
	}
	return u.Host, strings.TrimPrefix(u.Path, "/"), nil
}

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errorJSON(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	var v int
	if _, err := fmt.Sscanf(os.Getenv(key), "%d", &v); err == nil {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	var v int64
	if _, err := fmt.Sscanf(os.Getenv(key), "%d", &v); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func int32Ptr(v int32) *int32 { return &v }
