package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFirstMismatchStructureOnly(t *testing.T) {
	base := message{Role: "assistant", Content: "private text", ToolCalls: []toolCall{{ID: "private-id", Type: "function", Function: callFunction{Name: "private-name", Arguments: `{"n":1}`}}}}
	tests := []struct {
		name, field string
		change      func(*message)
	}{
		{"role", "role", func(m *message) { m.Role = "user" }},
		{"edit", "content", func(m *message) { m.Content = "different private text" }},
		{"encoding", "content", func(m *message) {
			m.Content = []any{map[string]any{"type": "text", "text": "private text", "extra": "meaningful"}}
		}},
		{"call id", "tool_calls[0].id", func(m *message) { m.ToolCalls[0].ID = "other-private-id" }},
		{"arguments", "tool_calls[0].function.arguments", func(m *message) { m.ToolCalls[0].Function.Arguments = `{"n":2}` }},
		{"tool result id", "tool_call_id", func(m *message) { m.ToolCallID = "private-result-id" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			changed.ToolCalls = append([]toolCall(nil), base.ToolCalls...)
			tt.change(&changed)
			got := firstMismatch([]message{{Role: "system", Content: "same"}, changed}, []message{{Role: "system", Content: "same"}, base})
			if got == nil || got.Index != 1 || got.Field != tt.field {
				t.Fatalf("diagnostic: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "private") {
				t.Fatal("private values leaked")
			}
		})
	}
	if got := firstMismatch(nil, []message{base}); got == nil || got.Index != 0 || got.Field != "history.truncated" {
		t.Fatalf("truncated: %+v", got)
	}
}

func TestMismatchEquivalentEncodingsAndJSON(t *testing.T) {
	a := []message{{Role: "assistant", Content: "same", ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "echo", Arguments: `{"a":1,"b":2}`}}}}}
	b := []message{{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "same"}}, ToolCalls: []toolCall{{ID: "call_1", Type: "function", Function: callFunction{Name: "echo", Arguments: "{\n \"b\": 2, \"a\": 1}"}}}}}
	if got := firstMismatch(a, b); got != nil {
		t.Fatalf("equivalent: %+v", got)
	}
}
