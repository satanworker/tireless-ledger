package main

import (
	"fmt"
	"strings"
)

// Contextual units: every record is embedded as "header\nbody". Metadata columns
// (chat title, session title, author) never reach the embedding by themselves, so
// the header and body carry them: where the record is from, who wrote it, the lines
// it follows or replies to, and for an agent's answer the question it answers.

const (
	// Earlier lines of the same chat within telegramLeadGap seconds lead into a
	// message, so "correct" or "they just provided the hype" keep their subject.
	telegramLeadGap   = 10 * 60
	telegramLeadLines = 2
	contextLineRunes  = 200
	questionRunes     = 300
)

// chunkGroup is one record's unit: the header repeated on every chunk and the body
// split into chunks <id>#<i> with message_id = id.
type chunkGroup struct {
	id      string
	meta    Metadata
	header  string
	text    string
	pending []MemoryItem // the record itself when new or changed
}

func (g chunkGroup) embedText() string { return joinHeader(g.header, g.text) }

func joinHeader(header, body string) string {
	if header == "" {
		return body
	}
	return header + "\n" + body
}

// chunkGroups builds every record's unit in object order and returns those whose
// unit is pending. A unit's hash covers its header and lead-in, so a renamed session
// or an edited earlier message re-embeds the units that quote it.
func chunkGroups(items []MemoryItem, pending func(item MemoryItem, unitHash string) bool) []chunkGroup {
	titles := map[string]string{}
	for _, item := range items {
		if item.Metadata.Kind == "title" {
			titles[item.Metadata.ContainerID] = sessionTitle(item.Text)
		}
	}
	lines := map[string]string{}      // telegram message id -> "Author: text"
	lead := map[string][]MemoryItem{} // telegram container -> its latest messages
	questions := map[string]string{}  // session -> latest user prompt
	groups := make([]chunkGroup, 0)
	for _, item := range items {
		meta := item.Metadata
		if meta.ContainerTitle == "" {
			meta.ContainerTitle = titles[meta.ContainerID]
		}
		var body []string
		switch {
		case meta.Source == "telegram":
			line := item.Text
			if author := telegramAuthor(meta); author != "" {
				line = author + ": " + line
			}
			if meta.ContainerKind != "channel" {
				quoted := false
				for _, prev := range lead[meta.ContainerID] {
					if meta.Timestamp-prev.Metadata.Timestamp <= telegramLeadGap {
						body = append(body, clipLine(lines[prev.ID], contextLineRunes))
						quoted = quoted || prev.ID == meta.ReplyTo
					}
				}
				if parent, ok := lines[meta.ReplyTo]; ok && !quoted {
					body = append([]string{"> " + clipLine(parent, contextLineRunes)}, body...)
				}
				recent := append(lead[meta.ContainerID], item)
				if len(recent) > telegramLeadLines {
					recent = recent[len(recent)-telegramLeadLines:]
				}
				lead[meta.ContainerID] = recent
			}
			lines[item.ID] = line
			body = append(body, line)
		case meta.Kind == "message" && meta.AuthorID == "user":
			questions[meta.ContainerID] = clipLine(item.Text, questionRunes)
			body = append(body, item.Text)
		case meta.Kind == "message" && meta.AuthorID == "assistant" && questions[meta.ContainerID] != "":
			body = append(body, "Re: "+questions[meta.ContainerID], item.Text)
		default:
			body = append(body, item.Text)
		}
		group := chunkGroup{id: item.ID, meta: meta, header: contextHeader(meta), text: strings.Join(body, "\n")}
		hash := hashText(group.embedText())
		if !pending(item, hash) {
			continue
		}
		group.meta.ContentHash = hash
		item.Metadata.ContentHash, item.Metadata.ContainerTitle = hash, meta.ContainerTitle
		group.pending = []MemoryItem{item}
		groups = append(groups, group)
	}
	return groups
}

var harnessNames = map[string]string{
	"claude": "Claude Code", "codex": "Codex", "pi": "Pi", "omp": "OMP", "grok": "Grok", "grokbot": "Grok bot",
}

var telegramKinds = map[string]string{"dm": "direct chat", "group": "group", "channel": "channel", "topic": "group topic"}

// contextHeader is the one line that says where a record is from.
func contextHeader(m Metadata) string {
	title := strings.TrimSpace(m.ContainerTitle)
	switch m.Source {
	case "telegram":
		kind := telegramKinds[m.ContainerKind]
		if kind == "" {
			kind = "chat"
		}
		if title == "" {
			return "Telegram " + kind
		}
		return fmt.Sprintf("Telegram %s %q", kind, title)
	case "crm":
		return fmt.Sprintf("CRM notes on %q", valueOr(title, m.ContainerID))
	}
	header := valueOr(harnessNames[m.Source], m.Source) + " session"
	if title != "" {
		header += fmt.Sprintf(" %q", title)
	}
	if m.Project != "" && m.Project != "unknown" {
		header += " in " + m.Project
	}
	return header
}

// sessionTitle strips titleText's "TITLE <source>: " marker.
func sessionTitle(text string) string {
	if strings.HasPrefix(text, "TITLE ") {
		if _, title, ok := strings.Cut(text, ": "); ok {
			return strings.TrimSpace(title)
		}
	}
	return strings.TrimSpace(text)
}

// clipLine folds text onto one line and cuts it to n runes.
func clipLine(text string, n int) string {
	line := strings.Join(strings.Fields(text), " ")
	if r := []rune(line); len(r) > n {
		return string(r[:n]) + "…"
	}
	return line
}

func telegramAuthor(m Metadata) string {
	if m.AuthorName != "" {
		return m.AuthorName
	}
	return m.AuthorID
}
