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
	// ContentOff is the first differing byte offset for string-vs-string
	// content (else -1). Offsets pinpoint append-vs-rewrite classes
	// (trailing newline, truncation marker, re-encode tail) without
	// revealing any text.
	ContentOff int
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

// firstDiffOff returns the first differing byte offset of two strings, or
// min(len) when one is a prefix of the other. Structural evidence only.
func firstDiffOff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func firstMismatch(incoming, saved []message) *prefixMismatch {
	a, b := canonicalMessages(incoming), canonicalMessages(saved)
	for i := range b {
		m := &prefixMismatch{Index: i, ContentOff: -1}
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
			if ai, ok := a[i].Content.(string); ok {
				if bi, ok := b[i].Content.(string); ok {
					m.ContentOff = firstDiffOff(ai, bi)
				}
			}
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
