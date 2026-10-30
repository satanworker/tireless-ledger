package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// telegramLine is one exported message version, written by cmd/tireless-telegram to
// <host>/telegram/<chat_id>/<yyyy-mm>.jsonl. An edit appends a new line with the same
// id; the last line wins.
type telegramLine struct {
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	ChatType   string `json:"chat_type"`
	ChatTitle  string `json:"chat_title"`
	TopicID    string `json:"topic_id"`
	Date       int64  `json:"date"`
	EditDate   int64  `json:"edit_date"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	ReplyTo    string `json:"reply_to"`
	FwdFrom    string `json:"fwd_from"`
	Media      string `json:"media"`
	Text       string `json:"text"`
}

func parseTelegramJSONL(body []byte, host string) ([]MemoryItem, error) {
	index := map[string]int{}
	items := make([]MemoryItem, 0)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	for scanner.Scan() {
		var line telegramLine
		if json.Unmarshal(bytes.TrimSpace(scanner.Bytes()), &line) != nil || line.ID == "" || line.ChatID == "" {
			continue
		}
		container := line.ChatID
		if line.TopicID != "" {
			container += ":" + line.TopicID
		}
		prefix := host + "/telegram/" + container + "/"
		text := strings.TrimSpace(line.Text)
		ts := line.Date
		if ts == 0 {
			ts = 1
		}
		item := MemoryItem{
			ID:   prefix + line.ID,
			Text: text,
			Metadata: Metadata{
				Source: "telegram", Host: host, ContainerID: container, ContainerKind: line.ChatType,
				ContainerTitle: line.ChatTitle, AuthorID: line.SenderID, AuthorName: line.SenderName,
				Kind: "message", Timestamp: ts, Attrs: telegramAttrs(line), ContentHash: hashText(text),
			},
		}
		if line.ReplyTo != "" {
			item.Metadata.ReplyTo = prefix + line.ReplyTo
		}
		if i, ok := index[item.ID]; ok {
			items[i] = item
			continue
		}
		index[item.ID] = len(items)
		items = append(items, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}
	// An edit that removed the text (media-only) drops the message.
	kept := items[:0]
	for _, item := range items {
		if item.Text != "" {
			kept = append(kept, item)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Metadata.Timestamp < kept[j].Metadata.Timestamp })
	return kept, nil
}

func telegramAttrs(line telegramLine) string {
	attrs := map[string]interface{}{}
	if line.FwdFrom != "" {
		attrs["fwd_from"] = line.FwdFrom
	}
	if line.EditDate != 0 {
		attrs["edit_date"] = line.EditDate
	}
	if line.Media != "" {
		attrs["media"] = line.Media
	}
	if len(attrs) == 0 {
		return ""
	}
	b, _ := json.Marshal(attrs)
	return string(b)
}
