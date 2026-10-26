package main

import (
	"strings"
	"testing"
)

func TestTelegramParseAndWindows(t *testing.T) {
	lines := []string{
		`{"id":"1","chat_id":"-100","chat_type":"group","chat_title":"Core","date":1000,"sender_id":"a","sender_name":"Alice","text":"pushed v0.3 to staging"}`,
		`{"id":"2","chat_id":"-100","chat_type":"group","chat_title":"Core","date":1060,"sender_id":"b","sender_name":"Bob","reply_to":"1","text":"on it"}`,
		`{"id":"1","chat_id":"-100","chat_type":"group","chat_title":"Core","date":1000,"edit_date":1100,"sender_id":"a","sender_name":"Alice","text":"pushed v0.3.1 to staging"}`,
		`{"id":"3","chat_id":"-100","chat_type":"group","chat_title":"Core","date":5000,"sender_id":"a","sender_name":"Alice","text":"next day"}`,
		`{"id":"4","chat_id":"-100","chat_type":"topic","topic_id":"7","date":1070,"sender_id":"c","sender_name":"Carol","text":"topic talk"}`,
		`{"id":"5","chat_id":"-100","chat_type":"group","date":1080,"sender_id":"a","text":""}`,
	}
	items, err := parseTelegramJSONL([]byte(strings.Join(lines, "\n")), "tgbox")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]MemoryItem{}
	for _, item := range items {
		byID[item.ID] = item
	}
	if len(items) != 4 {
		t.Fatalf("items=%+v", items)
	}
	first := byID["tgbox/telegram/-100/1"]
	if first.Text != "pushed v0.3.1 to staging" || first.Metadata.Attrs != `{"edit_date":1100}` {
		t.Fatalf("edit did not win: %+v", first)
	}
	if got := byID["tgbox/telegram/-100/2"].Metadata.ReplyTo; got != "tgbox/telegram/-100/1" {
		t.Fatalf("reply_to=%q", got)
	}
	if topic, ok := byID["tgbox/telegram/-100:7/4"]; !ok || topic.Metadata.ContainerID != "-100:7" || topic.Metadata.ContainerKind != "topic" {
		t.Fatalf("topic=%+v", byID)
	}

	all := chunkGroups(items, func(MemoryItem) bool { return true })
	windows := map[string]chunkGroup{}
	for _, g := range all {
		windows[g.id] = g
	}
	core := windows["tgbox/telegram/-100/1"]
	if len(all) != 3 || core.text != "Alice: pushed v0.3.1 to staging\nBob: on it" || len(core.pending) != 2 || core.meta.AuthorID != "" {
		t.Fatalf("windows=%+v", all)
	}
	if _, ok := windows["tgbox/telegram/-100/3"]; !ok {
		t.Fatalf("message after the gap must open a new window: %+v", all)
	}

	// Only the window holding a changed message is re-embedded.
	changed := chunkGroups(items, func(item MemoryItem) bool { return item.ID == "tgbox/telegram/-100/2" })
	if len(changed) != 1 || changed[0].id != "tgbox/telegram/-100/1" || len(changed[0].pending) != 1 {
		t.Fatalf("changed=%+v", changed)
	}

	channel := []MemoryItem{
		{ID: "c/1", Text: "post one", Metadata: Metadata{Source: "telegram", ContainerID: "-200", ContainerKind: "channel", Timestamp: 10}},
		{ID: "c/2", Text: "post two", Metadata: Metadata{Source: "telegram", ContainerID: "-200", ContainerKind: "channel", Timestamp: 11}},
	}
	if posts := chunkGroups(channel, func(MemoryItem) bool { return true }); len(posts) != 2 {
		t.Fatalf("channel posts must stand alone: %+v", posts)
	}
}
