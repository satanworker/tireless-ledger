package main

import (
	"fmt"
	"strings"
)

var lanceStringFields = map[string]bool{
	"source": true, "host": true, "container_id": true, "container_kind": true, "project": true,
	"author_id": true, "kind": true, "message_id": true,
}

func lanceQuote(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "''") + "'"
}

func lanceFilterSQL(filter *vectorFilter) string {
	if filter == nil {
		return ""
	}
	if len(filter.And) > 0 {
		parts := make([]string, 0, len(filter.And))
		for i := range filter.And {
			if part := lanceFilterSQL(&filter.And[i]); part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "(" + strings.Join(parts, " AND ") + ")"
	}
	if filter.Eq == nil || !lanceStringFields[filter.Eq.Field] || filter.Eq.Value == nil {
		return ""
	}
	return filter.Eq.Field + " = " + lanceQuote(fmt.Sprint(filter.Eq.Value))
}

func lanceCursorSQL(afterTS int64, afterID string) string {
	switch {
	case afterTS <= 0 && afterID == "":
		return ""
	case afterTS <= 0:
		return "id > " + lanceQuote(afterID)
	case afterID == "":
		return fmt.Sprintf("timestamp > %d", afterTS)
	default:
		return fmt.Sprintf("(timestamp > %d OR (timestamp = %d AND id > %s))", afterTS, afterTS, lanceQuote(afterID))
	}
}

func lanceCombineSQL(parts ...string) string {
	nonempty := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, " AND ")
}

// v4MessageID maps a v4 file_path to the readable message id the raw parser emits
// (rawRecord) and its kind: <host>/<harness>/<session>/<turn> and
// <host>/<harness>/<session>/message/<turn> are the message <host>/<harness>/<session>/<turn>;
// <host>/<harness>/<session>/<kind>/<turn> keeps its path as a title/todo/task id.
// Rows without a path (direct HTTP ingest) keep their v4 id.
func v4MessageID(filePath, v4ID string) (string, string) {
	if filePath == "" {
		return v4ID, "message"
	}
	if parts := strings.Split(filePath, "/"); len(parts) == 5 {
		if parts[3] == "message" {
			return parts[0] + "/" + parts[1] + "/" + parts[2] + "/" + parts[4], "message"
		}
		return filePath, parts[3]
	}
	return filePath, "message"
}
