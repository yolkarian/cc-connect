package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSessionMessageCount_UnknownIsNotZero(t *testing.T) {
	for _, tc := range []struct {
		count int
		want  string
	}{{17, "17"}, {0, "0"}, {UnknownMessageCount, "—"}, {-27, "—"}} {
		if got := sessionMessageCount(tc.count); got != tc.want {
			t.Fatalf("count=%d label=%q want=%q", tc.count, got, tc.want)
		}
	}
}

func sessionCountFixtures() []AgentSessionInfo {
	when := time.Date(2026, 10, 19, 13, 2, 0, 0, time.UTC)
	return []AgentSessionInfo{
		{ID: "session-live", MessageCount: 17, ModifiedAt: when},
		{ID: "session-empty", Summary: "New empty session", MessageCount: 0, ModifiedAt: when},
		{ID: "session-unknown", Summary: "Count not reported", MessageCount: UnknownMessageCount, ModifiedAt: when},
	}
}

func TestSessionList_CountsAndUntitledAgreeAcrossTextAndCard(t *testing.T) {
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		t.Run(string(lang), func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("list", &stubListAgent{sessions: sessionCountFixtures()}, []Platform{p}, "", lang)
			defer e.Stop()
			msg := &Message{SessionKey: "test:alice", ReplyCtx: "ctx"}
			e.cmdList(p, msg, nil)
			card, err := e.renderListCard(msg.SessionKey, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, out := range []string{strings.Join(p.getSent(), "\n"), card.RenderText()} {
				for _, want := range []string{"**17**", "**0**", "**—**", e.i18n.T(MsgUntitled)} {
					if !strings.Contains(out, want) {
						t.Fatalf("list missing %q: %s", want, out)
					}
				}
				if strings.Contains(out, "%!") {
					t.Fatalf("list formatting failed: %s", out)
				}
			}
		})
	}
}

func runSessionListCountJourney(t *testing.T) {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("list", &stubListAgent{sessions: sessionCountFixtures()}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	key := "test:alice"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("session-live", "test")
	for i, command := range []string{"/list", "/switch session-unknown", "/list"} {
		p.clearSent()
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", MessageID: fmt.Sprint(i), UserID: "alice", Content: command, ReplyCtx: "ctx"})
		out := strings.Join(p.getSent(), "\n")
		if i == 1 {
			if !strings.Contains(out, "— msgs") {
				t.Fatalf("switch fabricated a zero count: %s", out)
			}
		} else {
			for _, want := range []string{"**17**", "**0**", "**—**", "(untitled)"} {
				if !strings.Contains(out, want) {
					t.Fatalf("list missing %q: %s", want, out)
				}
			}
		}
	}
	if e.sessions.GetOrCreateActive(key).GetAgentSessionID() != "session-unknown" {
		t.Fatal("list changed selected native UUID")
	}
}
