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

const (
	// Short chat messages embed poorly one by one, so consecutive messages of a DM,
	// group or topic within telegramWindowGap seconds share one window, capped at
	// telegramWindowChars. Channel posts stand alone.
	telegramWindowGap   = 10 * 60
	telegramWindowChars = 1200
)

// chunkGroup is the text embedded into chunks: one message, or a Telegram window of
// consecutive messages. Its chunks are <id>#<i> with message_id = id.
type chunkGroup struct {
	id      string
	meta    Metadata
	text    string
	pending []MemoryItem // messages of the group that are new or changed
}

// chunkGroups returns the groups containing at least one pending message. Windows are
// rebuilt from every message of the object, so a new message re-embeds only the window
// it joins.
func chunkGroups(items []MemoryItem, pending func(MemoryItem) bool) []chunkGroup {
	groups := make([]chunkGroup, 0)
	open := map[string]int{} // container -> index of its open window in windows
	var windows []chunkGroup
	var lastTS []int64
	var authors []map[string]bool
	for _, item := range items {
		if item.Metadata.Source != "telegram" {
			if pending(item) {
				groups = append(groups, chunkGroup{id: item.ID, meta: item.Metadata, text: item.Text, pending: []MemoryItem{item}})
			}
			continue
		}
		line := telegramAuthor(item.Metadata) + ": " + item.Text
		if i, ok := open[item.Metadata.ContainerID]; ok && item.Metadata.ContainerKind != "channel" &&
			item.Metadata.Timestamp-lastTS[i] <= telegramWindowGap && len(windows[i].text)+1+len(line) <= telegramWindowChars {
			windows[i].text += "\n" + line
			lastTS[i] = item.Metadata.Timestamp
			authors[i][item.Metadata.AuthorID] = true
			if pending(item) {
				windows[i].pending = append(windows[i].pending, item)
			}
			continue
		}
		window := chunkGroup{id: item.ID, meta: item.Metadata, text: line}
		window.meta.ReplyTo, window.meta.Attrs = "", ""
		if pending(item) {
			window.pending = []MemoryItem{item}
		}
		open[item.Metadata.ContainerID] = len(windows)
		windows = append(windows, window)
		lastTS = append(lastTS, item.Metadata.Timestamp)
		authors = append(authors, map[string]bool{item.Metadata.AuthorID: true})
	}
	for i, window := range windows {
		if len(window.pending) == 0 {
			continue
		}
		if len(authors[i]) > 1 {
			window.meta.AuthorID, window.meta.AuthorName = "", ""
		}
		window.meta.ContentHash = hashText(window.text)
		groups = append(groups, window)
	}
	return groups
}

func telegramAuthor(m Metadata) string {
	if m.AuthorName != "" {
		return m.AuthorName
	}
	return m.AuthorID
}
