package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

type MemoryScope string

const (
	ScopeCode     MemoryScope = "code_memory"
	ScopeProject  MemoryScope = "project_memory"
	ScopeSession  MemoryScope = "session_memory"
	ScopeUser     MemoryScope = "user_memory"
	ScopeDecision MemoryScope = "decision_memory"
)

type Metadata struct {
	Scope       MemoryScope `json:"scope"`
	ProjectName string      `json:"project_name"`
	FilePath    string      `json:"file_path,omitempty"`
	FileHash    string      `json:"file_hash"`
	Timestamp   int64       `json:"timestamp"`
	SessionID   string      `json:"session_id,omitempty"`
	Host        string      `json:"host,omitempty"`
	Harness     string      `json:"harness,omitempty"`
	Role        string      `json:"role,omitempty"`
}

type MemoryItem struct {
	ID             string    `json:"id"`
	Vector         []float32 `json:"vector"`
	ForwardContent string    `json:"forward_content"`
	Metadata       Metadata  `json:"metadata"`
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
	QueryVector []float32 `json:"query_vector,omitempty"`
	QueryText   string    `json:"query_text,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	ProjectName string    `json:"project_name,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Host        string    `json:"host,omitempty"`
	Harness     string    `json:"harness,omitempty"`
	Limit       int       `json:"limit"`
}

type queryResult struct {
	ID             string                 `json:"id"`
	Score          float64                `json:"score"`
	ForwardContent string                 `json:"forward_content"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

type queryResponse struct {
	Results []queryResult `json:"results"`
}

type bm25Query struct {
	Field string `json:"field"`
	Query string `json:"query"`
}

type vectorSearchRequest struct {
	Vector        []float32     `json:"vector,omitempty"`
	BM25          *bm25Query    `json:"bm25,omitempty"`
	K             int           `json:"k"`
	Filter        *vectorFilter `json:"filter,omitempty"`
	IncludeFields []string      `json:"includeFields,omitempty"`
	AfterTS       int64         `json:"after_ts,omitempty"`
	AfterID       string        `json:"after_id,omitempty"`
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
	Files map[string]string `json:"files"` // file_path -> file_hash
}

type server struct {
	cfg        runtimeConfig
	dedup      *dedupState
	writeMu    sync.Mutex
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
	StatePath         string
	DryRunS3          bool
	Optimize          bool
	CreateVectorIndex bool
	DropVectorIndex   bool
	AuditDuplicates   bool
	DeleteHost        string
	RawURL            string
	RawPollInterval   time.Duration
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
	Upsert(context.Context, []MemoryItem) error
	Search(context.Context, vectorSearchRequest) ([]queryResult, error)
	Optimize(context.Context) error
	CreateVectorIndex(context.Context) error
	DropVectorIndex(context.Context) error
	AuditDuplicates(context.Context) (duplicateAudit, error)
	DeleteHost(context.Context, string) (int, error)
}

func main() {
	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
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
		cfg:   cfg,
		dedup: state,
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
			_, err := s.lance.Search(ctx, vectorSearchRequest{
				BM25: &bm25Query{Field: "forward_content", Query: "warmup"},
				K:    1,
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
		if cfg.Optimize || cfg.CreateVectorIndex || cfg.DropVectorIndex || cfg.AuditDuplicates || cfg.DeleteHost != "" {
			return
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/ready", s.ready)
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)
	mux.HandleFunc("GET /v1/memory/session", s.sessionWalk)
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
	flag.IntVar(&cfg.VectorNProbes, "vector-nprobes", envInt("PI_MEMORYD_VECTOR_NPROBES", 64), "IVF partitions scanned per vector query")
	flag.StringVar(&cfg.StatePath, "state", env("PI_MEMORYD_STATE", "./data/dedup_state.json"), "dedup state path")
	flag.BoolVar(&cfg.DryRunS3, "dry-run-s3", envBool("PI_MEMORYD_DRY_RUN_S3", false), "skip AWS SDK S3 validation")
	flag.BoolVar(&cfg.Optimize, "optimize", false, "compact data, refresh indexes, prune old versions, and exit")
	flag.BoolVar(&cfg.CreateVectorIndex, "create-vector-index", false, "create a 64-partition IVF-Flat vector index and exit")
	flag.BoolVar(&cfg.DropVectorIndex, "drop-vector-index", false, "drop the IVF-Flat vector index and exit")
	flag.BoolVar(&cfg.AuditDuplicates, "audit-duplicates", false, "scan all rows for duplicate IDs and exit non-zero if any exist")
	flag.StringVar(&cfg.DeleteHost, "delete-host", "", "delete every row and dedup entry for exactly this host, then exit")
	flag.StringVar(&cfg.RawURL, "raw-url", env("PI_MEMORYD_RAW_URL", ""), "optional S3 prefix containing raw session files")
	flag.DurationVar(&cfg.RawPollInterval, "raw-poll-interval", envDuration("PI_MEMORYD_RAW_POLL_INTERVAL", 30*time.Second), "raw S3 polling interval")
	flag.Int64Var(&cfg.RawMaxObjectBytes, "raw-max-object-bytes", envInt64("PI_MEMORYD_RAW_MAX_OBJECT_BYTES", 128<<20), "largest raw object accepted")
	flag.StringVar(&cfg.EmbedURL, "embed-url", env("PI_MEMORYD_EMBED_URL", "http://llama-embed:8091"), "OpenAI-compatible embedding server")
	flag.IntVar(&cfg.EmbedBatchSize, "embed-batch-size", envInt("PI_MEMORYD_EMBED_BATCH_SIZE", 32), "texts per embedding request")
	flag.Parse()
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
	todo := make([]MemoryItem, 0, len(req.Records))
	pending := make(map[string]string, len(req.Records))
	for i, rec := range req.Records {
		if err := validateMemoryItem(rec, s.cfg.Dimensions); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("records[%d]: %v", i, err))
			continue
		}
		if s.dedup.Seen(rec.Metadata.FilePath, rec.Metadata.FileHash) {
			resp.Skipped++
			continue
		}
		if hash, ok := pending[rec.ID]; ok {
			if hash != rec.Metadata.FileHash {
				resp.Errors = append(resp.Errors, fmt.Sprintf("records[%d]: id %q has conflicting content", i, rec.ID))
				continue
			}
			resp.Skipped++
			continue
		}
		pending[rec.ID] = rec.Metadata.FileHash
		todo = append(todo, rec)
	}
	if err := s.flushBatch(todo); err != nil {
		resp.Errors = append(resp.Errors, err.Error())
		writeJSON(w, http.StatusBadGateway, resp)
		return
	}
	resp.Accepted = len(todo)
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
	include := []string{"forward_content", "scope", "project_name", "file_path", "file_hash", "timestamp", "session_id", "host", "harness", "role"}
	fetchK := req.Limit
	if hasVec && hasText && fetchK < 20 {
		fetchK = 20
	}
	if s.mem != nil {
		var lists [][]queryResult
		if hasVec {
			lists = append(lists, s.mem.search(vectorSearchRequest{Vector: req.QueryVector, K: fetchK, Filter: filter}))
		}
		if hasText {
			lists = append(lists, s.mem.search(vectorSearchRequest{BM25: &bm25Query{Field: "forward_content", Query: req.QueryText}, K: fetchK, Filter: filter}))
		}
		out := queryResponse{Results: lists[0]}
		if len(lists) == 2 {
			out.Results = rrfMerge(lists[0], lists[1], req.Limit)
		} else if len(out.Results) > req.Limit {
			out.Results = out.Results[:req.Limit]
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	searchReq := vectorSearchRequest{K: fetchK, Filter: filter, IncludeFields: include}
	if hasVec {
		searchReq.Vector = req.QueryVector
	}
	if hasText {
		searchReq.BM25 = &bm25Query{Field: "forward_content", Query: req.QueryText}
	}
	hits, err := s.vectorSearch(searchReq)
	if err != nil {
		errorJSON(w, http.StatusBadGateway, err)
		return
	}
	out := queryResponse{Results: hits}
	if len(out.Results) > req.Limit {
		out.Results = out.Results[:req.Limit]
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) sessionWalk(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := strings.TrimSpace(q.Get("session_id"))
	if sid == "" {
		errorJSON(w, http.StatusBadRequest, errors.New("session_id required"))
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
	harness := strings.TrimSpace(q.Get("harness"))
	if s.mem != nil {
		writeJSON(w, http.StatusOK, queryResponse{Results: s.mem.bySession(sid, host, harness, afterTS, afterID, limit)})
		return
	}
	filter := sessionWalkFilter(sid, host, harness)
	hits, err := s.vectorSearch(vectorSearchRequest{
		K:             limit,
		Filter:        filter,
		AfterTS:       afterTS,
		AfterID:       afterID,
		IncludeFields: []string{"forward_content", "scope", "project_name", "file_path", "file_hash", "timestamp", "session_id", "host", "harness", "role"},
	})
	if err != nil {
		errorJSON(w, http.StatusBadGateway, err)
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

func sessionWalkFilter(sid, host, harness string) *vectorFilter {
	parts := []vectorFilter{{Eq: &comparisonFilter{Field: "session_id", Value: sid}}}
	if host != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "host", Value: host}})
	}
	if harness != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "harness", Value: harness}})
	}
	if len(parts) == 1 {
		return &parts[0]
	}
	return &vectorFilter{And: parts}
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

func (s *server) vectorSearch(searchReq vectorSearchRequest) ([]queryResult, error) {
	if s.mem != nil {
		return s.mem.search(searchReq), nil
	}
	if s.lance == nil {
		return nil, errors.New("lance store is not initialized")
	}
	return s.lance.Search(context.Background(), searchReq)
}

func queryFilter(req queryRequest) *vectorFilter {
	var parts []vectorFilter
	if req.Scope != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "scope", Value: req.Scope}})
	}
	if req.ProjectName != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "project_name", Value: req.ProjectName}})
	}
	if req.SessionID != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "session_id", Value: req.SessionID}})
	}
	if req.Host != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "host", Value: req.Host}})
	}
	if req.Harness != "" {
		parts = append(parts, vectorFilter{Eq: &comparisonFilter{Field: "harness", Value: req.Harness}})
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

func (s *server) flushBatch(items []MemoryItem) error {
	if len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.mem != nil {
		s.mem.upsert(items)
		for _, item := range items {
			s.dedup.Mark(item.Metadata.FilePath, item.Metadata.FileHash)
		}
		return s.dedup.Save()
	}
	if s.lance == nil {
		return errors.New("lance store is not initialized")
	}
	if err := s.lance.Upsert(context.Background(), items); err != nil {
		slog.Error("lance write failed", "records", len(items), "err", err)
		return err
	}
	for _, item := range items {
		s.dedup.Mark(item.Metadata.FilePath, item.Metadata.FileHash)
	}
	if err := s.dedup.Save(); err != nil {
		return err
	}
	slog.Info("flushed lance batch", "records", len(items))
	return nil
}

func validateMemoryItem(item MemoryItem, dims int) error {
	if item.ID == "" {
		return errors.New("id required")
	}
	if len(item.Vector) != dims {
		return fmt.Errorf("vector dimensions=%d want=%d", len(item.Vector), dims)
	}
	if item.ForwardContent == "" {
		return errors.New("forward_content required")
	}
	if !validScope(item.Metadata.Scope) {
		return fmt.Errorf("invalid scope %q", item.Metadata.Scope)
	}
	if item.Metadata.ProjectName == "" {
		return errors.New("metadata.project_name required")
	}
	if item.Metadata.FileHash == "" {
		return errors.New("metadata.file_hash required")
	}
	if item.Metadata.Timestamp == 0 {
		return errors.New("metadata.timestamp required")
	}
	return nil
}

func validScope(scope MemoryScope) bool {
	switch scope {
	case ScopeCode, ScopeProject, ScopeSession, ScopeUser, ScopeDecision:
		return true
	default:
		return false
	}
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

func safeID(id string) string {
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
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
