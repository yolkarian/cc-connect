package acp

import (
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestACPToolLifecycle_ProtocolStatusAndLabels(t *testing.T) {
	for _, tc := range []struct {
		name, update, status, label string
	}{
		{"start", `{"sessionUpdate":"tool_call","toolCallId":"ollama:1:bash","title":"$ printf OK","kind":"execute","status":"pending","rawInput":{"command":"printf OK"}}`, "pending", "execute"},
		{"progress", `{"sessionUpdate":"tool_call_update","toolCallId":"ollama:1:bash","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"partial"}}]}`, "in_progress", ""},
		{"completion without output", `{"sessionUpdate":"tool_call_update","toolCallId":"ollama:1:bash","status":"completed"}`, "completed", ""},
		{"failure without output", `{"sessionUpdate":"tool_call_update","toolCallId":"ollama:1:bash","status":"failed"}`, "failed", ""},
		{"changed title", `{"sessionUpdate":"tool_call_update","toolCallId":"ollama:1:bash","title":"a different action title","status":"completed"}`, "completed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := mapSessionUpdate("session", json.RawMessage(`{"update":`+tc.update+`}`))
			if len(events) != 1 {
				t.Fatalf("lifecycle patch dropped: %+v", events)
			}
			e := events[0]
			if e.ToolStatus != tc.status || e.ToolName != tc.label {
				t.Fatalf("lost status or displayed identity/action as tool name: %+v", e)
			}
		})
	}
}

func TestACPToolLifecycle_CallIDSurvivesAllPatches(t *testing.T) {
	for _, update := range []string{
		`{"sessionUpdate":"tool_call","toolCallId":"opaque:id","title":"Read file","kind":"read","status":"pending"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"opaque:id","rawInput":{"path":"artifact://0"}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"opaque:id","content":[{"type":"content","content":{"type":"text","text":"partial"}}]}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"opaque:id","status":"completed"}`,
	} {
		events := mapSessionUpdate("session", json.RawMessage(`{"update":`+update+`}`))
		if len(events) != 1 || events[0].ToolCallID != "opaque:id" || events[0].ToolName == "opaque:id" {
			t.Fatalf("tool identity lost or exposed as name: %+v", events)
		}
		if events[0].Type != core.EventToolUse && events[0].Type != core.EventToolResult {
			t.Fatalf("tool patch became another content channel: %+v", events)
		}
	}
}

func TestACPToolLifecycle_MissingIDIsNotGuessedFromTitle(t *testing.T) {
	for _, kind := range []string{"tool_call", "tool_call_update"} {
		update := `{"update":{"sessionUpdate":"` + kind + `","title":"same name","status":"completed"}}`
		if events := mapSessionUpdate("session", json.RawMessage(update)); len(events) != 0 {
			t.Fatalf("invalid tool update got an invented identity: %+v", events)
		}
	}
}
