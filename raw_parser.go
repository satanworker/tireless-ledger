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

const rawParserVersion = "session-jsonl-v3-important-state"

var skippedUserPrefixes = []string{"# AGENTS.md", "<permissions instructions>", "<INSTRUCTIONS>"}

func parseRawSession(key string, body []byte) ([]MemoryItem, error) {
	host, harness, ok := rawObjectIdentity(key)
	if !ok {
		return nil, fmt.Errorf("object key must be <host>/<pi|codex|omp>/<path>.jsonl")
	}
	return parseSessionJSONL(body, host, harness, key, false)
}

func parseRawSessionForIndex(key string, body []byte) ([]MemoryItem, error) {
	host, harness, ok := rawObjectIdentity(key)
	if !ok {
		return nil, fmt.Errorf("object key must be <host>/<pi|codex|omp>/<path>.jsonl")
	}
	return parseSessionJSONL(body, host, harness, key, true)
}

func rawObjectIdentity(key string) (host, harness string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(key, "/"), "/")
	if len(parts) < 3 || parts[0] == "" || (parts[1] != "pi" && parts[1] != "codex" && parts[1] != "omp") || !strings.HasSuffix(strings.ToLower(parts[len(parts)-1]), ".jsonl") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type pendingRawState struct {
	id        string
	role      string
	text      string
	timestamp int64
	kind      string
}

func parseSessionJSONL(body []byte, host, harness, source string, includeState bool) ([]MemoryItem, error) {
	sessionID := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	project := "unknown"
	seen := map[string]bool{}
	items := make([]MemoryItem, 0)
	pendingState := make([]pendingRawState, 0)
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
		if harness == "pi" || harness == "omp" {
			entryID := valueOr(stringField(obj, "id"), fmt.Sprintf("line-%d", lineNo))
			switch stringField(obj, "type") {
			case "session":
				sessionID = valueOr(stringField(obj, "id"), sessionID)
				project = projectFromCWD(stringField(obj, "cwd"))
				for _, pending := range pendingState {
					items = append(items, rawRecord(host, harness, sessionID, pending.id, pending.role, pending.text, pending.timestamp, project, pending.kind))
				}
				pendingState = pendingState[:0]
				if includeState {
					if text := titleText(obj, "session"); text != "" {
						items = append(items, rawRecord(host, harness, sessionID, entryID+"-session", "system", text, parseRawTimestamp(firstValue(obj["updatedAt"], obj["timestamp"])), project, "title"))
					}
				}
				continue
			case "title", "title_change":
				if includeState {
					if text := titleText(obj, stringField(obj, "type")); text != "" {
						ts := parseRawTimestamp(firstValue(obj["updatedAt"], obj["timestamp"]))
						if project == "unknown" {
							pendingState = append(pendingState, pendingRawState{id: entryID, role: "system", text: text, timestamp: ts, kind: "title"})
						} else {
							items = append(items, rawRecord(host, harness, sessionID, entryID, "system", text, ts, project, "title"))
						}
					}
				}
				continue
			case "custom":
				if includeState && stringField(obj, "customType") == "user_todo_edit" {
					if text := todoTextFromSessionRecord(obj, "user_todo_edit"); text != "" {
						items = append(items, rawRecord(host, harness, sessionID, entryID, "user_todo_edit", text, parseRawTimestamp(obj["timestamp"]), project, "todo"))
					}
				}
				continue
			case "message":
				msg, _ := obj["message"].(map[string]interface{})
				role := stringField(msg, "role")
				if includeState && role == "toolResult" {
					toolName := stringField(msg, "toolName")
					switch toolName {
					case "todo":
						if text := todoTextFromSessionRecord(msg, "todo"); text != "" {
							items = append(items, rawRecord(host, harness, sessionID, entryID, role, text, parseRawTimestamp(firstValue(obj["timestamp"], msg["timestamp"])), project, "todo"))
						}
					case "task":
						if text := toolResultText(msg, "task"); text != "" {
							items = append(items, rawRecord(host, harness, sessionID, entryID, role, text, parseRawTimestamp(firstValue(obj["timestamp"], msg["timestamp"])), project, "task"))
						}
					}
					continue
				}
				text := contentText(msg["content"])
				if !indexableMessage(role, text) || entryID == "" {
					continue
				}
				items = append(items, rawRecord(host, harness, sessionID, entryID, role, text, parseRawTimestamp(firstValue(obj["timestamp"], msg["timestamp"])), project, "message"))
				continue
			default:
				continue
			}
		}

		payload, _ := obj["payload"].(map[string]interface{})
		if stringField(obj, "type") == "session_meta" {
			sessionID = valueOr(stringField(payload, "id"), sessionID)
			project = projectFromCWD(stringField(payload, "cwd"))
			if includeState {
				if text := titleText(payload, "session"); text != "" {
					items = append(items, rawRecord(host, harness, sessionID, stringField(payload, "id")+"-session", "system", text, parseRawTimestamp(obj["timestamp"]), project, "title"))
				}
			}
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
		items = append(items, rawRecord(host, harness, sessionID, turnID, role, text, parseRawTimestamp(obj["timestamp"]), project, "message"))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}
	return items, nil
}

func rawTurn(host, harness, sessionID, turnID, role, text string, timestamp int64, project string) MemoryItem {
	return rawRecord(host, harness, sessionID, turnID, role, text, timestamp, project, "message")
}

func rawRecord(host, harness, sessionID, turnID, role, text string, timestamp int64, project, kind string) MemoryItem {
	if timestamp == 0 {
		timestamp = 1
	}
	idInput := host + "|" + harness + "|" + sessionID + "|" + turnID
	filePath := host + "/" + harness + "/" + sessionID + "/" + turnID
	if kind != "message" {
		idInput = host + "|" + harness + "|" + sessionID + "|" + kind + "|" + turnID
		filePath = host + "/" + harness + "/" + sessionID + "/" + kind + "/" + turnID
	}
	return MemoryItem{
		ID:             hashText(idInput),
		ForwardContent: text,
		Metadata: Metadata{
			Scope: ScopeSession, ProjectName: project, FilePath: filePath,
			FileHash: hashText(text), Timestamp: timestamp, SessionID: sessionID, Host: host, Harness: harness, Role: role,
			RecordKind: kind,
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

func titleText(m map[string]interface{}, source string) string {
	title := strings.TrimSpace(firstStringField(m, "title", "name", "summary"))
	if title == "" {
		return ""
	}
	return "TITLE " + source + ": " + title
}

func toolResultText(m map[string]interface{}, toolName string) string {
	text := contentText(m["content"])
	if text == "" {
		text = compactJSON(firstValue(m["details"], m["data"], m["result"]))
	}
	if text == "" {
		return ""
	}
	return "TOOL " + toolName + " RESULT:\n" + text
}

func todoTextFromSessionRecord(m map[string]interface{}, source string) string {
	for _, candidate := range []interface{}{m["details"], m["data"], m} {
		cm, ok := candidate.(map[string]interface{})
		if !ok {
			continue
		}
		state := firstValue(cm["phases"], cm["items"], cm["todos"], cm["tasks"])
		if state == nil {
			continue
		}
		text := compactJSON(state)
		if text != "" {
			return "TODO " + source + ":\n" + text
		}
	}
	text := compactJSON(firstValue(m["details"], m["data"]))
	if text == "" {
		text = contentText(m["content"])
	}
	if text == "" {
		return ""
	}
	return "TODO " + source + ":\n" + text
}

func compactJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return ""
	}
	return string(b)
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

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringField(m, key)); value != "" {
			return value
		}
	}
	return ""
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
