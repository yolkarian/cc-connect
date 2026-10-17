package core

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRichFooter_ExplicitContextMeterShowsExactUsage(t *testing.T) {
	for _, tc := range []struct {
		name              string
		used, size, total int
		want              string
	}{
		{"current-not-cumulative", 40960, 262144, 1283000, "ctx 15.6% (40960/262144)"},
		{"explicit-zero-not-total-fallback", 0, 131072, 900000, "ctx 0.0% (0/131072)"},
		{"small-context", 2500, 1000000, 0, "ctx 0.2% (2500/1000000)"},
		{"over-window", 150, 100, 0, "ctx 100.0% (150/100)"},
		{"no-integer-overflow", math.MaxInt, 1, 0, "ctx 100.0% ("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &ContextUsage{UsedTokens: tc.used, UsedTokensKnown: true, ContextWindow: tc.size, TotalTokens: tc.total}
			got := buildClaudeStatusLineFooter("fixture/model", "max", usage)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("footer=%q, want %q", got, tc.want)
			}
			// Non-rich fallback must not turn an explicit native zero into the
			// cumulative total, or replace exact counts with percent-left.
			if got := replyFooterContextText(usage, NewI18n(LangEnglish)); !strings.Contains(got, tc.want) {
				t.Fatalf("fallback footer=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestRichFooter_UnknownContextDoesNotInventWindowOrMeter(t *testing.T) {
	got := buildClaudeStatusLineFooter("fixture/model", "max", &ContextUsage{InputTokens: 640000, OutputTokens: 33000})
	if strings.Contains(got, "ctx ") || !strings.Contains(got, "out 33.0k in 640.0k") {
		t.Fatalf("unknown meter must not be derived from turn counters: %q", got)
	}
	// Existing CLI reporters that have not opted into an explicit context
	// measurement keep their original footer representation and fallback.
	legacy := buildClaudeStatusLineFooter("legacy/model", "high", &ContextUsage{UsedTokens: 40000, ContextWindow: 1000000})
	if legacy != "legacy/model · high · ctx 4%" {
		t.Fatalf("legacy footer changed: %q", legacy)
	}
	legacy = buildClaudeStatusLineFooter("legacy/model", "high", &ContextUsage{TotalTokens: 40000, ContextWindow: 1000000})
	if legacy != "legacy/model · high · ctx 4%" {
		t.Fatalf("legacy total fallback changed: %q", legacy)
	}
}

// The CUJ uses the real ReceiveMessage path and only mocks the backend and
// platform transport. Each Send publishes a new backend usage snapshot.
type contextUsageJourneySession struct {
	*cujAgentSession
	usageMu sync.Mutex
	usage   *ContextUsage
	turn    int
}

func (s *contextUsageJourneySession) GetContextUsage() *ContextUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usage == nil {
		return nil
	}
	copy := *s.usage
	return &copy
}

func (s *contextUsageJourneySession) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.usageMu.Lock()
	s.turn++
	switch s.turn {
	case 1:
		s.usage = &ContextUsage{UsedTokens: 40960, UsedTokensKnown: true, ContextWindow: 262144}
	case 2:
		s.usage = &ContextUsage{UsedTokens: 0, UsedTokensKnown: true, ContextWindow: 262144}
	default:
		s.usage = nil
	}
	s.usageMu.Unlock()
	return s.cujAgentSession.Send(prompt, id, images, files)
}

type contextUsageJourneyAgent struct {
	stubAgent
	live *contextUsageJourneySession
}

func (a *contextUsageJourneyAgent) StartSession(context.Context, string) (AgentSession, error) {
	return a.live, nil
}

func runRichCardContextUsageJourney(t *testing.T) {
	t.Helper()
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "telegram"}, style: "card", supportPayload: true}
	a := &contextUsageJourneyAgent{live: &contextUsageJourneySession{cujAgentSession: newCUJAgentSession()}}
	e := NewEngine("context", a, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", CardMode: "rich"})
	e.SetReplyFooterEnabled(true)
	e.SetShowContextIndicator(true)
	key := "telegram:context:user"
	saved := e.sessions.GetOrCreateActive(key)
	for i, want := range []string{"ctx 15.6% (40960/262144)", "ctx 0.0% (0/262144)", ""} {
		oldEdits, oldSent := len(p.getPreviewEdits()), len(p.getSent())
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "telegram", MessageID: fmt.Sprintf("context-%d", i), UserID: "user", Content: "Measure context usage", ReplyCtx: "ctx"})
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && (len(p.getPreviewEdits())+len(p.getSent()) <= oldEdits+oldSent || saved.Busy()) {
			time.Sleep(5 * time.Millisecond)
		}
		edits, sent := p.getPreviewEdits(), p.getSent()
		if len(edits)+len(sent) <= oldEdits+oldSent || saved.Busy() {
			t.Fatal("context journey turn did not finish")
		}
		var final string
		if len(edits) > oldEdits {
			final = edits[len(edits)-1]
		} else {
			final = sent[len(sent)-1]
		}
		if want == "" {
			if strings.Contains(final, "ctx ") {
				t.Fatalf("missing usage displayed an earlier turn's context: %q", final)
			}
		} else if !strings.Contains(final, want) {
			t.Fatalf("user-visible context footer=%q, want %q", final, want)
		}
	}
}

func TestComposeRichStatusFooter_ContextMeterRespectsDisplayToggles(t *testing.T) {
	e := newClaudeFooterEngine()
	e.i18n = NewI18n(LangEnglish)
	s := &controllableAgentSession{model: "fixture/model", contextUsage: &ContextUsage{UsedTokens: 40960, UsedTokensKnown: true, ContextWindow: 262144, InputTokens: 640000, OutputTokens: 33000}}
	start := time.Now()
	footer := e.composeRichStatusFooter(false, start, nil, s, "/workspace")
	if !strings.Contains(footer, "ctx 15.6% (40960/262144)") || !strings.Contains(footer, "out 33.0k in 640.0k") {
		t.Fatalf("rich card lost native usage: %q", footer)
	}
	if got := e.composeRichStatusFooter(true, start, nil, s, "/workspace"); got != "" {
		t.Fatalf("streaming must not display an unsettled footer: %q", got)
	}
	e.SetShowContextIndicator(false)
	if got := e.composeRichStatusFooter(false, start, nil, s, "/workspace"); strings.Contains(got, "ctx ") || strings.Contains(got, "out ") {
		t.Fatalf("hidden context was shown: %q", got)
	}
	e.SetReplyFooterEnabled(false)
	if got := e.composeRichStatusFooter(false, start, nil, s, "/workspace"); got != "" {
		t.Fatalf("disabled footer was shown: %q", got)
	}
}
