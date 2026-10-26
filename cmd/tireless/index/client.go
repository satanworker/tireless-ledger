package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	QueryPrefix = "Represent this sentence for searching relevant passages: "
	Dims        = 384
	PostBatch   = 32
	// bge-small-en-v1.5 n_ctx=512; llama.cpp adds CLS/SEP.
	embedMax      = 448
	embedMaxChars = 800
)

func clipChars(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type Client struct {
	Server   string
	EmbedURL string
	HTTP     *http.Client
	Host     string
	DryRun   bool
}

func NewClient(server, embed, host string) *Client {
	return &Client{
		Server:   server,
		EmbedURL: embed,
		Host:     host,
		HTTP:     &http.Client{Timeout: 120 * time.Second},
	}
}

type embedResp struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (c *Client) Tokenize(text string) ([]int, error) {
	body, _ := json.Marshal(map[string]any{"content": text, "add_special": false})
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := c.post(c.EmbedURL+"/tokenize", body, &out); err != nil {
		return nil, err
	}
	return out.Tokens, nil
}

func (c *Client) Detokenize(ids []int) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	body, _ := json.Marshal(map[string]any{"tokens": ids})
	var out struct {
		Content string `json:"content"`
	}
	if err := c.post(c.EmbedURL+"/detokenize", body, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

func (c *Client) Embed(texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"input": texts, "model": "bge-small-en-v1.5"})
	var out embedResp
	if err := c.post(c.EmbedURL+"/v1/embeddings", body, &out); err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(texts))
	for _, row := range out.Data {
		if row.Index < 0 || row.Index >= len(texts) {
			continue
		}
		vecs[row.Index] = row.Embedding
	}
	for i, v := range vecs {
		if len(v) != Dims {
			return nil, fmt.Errorf("embed[%d] dims=%d want=%d", i, len(v), Dims)
		}
	}
	return vecs, nil
}

func (c *Client) clipTokens(text string, max int) (string, error) {
	ids, err := c.Tokenize(text)
	if err != nil {
		return "", err
	}
	if max <= 0 || len(ids) <= max {
		return text, nil
	}
	return c.Detokenize(ids[:max])
}

type ingestReq struct {
	Records []map[string]any `json:"records"`
}

type IngestResp struct {
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

func (c *Client) Ingest(turns []Turn) (IngestResp, error) {
	var zero IngestResp
	if len(turns) == 0 || c.DryRun {
		zero.Accepted = len(turns)
		return zero, nil
	}
	texts := make([]string, len(turns))
	for i, t := range turns {
		texts[i] = clipChars(t.Text, embedMaxChars)
	}
	vecs := make([][]float32, len(texts))
	for i := 0; i < len(texts); i += PostBatch {
		end := i + PostBatch
		if end > len(texts) {
			end = len(texts)
		}
		part, err := c.Embed(texts[i:end])
		if err != nil {
			return zero, err
		}
		copy(vecs[i:], part)
	}
	recs := make([]map[string]any, len(turns))
	for i, t := range turns {
		recs[i] = map[string]any{
			"id":     t.ID,
			"vector": vecs[i],
			"text":   t.Text,
			"metadata": map[string]any{
				"source":       t.Source,
				"host":         t.Host,
				"container_id": t.Container,
				"timestamp":    t.TS,
				"project":      t.Project,
				"author_id":    t.Author,
				"kind":         "message",
			},
		}
	}
	body, _ := json.Marshal(ingestReq{Records: recs})
	if err := c.post(c.Server+"/v1/memory/ingest", body, &zero); err != nil {
		return zero, err
	}
	return zero, nil
}

// Query filters: container_id, project, source, kind (empty = unset).
func (c *Client) Query(text string, embed bool, filters map[string]string, limit int) (json.RawMessage, error) {
	q := map[string]any{"limit": limit}
	for k, v := range filters {
		if v != "" {
			q[k] = v
		}
	}
	if text != "" {
		q["query_text"] = text
	}
	if embed || text == "" {
		qtext, err := c.clipTokens(QueryPrefix+text, embedMax)
		if err != nil {
			return nil, err
		}
		vecs, err := c.Embed([]string{qtext})
		if err != nil {
			return nil, err
		}
		q["query_vector"] = vecs[0]
	}
	body, _ := json.Marshal(q)
	req, err := http.NewRequest(http.MethodPost, c.Server+"/v1/memory/query", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, c.Server+"/v1/memory/query", b)
	}
	return b, nil
}

// Container walks one container. params: container_id, host, source, after_ts, after_id, include (empty = unset).
func (c *Client) Container(params map[string]string, limit int) (json.RawMessage, error) {
	u, err := url.Parse(c.Server + "/v1/memory/container")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()
	resp, err := c.HTTP.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return b, nil
}

func (c *Client) post(url string, body []byte, out any) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, url, b)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
