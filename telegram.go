package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// chatSources are the chat platforms. Their objects share one line format and code
// path; the platform is only the record's source. Add a platform here.
var chatSources = map[string]bool{"telegram": true, "x": true}

func isChatSource(source string) bool { return chatSources[source] }

// chatLine is one exported message version, written to
// <host>/<source>/<chat_id>/<yyyy-mm>.jsonl (cmd/tireless-telegram for Telegram, the
// Mac's export for X). An edit appends a new line with the same id; the last line wins.
type chatLine struct {
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
	// Extra fields (X export). Decoded loosely so a type surprise keeps the line.
	SenderHandle  interface{} `json:"sender_handle"`
	Encrypted     interface{} `json:"encrypted"`
	Verified      interface{} `json:"verified"`
	MessageID     interface{} `json:"message_id"`
	SourceAccount interface{} `json:"source_account"`
}

func parseChatJSONL(body []byte, host, source string) ([]MemoryItem, error) {
	index := map[string]int{}
	items := make([]MemoryItem, 0)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	for scanner.Scan() {
		var line chatLine
		if json.Unmarshal(bytes.TrimSpace(scanner.Bytes()), &line) != nil || line.ID == "" || line.ChatID == "" {
			continue
		}
		container := line.ChatID
		if line.TopicID != "" {
			container += ":" + line.TopicID
		}
		prefix := host + "/" + source + "/" + container + "/"
		text := strings.TrimSpace(line.Text)
		ts := line.Date
		if ts == 0 {
			ts = 1
		}
		item := MemoryItem{
			ID:   prefix + line.ID,
			Text: text,
			Metadata: Metadata{
				Source: source, Host: host, ContainerID: container, ContainerKind: line.ChatType,
				ContainerTitle: line.ChatTitle, AuthorID: line.SenderID, AuthorName: line.SenderName,
				Kind: "message", Timestamp: ts, Attrs: chatAttrs(line), ContentHash: hashText(text),
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

func chatAttrs(line chatLine) string {
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
	for key, v := range map[string]interface{}{"sender_handle": line.SenderHandle, "encrypted": line.Encrypted,
		"verified": line.Verified, "message_id": line.MessageID, "source_account": line.SourceAccount} {
		if v != nil && v != "" {
			attrs[key] = v
		}
	}
	if len(attrs) == 0 {
		return ""
	}
	b, _ := json.Marshal(attrs)
	return string(b)
}
