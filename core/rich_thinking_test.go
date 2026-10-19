package core

import (
	"strings"
	"testing"
	"time"
)

func TestRichThinking_WireFragmentsPreserveWordsAndNewlines(t *testing.T) {
	var state richThinkingAccumulator
	var steps []ToolStep
	for _, fragment := range []string{"I", " should", " check", " whether", " they", "'re", " checking", " the", " session", ".", "\n", "Then", " respond", " briefly", " with", " confirmation", "."} {
		steps = state.append(steps, Event{Type: EventThinking, ThinkingDelta: true, Content: fragment}, 300)
	}
	want := "I should check whether they're checking the session.\nThen respond briefly with confirmation."
	if len(steps) != 1 || steps[0].Summary != want {
		t.Fatalf("reasoning fragments became separate or broken steps: %+v", steps)
	}
	state.endBlock()
	steps = append(steps, ToolStep{Kind: ToolStepKindTool, Name: "Bash"})
	steps = state.append(steps, Event{Type: EventThinking, ThinkingDelta: true, Content: "After the tool."}, 300)
	if len(steps) != 3 || steps[2].Summary != "After the tool." {
		t.Fatalf("tool boundary did not start a new block: %+v", steps)
	}
}

func TestRichThinking_CompleteThoughtsRemainDistinct(t *testing.T) {
	var state richThinkingAccumulator
	var steps []ToolStep
	for _, text := range []string{"First complete thought.", "Second complete thought."} {
		steps = state.append(steps, Event{Type: EventThinking, Content: text}, 300)
	}
	if len(steps) != 2 {
		t.Fatalf("complete thought messages were joined: %+v", steps)
	}
}

func TestRichThinking_BoundsAccumulatedPreview(t *testing.T) {
	var state richThinkingAccumulator
	var steps []ToolStep
	for range 100 {
		steps = state.append(steps, Event{Type: EventThinking, ThinkingDelta: true, Content: "字"}, 20)
	}
	if len(steps) != 1 || steps[0].Summary != strings.Repeat("字", 20)+"..." || len([]rune(state.text)) > 21 {
		t.Fatalf("unbounded or incorrect preview: %+v retained=%q", steps, state.text)
	}
}

func TestProcessInteractiveEvents_RichCardCoalescesThoughtDeltas(t *testing.T) {
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "telegram"}, style: "card", supportPayload: true}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{ThinkingMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500, ToolMessages: true, Mode: "quiet", CardMode: "rich"})
	key := "telegram:thoughts:user"
	saved := e.sessions.GetOrCreateActive(key)
	live := newControllableSession("original-uuid")
	live.events = make(chan Event, 64)
	state := &interactiveState{agentSession: live, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	for _, fragment := range []string{"I", " should", " respond", "\n", "briefly", " with", " confirmation", "."} {
		live.events <- Event{Type: EventThinking, ThinkingDelta: true, Content: fragment}
	}
	live.events <- Event{Type: EventText, Content: "ANSWER_ONLY"}
	live.events <- Event{Type: EventResult, Content: "ANSWER_ONLY", Done: true}
	e.processInteractiveEvents(state, saved, e.sessions, key, "msg", time.Now(), nil, nil, state.replyCtx, 0)
	edits := p.getPreviewEdits()
	if len(edits) == 0 {
		t.Fatal("no rich card updates")
	}
	final := edits[len(edits)-1]
	if strings.Count(final, "Kind:thinking") != 1 || !strings.Contains(final, "Summary:I should respond\nbriefly with confirmation.") || !strings.Contains(final, "markdown=ANSWER_ONLY") {
		t.Fatalf("final reasoning/answer framing: %s", final)
	}
}
