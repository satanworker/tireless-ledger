package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const minChars = 8

var skipUser = []string{"# AGENTS.md", "<permissions instructions>", "<INSTRUCTIONS>"}

type Turn struct {
	ID        string
	Text      string
	Project   string
	TS        int64
	Container string
	Host      string
	Source    string
	Author    string
}

func recordID(host, source, container, native string) string {
	return host + "/" + source + "/" + container + "/" + native
}

func textHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func projectFromCWD(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if cwd == "" {
		return "unknown"
	}
	base := filepath.Base(cwd)
	if base == "" || base == "." || base == "/" {
		return "unknown"
	}
	return base
}

func parseTS(v any) int64 {
	switch t := v.(type) {
	case float64:
		n := int64(t)
		if n > 10_000_000_000 {
			return n / 1000
		}
		return n
	case json.Number:
		n, _ := t.Int64()
		if n > 10_000_000_000 {
			return n / 1000
		}
		return n
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if strings.HasSuffix(s, "Z") {
			s = strings.TrimSuffix(s, "Z") + "+00:00"
		}
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			tm, err = time.Parse(time.RFC3339, s)
		}
		if err != nil {
			return 0
		}
		return tm.Unix()
	default:
		return 0
	}
}

func partsText(content any) string {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var b strings.Builder
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "thinking" || typ == "reasoning" {
				continue
			}
			if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(strings.TrimSpace(t))
			}
		}
		return b.String()
	default:
		return ""
	}
}

func skipUserText(text string) bool {
	head := strings.TrimLeft(text, " \t")
	for _, p := range skipUser {
		if strings.HasPrefix(head, p) {
			return true
		}
	}
	return false
}

func makeTurn(host, source, container, nativeID, author, text string, ts int64, project string) Turn {
	if ts == 0 {
		ts = 1
	}
	return Turn{
		ID:        recordID(host, source, container, nativeID),
		Text:      text,
		Project:   project,
		TS:        ts,
		Container: container,
		Host:      host,
		Source:    source,
		Author:    author,
	}
}

func parseLine(raw []byte, st *fileState) (Turn, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Turn{}, false
	}
	if st.Source == "pi" || st.Source == "omp" {
		return parsePi(obj, st)
	}
	return parseCodex(obj, st)
}

type fileState struct {
	Source    string
	Host      string
	Container string
	Project   string
	Line      int
	Seen      map[string]struct{}
}

func parsePi(obj map[string]any, st *fileState) (Turn, bool) {
	typ, _ := obj["type"].(string)
	if typ == "session" {
		if id, _ := obj["id"].(string); id != "" {
			st.Container = id
		}
		if cwd, _ := obj["cwd"].(string); cwd != "" {
			st.Project = projectFromCWD(cwd)
		}
		return Turn{}, false
	}
	if typ != "message" {
		return Turn{}, false
	}
	msg, _ := obj["message"].(map[string]any)
	role, _ := msg["role"].(string)
	if role != "user" && role != "assistant" {
		return Turn{}, false
	}
	text := partsText(msg["content"])
	if len(text) < minChars || (role == "user" && skipUserText(text)) {
		return Turn{}, false
	}
	turnID, _ := obj["id"].(string)
	if turnID == "" {
		return Turn{}, false
	}
	ts := parseTS(obj["timestamp"])
	if ts == 0 {
		ts = parseTS(msg["timestamp"])
	}
	return makeTurn(st.Host, st.Source, st.Container, turnID, role, text, ts, st.Project), true
}

func parseCodex(obj map[string]any, st *fileState) (Turn, bool) {
	typ, _ := obj["type"].(string)
	pl, _ := obj["payload"].(map[string]any)
	if typ == "session_meta" {
		if id, _ := pl["id"].(string); id != "" {
			st.Container = id
		}
		if cwd, _ := pl["cwd"].(string); cwd != "" {
			st.Project = projectFromCWD(cwd)
		}
		return Turn{}, false
	}
	if typ != "response_item" {
		return Turn{}, false
	}
	if pt, _ := pl["type"].(string); pt != "message" {
		return Turn{}, false
	}
	role, _ := pl["role"].(string)
	if role != "user" && role != "assistant" {
		return Turn{}, false
	}
	text := partsText(pl["content"])
	if len(text) < minChars || (role == "user" && skipUserText(text)) {
		return Turn{}, false
	}
	sourceID, _ := pl["id"].(string)
	key := strings.TrimSpace(sourceID)
	if key == "" {
		key = textHash(text)[:16]
	}
	if st.Seen == nil {
		st.Seen = map[string]struct{}{}
	}
	if _, ok := st.Seen[key]; ok {
		return Turn{}, false
	}
	st.Seen[key] = struct{}{}
	return makeTurn(st.Host, "codex", st.Container, key, role, text, parseTS(obj["timestamp"]), st.Project), true
}

// ReadTurns parses complete JSONL lines. Trailing partial line is not consumed.
func ReadTurns(r io.Reader, st *fileState) (turns []Turn, consumed int64, err error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	last := bytes.LastIndexByte(b, '\n')
	if last < 0 {
		return nil, 0, nil
	}
	complete := b[:last+1]
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		if t, ok := parseLine(line, st); ok {
			turns = append(turns, t)
		}
	}
	return turns, int64(len(complete)), nil
}
