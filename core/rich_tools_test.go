package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRichTools_ParallelSameNameCallsUseIdentity(t *testing.T) {
	var steps []ToolStep
	for _, id := range []string{"first", "second"} {
		steps = mergeRichToolStep(steps, Event{Type: EventToolUse, ToolCallID: id, ToolName: "execute", ToolInput: id, ToolStatus: "pending"}, "", 500)
	}
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolCallID: "first", ToolStatus: "in_progress"}, "partial", 500)
	if len(steps) != 2 || steps[0].Done || steps[1].Result != "" || steps[0].Result != "partial" {
		t.Fatalf("progress completed a call or updated its parallel sibling: %+v", steps)
	}
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolCallID: "second", ToolStatus: "completed"}, "OK", 500)
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolCallID: "first", ToolStatus: "failed"}, "", 500)
	if len(steps) != 2 || !steps[0].Done || !steps[1].Done || steps[0].Status != "failed" || steps[1].Status != "completed" || steps[0].Result != "partial" {
		t.Fatalf("empty terminal patch lost identity/status/output: %+v", steps)
	}
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolCallID: "first", ToolStatus: "in_progress"}, "late output", 500)
	if !steps[0].Done || steps[0].Status != "failed" || steps[0].Result != "partial" {
		t.Fatalf("late progress undid completion: %+v", steps)
	}
}

func TestRichTools_DuplicateStartAndMetadataPatchStayOneRow(t *testing.T) {
	start := Event{Type: EventToolUse, ToolCallID: "opaque", ToolName: "execute", ToolInput: "printf OK", ToolStatus: "pending"}
	steps := mergeRichToolStep(nil, start, "", 500)
	steps = mergeRichToolStep(steps, start, "", 500)
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolCallID: "opaque", ToolInput: "new action"}, "partial", 500)
	if len(steps) != 1 || steps[0].Done || steps[0].Name != "execute" || steps[0].Summary != "new action" {
		t.Fatalf("metadata/progress patch created a second row or a false completion: %+v", steps)
	}
}

func TestRichTools_IDUpdatesNeverMatchAnUnrelatedName(t *testing.T) {
	steps := []ToolStep{{Kind: ToolStepKindThinking, Summary: "thought"}, {Name: "execute"}}
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolCallID: "unknown", ToolName: "execute", ToolStatus: "completed"}, "result", 500)
	if len(steps) != 3 || steps[1].Done || steps[2].ToolCallID != "unknown" || !steps[2].Done {
		t.Fatalf("unseen call ID guessed a row by name/order: %+v", steps)
	}
}

func TestRichTools_UnidentifiedCLIResultsKeepTheirBehavior(t *testing.T) {
	steps := mergeRichToolStep(nil, Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "echo hi"}, "", 500)
	steps = mergeRichToolStep(steps, Event{Type: EventToolResult, ToolName: "Bash"}, "hi", 500)
	if len(steps) != 1 || !steps[0].Done || steps[0].Result != "hi" {
		t.Fatalf("unidentified CLI result changed: %+v", steps)
	}
}

func runRichToolLifecycleJourney(t *testing.T) {
	t.Helper()
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	live := newCUJAgentSession()
	a := &finalDeliveryAgent{live: live}
	e := NewEngine("tools", a, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	e.SetDisplayConfig(DisplayCfg{Mode: "full", CardMode: "rich", ToolMessages: true, ToolMaxLen: 500})
	saved := e.sessions.GetOrCreateActive("test:alice")
	// Three user turns: IDs can recur next turn, never leaking old call rows.
	for turn := 0; turn < 3; turn++ {
		var events []Event
		for i := 0; i < 3; i++ {
			id := fmt.Sprintf("opaque:%d", i)
			events = append(events,
				Event{Type: EventToolUse, ToolCallID: id, ToolName: "execute", ToolInput: fmt.Sprintf("command_%d_%d", turn, i), ToolStatus: "pending"},
				Event{Type: EventToolResult, ToolCallID: id, ToolStatus: "in_progress", Content: "partial"},
				Event{Type: EventToolResult, ToolCallID: id, ToolStatus: "completed"})
		}
		events = append(events, Event{Type: EventResult, Content: fmt.Sprintf("answer_%d", turn), Done: true})
		live.mu.Lock()
		live.pendingEvents = events
		live.mu.Unlock()
		e.ReceiveMessage(p, &Message{SessionKey: "test:alice", Platform: "test", MessageID: fmt.Sprint(turn), UserID: "alice", Content: "run tools", ReplyCtx: "ctx"})
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && (saved.Busy() || len(saved.GetHistory(0)) < 2*(turn+1)) {
			time.Sleep(5 * time.Millisecond)
		}
		frames := p.getPreviewEdits()
		if saved.Busy() || len(frames) == 0 {
			t.Fatal("tool turn failed to finalize")
		}
		final := frames[len(frames)-1]
		if strings.Count(final, "step=") != 3 || strings.Count(final, "Done:true") != 3 || strings.Contains(final, "Done:false") {
			t.Fatalf("user sees duplicates or pending rows after completion: %s", final)
		}
		for i := 0; i < 3; i++ {
			if strings.Count(final, fmt.Sprintf("command_%d_%d", turn, i)) != 1 {
				t.Fatalf("tool action repeated or missing in final card: %s", final)
			}
		}
	}
}
