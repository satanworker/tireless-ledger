package main

import (
	"fmt"
	"strings"
)

var lanceStringFields = map[string]bool{
	"scope": true, "project_name": true, "file_path": true, "file_hash": true,
	"session_id": true, "host": true, "harness": true, "role": true,
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
