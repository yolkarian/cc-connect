package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backendHistoryAgent struct {
	stubListAgent
	history      map[string][]HistoryEntry
	historyError error
}

func (a *backendHistoryAgent) GetSessionHistory(_ context.Context, id string, limit int) ([]HistoryEntry, error) {
	if a.historyError != nil {
		return nil, a.historyError
	}
	entries := a.history[id]
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

func TestHistory_PrefersNativeOverPartialBridgeAndSurfacesErrors(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	a := &backendHistoryAgent{history: map[string][]HistoryEntry{"native-id": {{Role: "user", Content: "native original question"}, {Role: "assistant", Content: "native original long answer"}}}}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	s := e.sessions.GetOrCreateActive("test:alice")
	s.SetAgentSessionID("native-id", a.Name())
	s.AddHistory("assistant", "only partial bridge history")
	msg := &Message{SessionKey: "test:alice", Platform: "test", UserID: "alice", Content: "/history 10", ReplyCtx: "ctx"}
	e.ReceiveMessage(p, msg)
	got := strings.Join(p.getSent(), "\n")
	if !strings.Contains(got, "native original question") || strings.Contains(got, "only partial bridge history") {
		t.Fatalf("native history hidden: %s", got)
	}
	a.historyError = errors.New("explicit load rejected; saved model unavailable")
	p.clearSent()
	e.ReceiveMessage(p, msg)
	got = strings.Join(p.getSent(), "\n")
	if !strings.Contains(got, "explicit load rejected") {
		t.Fatalf("native history failure hidden: %s", got)
	}
	if s.GetAgentSessionID() != "native-id" || len(s.GetHistory(0)) != 1 {
		t.Fatal("read-only history changed conversation")
	}
}

func TestCardSwitch_PreservesHistoryOnBothRoundTripLegs(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	a := &stubListAgent{sessions: []AgentSessionInfo{{ID: "native-A", Summary: "A"}, {ID: "native-B", Summary: "B"}}}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	key := "test:alice"
	one := e.sessions.GetOrCreateActive(key)
	one.SetAgentSessionID("native-A", a.Name())
	one.AddHistory("user", "A original question")
	one.AddHistory("assistant", "A original answer")
	two := e.sessions.NewSession(key, "B")
	two.SetAgentSessionID("native-B", a.Name())
	two.AddHistory("user", "B original question")
	two.AddHistory("assistant", "B original answer")
	for _, id := range []string{"native-A", "native-B", "native-A"} {
		e.handleCardNav("act:/switch "+id, key)
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "alice", Content: "/history 10", ReplyCtx: "ctx"})
		last := p.getSent()
		want := strings.TrimPrefix(id, "native-") + " original answer"
		if len(last) == 0 || !strings.Contains(last[len(last)-1], want) {
			t.Fatalf("card switch erased %s history: %v", id, last)
		}
	}
	if len(one.GetHistory(0)) != 2 || len(two.GetHistory(0)) != 2 {
		t.Fatal("round trip overwrote native bindings/history")
	}
}

func TestHistory_UnknownBackendTimestampIsNotInvented(t *testing.T) {
	if got := historyEntryTime(time.Time{}); got != "—" {
		t.Fatalf("unknown timestamp = %q", got)
	}
}
