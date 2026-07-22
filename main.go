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
	defaultBatchThreshold = 500
	defaultQueueDepth     = 10000
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
	QueryVector []float32 `json:"query_vector"`
	Scope       string    `json:"scope"`
	ProjectName string    `json:"project_name"`
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

type vectorSearchRequest struct {
	Vector        []float32    `json:"vector"`
	K             int          `json:"k"`
	Filter        vectorFilter `json:"filter,omitempty"`
	IncludeFields []string     `json:"includeFields,omitempty"`
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
	queue      chan MemoryItem
	vectorProc *exec.Cmd
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
	QueueDepth     int
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

	if err := validateS3(context.Background(), cfg); err != nil {
		slog.Error("s3 validation failed", "err", err)
		os.Exit(1)
	}

	if err := writeVectorConfig(cfg); err != nil {
		slog.Error("write vector config", "err", err)
		os.Exit(1)
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
		queue:  make(chan MemoryItem, cfg.QueueDepth),
	}

	if cfg.StartVector {
		if err := s.startVector(); err != nil {
			slog.Error("start opendata vector", "err", err)
			os.Exit(1)
		}
		defer s.stopVector()
	}

	go s.writerLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/ready", s.ready)
	mux.HandleFunc("POST /v1/memory/ingest", s.ingest)
	mux.HandleFunc("POST /v1/memory/query", s.query)

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
	flag.IntVar(&cfg.QueueDepth, "queue-depth", envInt("PI_MEMORYD_QUEUE_DEPTH", defaultQueueDepth), "ingest FIFO depth")
	flag.StringVar(&cfg.StatePath, "state", env("PI_MEMORYD_STATE", "./data/dedup_state.json"), "dedup state path")
	flag.StringVar(&cfg.ConfigPath, "vector-config", env("PI_MEMORYD_VECTOR_CONFIG", "./data/vector.yaml"), "generated OpenData Vector config path")
	flag.StringVar(&cfg.VectorBinary, "vector-binary", env("PI_MEMORYD_VECTOR_BINARY", "opendata-vector"), "OpenData Vector binary")
	flag.BoolVar(&cfg.StartVector, "start-vector", envBool("PI_MEMORYD_START_VECTOR", false), "spawn OpenData Vector process")
	flag.BoolVar(&cfg.DryRunS3, "dry-run-s3", envBool("PI_MEMORYD_DRY_RUN_S3", false), "skip AWS SDK S3 validation")
	flag.Parse()
	return cfg
}

func validateS3(ctx context.Context, cfg runtimeConfig) error {
	bucket, prefix, err := parseS3URL(cfg.StorageURL)
	if err != nil {
		return err
	}
	if cfg.DryRunS3 {
		return nil
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
  - name: forward_content
    field_type: String
    indexed: false
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
	for i, rec := range req.Records {
		if err := validateMemoryItem(rec, s.cfg.Dimensions); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("records[%d]: %v", i, err))
			continue
		}
		if s.dedup.Seen(rec.Metadata.FilePath, rec.Metadata.FileHash) {
			resp.Skipped++
			continue
		}
		select {
		case s.queue <- rec:
			resp.Accepted++
		default:
			resp.Errors = append(resp.Errors, "ingest queue full")
		}
	}
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
	if len(req.QueryVector) != s.cfg.Dimensions {
		errorJSON(w, http.StatusBadRequest, fmt.Errorf("query_vector dimensions=%d want=%d", len(req.QueryVector), s.cfg.Dimensions))
		return
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}
	searchReq := vectorSearchRequest{
		Vector:        req.QueryVector,
		K:             req.Limit,
		IncludeFields: []string{"forward_content", "scope", "project_name", "file_path", "file_hash", "timestamp"},
		Filter: vectorFilter{And: []vectorFilter{
			{Eq: &comparisonFilter{Field: "scope", Value: req.Scope}},
			{Eq: &comparisonFilter{Field: "project_name", Value: req.ProjectName}},
		}},
	}
	var searchResp vectorSearchResponse
	if err := s.postVector("/api/v1/vector/search", searchReq, &searchResp); err != nil {
		errorJSON(w, http.StatusBadGateway, err)
		return
	}
	out := queryResponse{Results: make([]queryResult, 0, len(searchResp.Results))}
	for _, res := range searchResp.Results {
		attrs := res.Vector.Attributes
		content, _ := attrs["forward_content"].(string)
		delete(attrs, "vector")
		out.Results = append(out.Results, queryResult{ID: res.Vector.ID, Score: res.Score, ForwardContent: content, Metadata: attrs})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) writerLoop() {
	batch := make([]MemoryItem, 0, s.cfg.BatchThreshold)
	timer := time.NewTimer(time.Duration(s.cfg.FlushSeconds) * time.Second)
	defer timer.Stop()
	for {
		select {
		case item := <-s.queue:
			batch = append(batch, item)
			if len(batch) >= s.cfg.BatchThreshold {
				s.flushBatch(batch)
				batch = batch[:0]
				resetTimer(timer, time.Duration(s.cfg.FlushSeconds)*time.Second)
			}
		case <-timer.C:
			if len(batch) > 0 {
				s.flushBatch(batch)
				batch = batch[:0]
			}
			resetTimer(timer, time.Duration(s.cfg.FlushSeconds)*time.Second)
		}
	}
}

func (s *server) flushBatch(items []MemoryItem) {
	if len(items) == 0 {
		return
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
		}})
	}
	var resp map[string]interface{}
	if err := s.postVector("/api/v1/vector/write", writeReq, &resp); err != nil {
		slog.Error("vector write failed", "records", len(items), "err", err)
		return
	}
	for _, item := range items {
		s.dedup.Mark(item.Metadata.FilePath, item.Metadata.FileHash)
	}
	if err := s.dedup.Save(); err != nil {
		slog.Error("save dedup state failed", "err", err)
	}
	slog.Info("flushed memory batch", "records", len(items))
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
		return errors.New(er.Message)
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

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
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
