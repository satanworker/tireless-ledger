package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	embedChunkTokens  = 384
	embedChunkOverlap = 64
)

type rawObject struct {
	Key, ETag string
	Size      int64
}

type rawObjectStore interface {
	List(context.Context) ([]rawObject, error)
	Get(context.Context, string, int64) ([]byte, error)
}

type rawS3Store struct {
	client         *s3.Client
	bucket, prefix string
}

func newRawS3Store(ctx context.Context, cfg runtimeConfig) (*rawS3Store, error) {
	bucket, prefix, err := parseS3URL(cfg.RawURL)
	if err != nil {
		return nil, err
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
			o.UsePathStyle = true
		}
	})
	return &rawS3Store{client: client, bucket: bucket, prefix: strings.Trim(prefix, "/")}, nil
}

func (s *rawS3Store) List(ctx context.Context) ([]rawObject, error) {
	prefix := s.prefix
	if prefix != "" {
		prefix += "/"
	}
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	var out []rawObject
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(obj.Key), prefix)
			if _, _, ok := rawObjectIdentity(key); !ok {
				continue
			}
			out = append(out, rawObject{Key: key, ETag: strings.Trim(aws.ToString(obj.ETag), "\""), Size: aws.ToInt64(obj.Size)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *rawS3Store) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	fullKey := key
	if s.prefix != "" {
		fullKey = s.prefix + "/" + key
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &fullKey})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("object exceeds %d bytes", limit)
	}
	return b, nil
}

type rawRegistryEntry struct {
	ETag          string    `json:"etag"`
	Size          int64     `json:"size"`
	Status        string    `json:"status"`
	ParserVersion string    `json:"parser_version,omitempty"`
	Records       int       `json:"records,omitempty"`
	Attempts      int       `json:"attempts,omitempty"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type rawRegistry struct {
	mu      sync.RWMutex
	Path    string                      `json:"-"`
	Objects map[string]rawRegistryEntry `json:"objects"`
}

func loadRawRegistry(path string) (*rawRegistry, error) {
	r := &rawRegistry{Path: path, Objects: map[string]rawRegistryEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, err
	}
	if r.Objects == nil {
		r.Objects = map[string]rawRegistryEntry{}
	}
	return r, nil
}

func (r *rawRegistry) Done(obj rawObject) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.Objects[obj.Key]
	return ok && e.Status == "indexed" && e.ETag == obj.ETag && e.Size == obj.Size && e.ParserVersion == rawParserVersion
}

func (r *rawRegistry) Due(obj rawObject, now time.Time, writeInterval time.Duration) bool {
	r.mu.RLock()
	e, ok := r.Objects[obj.Key]
	r.mu.RUnlock()
	if !ok || e.ParserVersion != rawParserVersion {
		return true
	}
	if e.ETag != obj.ETag || e.Size != obj.Size {
		// Growing rollout files may change every minute. Coalesce those
		// versions so a handful of new turns do not create a new Lance
		// fragment and invalidate caches on every raw-object scan.
		if e.Status == "indexed" && writeInterval > 0 && now.Before(e.UpdatedAt.Add(writeInterval)) {
			return false
		}
		return true
	}
	if e.Status == "indexed" {
		return false
	}
	if e.Status != "failed" {
		return true
	}
	delay := 30 * time.Second
	for i := 1; i < e.Attempts && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return !now.Before(e.UpdatedAt.Add(delay))
}

func (r *rawRegistry) set(obj rawObject, status string, records int, setErr error) error {
	r.mu.Lock()
	entry := r.Objects[obj.Key]
	if entry.ETag != obj.ETag || entry.Size != obj.Size || entry.ParserVersion != rawParserVersion {
		entry.Attempts = 0
		entry.Records = 0
	}
	entry.ETag, entry.Size, entry.Status, entry.ParserVersion = obj.ETag, obj.Size, status, rawParserVersion
	entry.Records, entry.UpdatedAt = records, time.Now().UTC()
	if status == "processing" {
		entry.Attempts++
	}
	entry.Error = ""
	if setErr != nil {
		entry.Error = setErr.Error()
	}
	r.Objects[obj.Key] = entry
	r.mu.Unlock()
	return r.Save()
}

func (r *rawRegistry) Save() error {
	r.mu.RLock()
	b, err := json.MarshalIndent(r, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.Path), ".raw-registry-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
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
	return os.Rename(tmpName, r.Path)
}

func (r *rawRegistry) RemoveHost(host string) int {
	prefix := host + "/"
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	for key := range r.Objects {
		if strings.HasPrefix(key, prefix) {
			delete(r.Objects, key)
			removed++
		}
	}
	return removed
}

type rawStatusResponse struct {
	Enabled       bool      `json:"enabled"`
	Indexed       int       `json:"indexed"`
	Failed        int       `json:"failed"`
	Processing    int       `json:"processing"`
	LastScan      time.Time `json:"last_scan,omitempty"`
	LastScanError string    `json:"last_scan_error,omitempty"`
}

type rawIngestor struct {
	server        *server
	store         rawObjectStore
	registry      *rawRegistry
	http          *http.Client
	mu            sync.RWMutex
	lastScan      time.Time
	lastScanError string
}

func newRawIngestor(ctx context.Context, server *server) (*rawIngestor, error) {
	store, err := newRawS3Store(ctx, server.cfg)
	if err != nil {
		return nil, err
	}
	registry, err := loadRawRegistry(filepath.Join(filepath.Dir(server.cfg.StatePath), "raw_registry.json"))
	if err != nil {
		return nil, err
	}
	return &rawIngestor{server: server, store: store, registry: registry, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (r *rawIngestor) Run(ctx context.Context) {
	interval := r.server.cfg.RawPollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		r.scan(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *rawIngestor) scan(ctx context.Context) {
	objects, err := r.store.List(ctx)
	r.mu.Lock()
	r.lastScan = time.Now().UTC()
	r.lastScanError = ""
	if err != nil {
		r.lastScanError = err.Error()
	}
	r.mu.Unlock()
	if err != nil {
		slog.Error("raw S3 scan failed", "err", err)
		return
	}
	for _, obj := range objects {
		if ctx.Err() != nil {
			return
		}
		if !r.registry.Due(obj, time.Now(), r.server.cfg.RawWriteInterval) {
			continue
		}
		if err := r.process(ctx, obj); err != nil {
			_ = r.registry.set(obj, "failed", 0, err)
			slog.Error("raw object ingestion failed", "key", obj.Key, "err", err)
		}
	}
}

func (r *rawIngestor) process(ctx context.Context, obj rawObject) error {
	if err := r.registry.set(obj, "processing", 0, nil); err != nil {
		return fmt.Errorf("save processing state: %w", err)
	}
	if obj.Size > r.server.cfg.RawMaxObjectBytes {
		return fmt.Errorf("object is %d bytes; limit is %d", obj.Size, r.server.cfg.RawMaxObjectBytes)
	}
	body, err := r.store.Get(ctx, obj.Key, r.server.cfg.RawMaxObjectBytes)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	items, err := parseRawSession(obj.Key, body)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	pending := items[:0]
	for _, item := range items {
		if !r.server.dedup.Seen(rawDerivedPath(item.Metadata.FilePath), item.Metadata.FileHash) {
			pending = append(pending, item)
		}
	}
	messageBatch := r.server.cfg.EmbedBatchSize
	if messageBatch <= 0 {
		messageBatch = 32
	}
	for start := 0; start < len(pending); start += messageBatch {
		end := start + messageBatch
		if end > len(pending) {
			end = len(pending)
		}
		if err := r.indexMessages(ctx, pending[start:end]); err != nil {
			return err
		}
	}
	if err := r.registry.set(obj, "indexed", len(items), nil); err != nil {
		return fmt.Errorf("save indexed state: %w", err)
	}
	slog.Info("raw object indexed", "key", obj.Key, "records", len(items), "new_records", len(pending))
	return nil
}

func rawDerivedPath(filePath string) string {
	return filePath + "|" + rawParserVersion
}

func (r *rawIngestor) indexMessages(ctx context.Context, messages []MemoryItem) error {
	type prepared struct {
		message MemoryItem
		chunks  []MemoryItem
	}
	preparedMessages := make([]prepared, 0, len(messages))
	flat := make([]MemoryItem, 0, len(messages))
	for _, message := range messages {
		texts, err := r.splitTokenChunks(ctx, message.ForwardContent)
		if err != nil {
			return fmt.Errorf("tokenize message %s: %w", message.ID, err)
		}
		chunks := make([]MemoryItem, len(texts))
		for i, text := range texts {
			chunk := message
			chunk.ID = hashText(message.ID + "|token-chunk-v1|" + strconv.Itoa(i))
			chunk.ForwardContent = text
			chunk.Metadata.RecordKind = "chunk"
			chunk.Metadata.ParentID = message.ID
			chunk.Metadata.ChunkIndex = int64(i)
			chunk.Metadata.ChunkCount = int64(len(texts))
			chunks[i] = chunk
			flat = append(flat, chunk)
		}
		message.Metadata.RecordKind = "message"
		message.Metadata.ParentID = message.ID
		message.Metadata.ChunkCount = int64(len(chunks))
		preparedMessages = append(preparedMessages, prepared{message: message, chunks: chunks})
	}
	texts := make([]string, len(flat))
	for i := range flat {
		texts[i] = flat[i].ForwardContent
	}
	vectors := make([][]float32, 0, len(texts))
	batchSize := r.server.cfg.EmbedBatchSize
	if batchSize <= 0 {
		batchSize = 32
	}
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := r.embedTexts(ctx, texts[start:end])
		if err != nil {
			return err
		}
		vectors = append(vectors, batch...)
	}
	for i := range flat {
		flat[i].Vector = vectors[i]
	}
	records := make([]MemoryItem, 0, len(messages)+len(flat))
	flatIndex := 0
	for i := range preparedMessages {
		count := len(preparedMessages[i].chunks)
		preparedMessages[i].message.Vector = flat[flatIndex].Vector
		records = append(records, preparedMessages[i].message)
		records = append(records, flat[flatIndex:flatIndex+count]...)
		flatIndex += count
	}
	if err := r.server.flushDerived(messages, records); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

func (r *rawIngestor) embedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	payload, _ := json.Marshal(map[string]interface{}{"input": texts, "model": "bge-small-en-v1.5"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.server.cfg.EmbedURL, "/")+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embed HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	vectors := make([][]float32, len(texts))
	for _, row := range out.Data {
		if row.Index >= 0 && row.Index < len(vectors) {
			vectors[row.Index] = row.Embedding
		}
	}
	for i, vector := range vectors {
		if len(vector) != r.server.cfg.Dimensions {
			return nil, fmt.Errorf("embedding[%d] dimensions=%d want=%d", i, len(vector), r.server.cfg.Dimensions)
		}
	}
	return vectors, nil
}

func (r *rawIngestor) tokenCount(ctx context.Context, text string) (int, error) {
	payload, _ := json.Marshal(map[string]interface{}{"content": text, "add_special": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.server.cfg.EmbedURL, "/")+"/tokenize", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("tokenize request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("tokenize HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Tokens []json.RawMessage `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode tokens: %w", err)
	}
	return len(out.Tokens), nil
}

func (r *rawIngestor) splitTokenChunks(ctx context.Context, text string) ([]string, error) {
	count, err := r.tokenCount(ctx, text)
	if err != nil {
		return nil, err
	}
	if count <= embedChunkTokens {
		return []string{text}, nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, count/(embedChunkTokens-embedChunkOverlap)+1)
	start := 0
	for start < len(runes) {
		remaining := string(runes[start:])
		remainingCount, err := r.tokenCount(ctx, remaining)
		if err != nil {
			return nil, err
		}
		if remainingCount <= embedChunkTokens {
			chunks = append(chunks, remaining)
			break
		}
		lo, hi := start+1, len(runes)
		best := start + 1
		for lo <= hi {
			mid := lo + (hi-lo)/2
			n, err := r.tokenCount(ctx, string(runes[start:mid]))
			if err != nil {
				return nil, err
			}
			if n <= embedChunkTokens {
				best = mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		end := preferTextBoundary(runes, start, best)
		chunks = append(chunks, string(runes[start:end]))
		overlapStart := end
		lo, hi = start, end
		for lo <= hi {
			mid := lo + (hi-lo)/2
			n, err := r.tokenCount(ctx, string(runes[mid:end]))
			if err != nil {
				return nil, err
			}
			if n <= embedChunkOverlap {
				overlapStart = mid
				hi = mid - 1
			} else {
				lo = mid + 1
			}
		}
		if overlapStart <= start {
			overlapStart = end
		}
		start = overlapStart
	}
	return chunks, nil
}

func preferTextBoundary(runes []rune, start, end int) int {
	minimum := start + (end-start)*3/4
	for i := end - 1; i >= minimum; i-- {
		if runes[i] == '\n' {
			return i + 1
		}
	}
	return end
}

func (s *server) rawStatus(w http.ResponseWriter, _ *http.Request) {
	if s.raw == nil {
		writeJSON(w, http.StatusOK, rawStatusResponse{})
		return
	}
	status := rawStatusResponse{Enabled: true}
	s.raw.registry.mu.RLock()
	for _, entry := range s.raw.registry.Objects {
		switch entry.Status {
		case "indexed":
			status.Indexed++
		case "failed":
			status.Failed++
		case "processing":
			status.Processing++
		}
	}
	s.raw.registry.mu.RUnlock()
	s.raw.mu.RLock()
	status.LastScan, status.LastScanError = s.raw.lastScan, s.raw.lastScanError
	s.raw.mu.RUnlock()
	writeJSON(w, http.StatusOK, status)
}
