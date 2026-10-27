package acp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestNotification_ForeignSessionCannotCompleteOrMutateConversation(t *testing.T) {
	for _, update := range []string{
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"foreign answer"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"foreign-call","kind":"execute","rawInput":{"command":"foreign command"}}`,
		`{"sessionUpdate":"current_mode_update","currentModeId":"foreign-mode"}`,
	} {
		t.Run(update, func(t *testing.T) {
			s := &acpSession{ctx: context.Background(), events: make(chan core.Event, 8), acpSessID: "own-session", currentMode: "own-mode", toolInputByID: make(map[string]string)}
			s.history.bind("own-session")
			s.onNotification("session/update", json.RawMessage(`{"sessionId":"other-session","update":`+update+`}`))
			if len(s.events) != 0 || len(s.history.snapshot(0)) != 0 || s.CurrentMode() != "own-mode" || len(s.toolInputByID) != 0 {
				t.Fatalf("foreign notification crossed session boundary: events=%d history=%v mode=%q tools=%v", len(s.events), s.history.snapshot(0), s.CurrentMode(), s.toolInputByID)
			}
		})
	}
}

func TestNotification_PartialToolResultPreservesCategoryAndIdentity(t *testing.T) {
	s := &acpSession{ctx: context.Background(), events: make(chan core.Event, 8), acpSessID: "own-session"}
	s.history.bind("own-session")
	for _, update := range []string{
		`{"sessionUpdate":"tool_call","toolCallId":"opaque-call","kind":"execute","title":"run a readable command"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"opaque-call","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"working"}}]}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"opaque-call","title":"updated action summary","status":"completed"}`,
	} {
		s.onNotification("session/update", json.RawMessage(`{"sessionId":"own-session","update":`+update+`}`))
		ev := <-s.events
		if ev.ToolName != "execute" || ev.ToolCallID != "opaque-call" {
			t.Fatalf("partial tool update lost protocol category/identity: %+v", ev)
		}
		if ev.Type == core.EventToolResult && ev.ToolStatus == "completed" && ev.ToolInput != "updated action summary" {
			t.Fatalf("action title was not retained as summary: %+v", ev)
		}
	}
}
