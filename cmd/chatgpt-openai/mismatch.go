package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// prefixMismatch contains structure only. Never include text, argument values,
// URLs, media payloads, tool names, or call IDs in diagnostics.
type prefixMismatch struct {
	Index            int
	Field            string
	IncomingEncoding string
	SavedEncoding    string
	// Content-free shape of the mismatched values: "str:<bytes>:<lines>",
	// "parts:<n>", "null" or "other". Lengths distinguish wrappers and
	// rewrites without revealing any text.
	IncomingShape string
	SavedShape    string
}

func contentShape(v any) string {
	switch c := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("str:%d:%d", len(c), len(strings.Split(c, "\n")))
	case []any:
		return fmt.Sprintf("parts:%d", len(c))
	default:
		return "other"
	}
}

func contentEncoding(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case []any:
		return "parts"
	default:
		return "other"
	}
}

func sameJSON(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && string(x) == string(y)
}

func firstMismatch(incoming, saved []message) *prefixMismatch {
	a, b := canonicalMessages(incoming), canonicalMessages(saved)
	for i := range b {
		m := &prefixMismatch{Index: i}
		if i >= len(a) {
			m.Field = "history.truncated"
			return m
		}
		m.IncomingEncoding, m.SavedEncoding = contentEncoding(incoming[i].Content), contentEncoding(saved[i].Content)
		m.IncomingShape, m.SavedShape = contentShape(a[i].Content), contentShape(b[i].Content)
		switch {
		case a[i].Role != b[i].Role:
			m.Field = "role"
		case a[i].Name != b[i].Name:
			m.Field = "name"
		case a[i].ToolCallID != b[i].ToolCallID:
			m.Field = "tool_call_id"
		case !sameJSON(a[i].Content, b[i].Content):
			m.Field = "content"
		case len(a[i].ToolCalls) != len(b[i].ToolCalls):
			m.Field = "tool_calls.length"
		default:
			for j, call := range a[i].ToolCalls {
				other := b[i].ToolCalls[j]
				field := ""
				switch {
				case call.ID != other.ID:
					field = "id"
				case call.Type != other.Type:
					field = "type"
				case call.Function.Name != other.Function.Name:
					field = "function.name"
				case call.Function.Arguments != other.Function.Arguments:
					field = "function.arguments"
				}
				if field != "" {
					m.Field = fmt.Sprintf("tool_calls[%d].%s", j, field)
					break
				}
			}
		}
		if m.Field != "" {
			return m
		}
	}
	return nil
}
