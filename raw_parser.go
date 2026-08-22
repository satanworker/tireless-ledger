package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const rawParserVersion = "session-jsonl-v1"

var skippedUserPrefixes = []string{"# AGENTS.md", "<permissions instructions>", "<INSTRUCTIONS>"}

func parseRawSession(key string, body []byte) ([]MemoryItem, error) {
	host, harness, ok := rawObjectIdentity(key)
	if !ok {
		return nil, fmt.Errorf("object key must be <host>/<pi|codex>/<path>.jsonl")
	}
	return parseSessionJSONL(body, host, harness, key)
}

func rawObjectIdentity(key string) (host, harness string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(key, "/"), "/")
	if len(parts) < 3 || parts[0] == "" || (parts[1] != "pi" && parts[1] != "codex") || !strings.HasSuffix(strings.ToLower(parts[len(parts)-1]), ".jsonl") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseSessionJSONL(body []byte, host, harness, source string) ([]MemoryItem, error) {
	sessionID := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	project := "unknown"
	seen := map[string]bool{}
	items := make([]MemoryItem, 0)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var obj map[string]interface{}
		if json.Unmarshal(line, &obj) != nil {
			continue
		}
		if harness == "pi" {
			if stringField(obj, "type") == "session" {
				sessionID = valueOr(stringField(obj, "id"), sessionID)
				project = projectFromCWD(stringField(obj, "cwd"))
				continue
			}
			if stringField(obj, "type") != "message" {
				continue
			}
			msg, _ := obj["message"].(map[string]interface{})
			role := stringField(msg, "role")
			text := contentText(msg["content"])
			turnID := stringField(obj, "id")
			if !indexableMessage(role, text) || turnID == "" {
				continue
			}
			items = append(items, rawTurn(host, harness, sessionID, turnID, role, text, parseRawTimestamp(firstValue(obj["timestamp"], msg["timestamp"])), project))
			continue
		}

		payload, _ := obj["payload"].(map[string]interface{})
		if stringField(obj, "type") == "session_meta" {
			sessionID = valueOr(stringField(payload, "id"), sessionID)
			project = projectFromCWD(stringField(payload, "cwd"))
			continue
		}
		if stringField(obj, "type") != "response_item" || stringField(payload, "type") != "message" {
			continue
		}
		role := stringField(payload, "role")
		text := contentText(payload["content"])
		if !indexableMessage(role, text) {
			continue
		}
		sourceID := strings.TrimSpace(stringField(payload, "id"))
		key := sourceID
		if key == "" {
			key = "legacy-" + hashText(text)[:16]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		turnID := sourceID
		if turnID == "" {
			turnID = fmt.Sprintf("line-%d", lineNo-1)
		}
		items = append(items, rawTurn(host, harness, sessionID, turnID, role, text, parseRawTimestamp(obj["timestamp"]), project))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}
	return items, nil
}

func rawTurn(host, harness, sessionID, turnID, role, text string, timestamp int64, project string) MemoryItem {
	if timestamp == 0 {
		timestamp = 1
	}
	return MemoryItem{
		ID:             hashText(host + "|" + harness + "|" + sessionID + "|" + turnID),
		ForwardContent: text,
		Metadata: Metadata{
			Scope: ScopeSession, ProjectName: project, FilePath: host + "/" + harness + "/" + sessionID + "/" + turnID,
			FileHash: hashText(text), Timestamp: timestamp, SessionID: sessionID, Host: host, Harness: harness, Role: role,
		},
	}
}

func contentText(v interface{}) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	parts, ok := v.([]interface{})
	if !ok {
		return ""
	}
	var out []string
	for _, part := range parts {
		m, ok := part.(map[string]interface{})
		if !ok || stringField(m, "type") == "thinking" || stringField(m, "type") == "reasoning" {
			continue
		}
		if text := strings.TrimSpace(stringField(m, "text")); text != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, "\n")
}

func indexableMessage(role, text string) bool {
	if (role != "user" && role != "assistant") || len(text) < 8 {
		return false
	}
	if role == "user" {
		head := strings.TrimLeft(text, " \t\r\n")
		for _, prefix := range skippedUserPrefixes {
			if strings.HasPrefix(head, prefix) {
				return false
			}
		}
	}
	return true
}

func projectFromCWD(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return "unknown"
	}
	name := filepath.Base(strings.TrimRight(cwd, "/"))
	if name == "." || name == "/" || name == "" {
		return "unknown"
	}
	return name
}

func parseRawTimestamp(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		if n > 10_000_000_000 {
			return int64(n) / 1000
		}
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		if i > 10_000_000_000 {
			return i / 1000
		}
		return i
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func firstValue(values ...interface{}) interface{} {
	for _, v := range values {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}
func hashText(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
