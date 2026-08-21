package main

import (
	"bytes"
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
	"os/exec"
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
	defaultListenAddr     = ":8090"
	defaultVectorURL      = "http://127.0.0.1:8080"
	defaultRegion         = "eu-west-1"
	defaultStorageURL     = "s3://YOUR_PRODUCTION_BUCKET_NAME/vector-index"
	defaultFlushSeconds   = 300
	defaultBatchThreshold = 32
	defaultHTTPClientTO   = 60 * time.Second
	contentTypeProtoJSON  = "application/protobuf+json"
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

type vectorWriteRequest struct {
	UpsertVectors []vectorRecord `json:"upsertVectors"`
}

type vectorRecord struct {
	ID         string                 `json:"id"`
	Attributes map[string]interface{} `json:"attributes"`
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

type vectorSearchResponse struct {
	Status  string `json:"status"`
	Results []struct {
		Score  float64      `json:"score"`
		Vector vectorRecord `json:"vector"`
	} `json:"results"`
	Message string `json:"message,omitempty"`
}

type dedupState struct {
	mu    sync.RWMutex
	Path  string            `json:"-"`
	Files map[string]string `json:"files"` // file_path -> file_hash
}

type server struct {
	cfg        runtimeConfig
	client     *http.Client
	dedup      *dedupState
	writeMu    sync.Mutex
	vectorProc *exec.Cmd
	mem        *memStore
}

type runtimeConfig struct {
	ListenAddr     string
	VectorURL      string
	StorageURL     string
	AWSRegion      string
	S3Endpoint     string
	Dimensions     int
	DistanceMetric string
	FlushSeconds   int
	BatchThreshold int
	StatePath      string
	ConfigPath     string
	VectorBinary   string
	StartVector    bool
	DryRunS3       bool
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
	if !local {
		if err := writeVectorConfig(cfg); err != nil {
			slog.Error("write vector config", "err", err)
			os.Exit(1)
		}
	}

	state, err := loadDedupState(cfg.StatePath)
	if err != nil {
		slog.Error("load dedup state", "err", err)
		os.Exit(1)
	}

	s := &server{
		cfg:    cfg,
		client: &http.Client{Timeout: defaultHTTPClientTO},
		dedup:  state,
	}
	if local {
		s.mem = newMemStore(cfg.Dimensions)
		slog.Info("using in-memory vector store (memory://)")
	}

	if cfg.StartVector && s.mem == nil {
		if err := s.startVector(); err != nil {
			slog.Error("start opendata vector", "err", err)
			os.Exit(1)
		}
		defer s.stopVector()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/ready", s.ready)
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)
	mux.HandleFunc("GET /v1/memory/session", s.sessionWalk)

	slog.Info("pi-memoryd listening", "addr", cfg.ListenAddr, "vector_url", cfg.VectorURL, "storage_url", cfg.StorageURL)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		slog.Error("http server stopped", "err", err)
		os.Exit(1)
	}
}

func loadConfig() runtimeConfig {
	cfg := runtimeConfig{}
	flag.StringVar(&cfg.ListenAddr, "listen", env("PI_MEMORYD_LISTEN", defaultListenAddr), "HTTP listen address")
	flag.StringVar(&cfg.VectorURL, "vector-url", env("PI_MEMORYD_VECTOR_URL", defaultVectorURL), "OpenData Vector HTTP URL")
	flag.StringVar(&cfg.StorageURL, "storage-url", env("PI_MEMORYD_STORAGE_URL", defaultStorageURL), "S3 URL, e.g. s3://bucket/vector-index")
	flag.StringVar(&cfg.AWSRegion, "aws-region", env("AWS_REGION", env("AWS_DEFAULT_REGION", defaultRegion)), "AWS region")
	flag.StringVar(&cfg.S3Endpoint, "s3-endpoint", env("PI_MEMORYD_S3_ENDPOINT", env("AWS_ENDPOINT_URL", env("AWS_ENDPOINT", ""))), "S3-compatible endpoint URL")
	flag.IntVar(&cfg.Dimensions, "dimensions", envInt("PI_MEMORYD_DIMENSIONS", 384), "vector dimensions")
	flag.StringVar(&cfg.DistanceMetric, "distance", env("PI_MEMORYD_DISTANCE", "L2"), "OpenData distance metric")
	flag.IntVar(&cfg.FlushSeconds, "flush-seconds", envInt("PI_MEMORYD_FLUSH_SECONDS", defaultFlushSeconds), "writer debounce flush window")
	flag.IntVar(&cfg.BatchThreshold, "batch-threshold", envInt("PI_MEMORYD_BATCH_THRESHOLD", defaultBatchThreshold), "writer batch row threshold")
	flag.StringVar(&cfg.StatePath, "state", env("PI_MEMORYD_STATE", "./data/dedup_state.json"), "dedup state path")
	flag.StringVar(&cfg.ConfigPath, "vector-config", env("PI_MEMORYD_VECTOR_CONFIG", "./data/vector.yaml"), "generated OpenData Vector config path")
	flag.StringVar(&cfg.VectorBinary, "vector-binary", env("PI_MEMORYD_VECTOR_BINARY", "opendata-vector"), "OpenData Vector binary")
	flag.BoolVar(&cfg.StartVector, "start-vector", envBool("PI_MEMORYD_START_VECTOR", false), "spawn OpenData Vector process")
	flag.BoolVar(&cfg.DryRunS3, "dry-run-s3", envBool("PI_MEMORYD_DRY_RUN_S3", false), "skip AWS SDK S3 validation")
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

func writeVectorConfig(cfg runtimeConfig) error {
	bucket, prefix, err := parseS3URL(cfg.StorageURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ConfigPath), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`storage:
  type: SlateDb
  path: %s
  object_store:
    type: Aws
    region: %s
    bucket: %s
dimensions: %d
distance_metric: %s
flush_interval: %d
metadata_fields:
  - name: scope
    field_type: String
    indexed: true
  - name: project_name
    field_type: String
    indexed: true
  - name: file_path
    field_type: String
    indexed: true
  - name: file_hash
    field_type: String
    indexed: true
  - name: timestamp
    field_type: Int64
    indexed: false
  - name: session_id
    field_type: String
    indexed: true
  - name: host
    field_type: String
    indexed: true
  - name: harness
    field_type: String
    indexed: true
  - name: role
    field_type: String
    indexed: true
  - name: forward_content
    field_type: Text
    indexed: true
`, prefix, cfg.AWSRegion, bucket, cfg.Dimensions, cfg.DistanceMetric, cfg.FlushSeconds)
	return os.WriteFile(cfg.ConfigPath, []byte(body), 0o600)
}

func (s *server) startVector() error {
	u, err := url.Parse(s.cfg.VectorURL)
	if err != nil {
		return err
	}
	port := u.Port()
	if port == "" {
		port = "8080"
	}
	s.vectorProc = exec.Command(s.cfg.VectorBinary, "--port", port, "vector", "--config", s.cfg.ConfigPath)
	s.vectorProc.Stdout = os.Stdout
	s.vectorProc.Stderr = os.Stderr
	return s.vectorProc.Start()
}

func (s *server) stopVector() {
	if s.vectorProc != nil && s.vectorProc.Process != nil {
		_ = s.vectorProc.Process.Kill()
	}
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
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
	for i, rec := range req.Records {
		if err := validateMemoryItem(rec, s.cfg.Dimensions); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("records[%d]: %v", i, err))
			continue
		}
		if s.dedup.Seen(rec.Metadata.FilePath, rec.Metadata.FileHash) {
			resp.Skipped++
			continue
		}
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
	var searchResp vectorSearchResponse
	if err := s.postVector("/api/v1/vector/search", searchReq, &searchResp); err != nil {
		return nil, err
	}
	out := make([]queryResult, 0, len(searchResp.Results))
	for _, res := range searchResp.Results {
		attrs := res.Vector.Attributes
		if attrs == nil {
			attrs = map[string]interface{}{}
		}
		content, _ := attrs["forward_content"].(string)
		delete(attrs, "vector")
		delete(attrs, "forward_content")
		out = append(out, queryResult{ID: res.Vector.ID, Score: res.Score, ForwardContent: content, Metadata: attrs})
	}
	return out, nil
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
	n := s.cfg.BatchThreshold
	if n <= 0 {
		n = defaultBatchThreshold
	}
	for i := 0; i < len(items); i += n {
		end := i + n
		if end > len(items) {
			end = len(items)
		}
		if err := s.writeVectors(items[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func tooBig(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "413") || strings.Contains(msg, "Payload Too Large") || strings.Contains(msg, "broken pipe")
}

func (s *server) writeVectors(items []MemoryItem) error {
	if len(items) == 0 {
		return nil
	}
	writeReq := vectorWriteRequest{UpsertVectors: make([]vectorRecord, 0, len(items))}
	for _, item := range items {
		writeReq.UpsertVectors = append(writeReq.UpsertVectors, vectorRecord{ID: safeID(item.ID), Attributes: map[string]interface{}{
			"vector":          item.Vector,
			"forward_content": item.ForwardContent,
			"scope":           string(item.Metadata.Scope),
			"project_name":    item.Metadata.ProjectName,
			"file_path":       item.Metadata.FilePath,
			"file_hash":       item.Metadata.FileHash,
			"timestamp":       item.Metadata.Timestamp,
			"session_id":      item.Metadata.SessionID,
			"host":            item.Metadata.Host,
			"harness":         item.Metadata.Harness,
			"role":            item.Metadata.Role,
		}})
	}
	var resp map[string]interface{}
	if err := s.postVector("/api/v1/vector/write", writeReq, &resp); err != nil {
		if len(items) > 1 && tooBig(err) {
			mid := len(items) / 2
			if e := s.writeVectors(items[:mid]); e != nil {
				return e
			}
			return s.writeVectors(items[mid:])
		}
		slog.Error("vector write failed", "records", len(items), "err", err)
		return err
	}
	for _, item := range items {
		s.dedup.Mark(item.Metadata.FilePath, item.Metadata.FileHash)
	}
	if err := s.dedup.Save(); err != nil {
		return err
	}
	slog.Info("flushed memory batch", "records", len(items))
	return nil
}

func (s *server) postVector(path string, in, out interface{}) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := s.client.Post(strings.TrimRight(s.cfg.VectorURL, "/")+path, contentTypeProtoJSON, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var er struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&er)
		if er.Message == "" {
			er.Message = resp.Status
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, er.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
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

func (d *dedupState) Save() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.Path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.Path, b, 0o600)
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
