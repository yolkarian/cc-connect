package core

import "strings"

// richThinkingAccumulator distinguishes wire deltas from complete thought
// messages. Adjacent deltas update one progress block without trimming spaces
// between words or manufacturing a new step for every token/newline.
type richThinkingAccumulator struct {
	active bool
	index  int
	text   string
}

func (a *richThinkingAccumulator) endBlock() {
	a.active = false
	a.text = ""
}

func (a *richThinkingAccumulator) append(steps []ToolStep, event Event, maxLen int) []ToolStep {
	if !event.ThinkingDelta {
		a.endBlock()
		if text := strings.TrimSpace(truncateIf(event.Content, maxLen)); text != "" {
			steps = append(steps, ToolStep{Kind: ToolStepKindThinking, Name: "Thinking", Summary: text, Done: true})
		}
		return steps
	}
	a.text += event.Content
	// Keep one extra rune to retain the truncation marker, bounding retained
	// state as well as the rendered preview. A nonpositive limit is unlimited.
	if maxLen > 0 {
		if runes := []rune(a.text); len(runes) > maxLen+1 {
			a.text = string(runes[:maxLen+1])
		}
	}
	text := strings.TrimSpace(truncateIf(a.text, maxLen))
	if text == "" {
		return steps
	}
	if !a.active {
		a.index = len(steps)
		a.active = true
		steps = append(steps, ToolStep{Kind: ToolStepKindThinking, Name: "Thinking", Done: true})
	}
	steps[a.index].Summary = text
	return steps
}
