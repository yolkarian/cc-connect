package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type unconfirmedCloseTestSession struct {
	*controllableAgentSession
}

func (s *unconfirmedCloseTestSession) Close() error {
	return errors.New("fixture cannot confirm process termination")
}

type unconfirmedHistoryTestAgent struct {
	nativeControlTestAgent
	historyCalls int
}

func (a *unconfirmedHistoryTestAgent) GetSessionHistory(context.Context, string, int) ([]HistoryEntry, error) {
	a.historyCalls++
	return []HistoryEntry{{Role: "assistant", Content: "native history probe result"}}, nil
}

func TestNativeSession_UnconfirmedTeardownPreservesConversation(t *testing.T) {
	runUnconfirmedTeardownJourney(t)
}

func runUnconfirmedTeardownJourney(t *testing.T) {
	t.Helper()
	env := newCUJEnv(t)
	key := "test:resume-safety"
	const originalID = "original-uuid"
	agent := &unconfirmedHistoryTestAgent{nativeControlTestAgent: nativeControlTestAgent{session: newNativeControlTestSession("explicit-new-uuid")}}
	env.engine.agent = agent
	saved := env.engine.sessions.GetOrCreateActive(key)
	saved.SetAgentSessionID(originalID, agent.Name())
	saved.AddHistory("user", "history before the uncertain stop")
	env.engine.sessions.Save()

	// Use the real teardown boundary rather than writing an unsafe flag: a
	// failed Close must not let the next model menu abandon the saved UUID.
	previous := &unconfirmedCloseTestSession{newControllableSession(originalID)}
	env.engine.closeAgentSessionWithTimeout(key, previous, env.plat, "ctx")
	t.Cleanup(func() { env.engine.cleanupInteractiveState(key) })

	for attempt := 0; attempt < 2; attempt++ {
		env.plat.clearSent()
		env.userSends("resume-safety", "/model")
		if len(agent.calls) != 0 || saved.GetAgentSessionID() != originalID || len(saved.GetHistory(0)) != 1 {
			t.Fatalf("uncertain stop replaced or reopened the conversation: starts=%v uuid=%q", agent.calls, saved.GetAgentSessionID())
		}
		got := strings.Join(env.plat.getSent(), "\n")
		if !strings.Contains(got, originalID) || !strings.Contains(got, "/new") {
			t.Fatalf("blocked resume must identify the retained UUID and explicit recovery action: %s", got)
		}
	}

	// History probes can also open a native writer. Text and card requests
	// must respect the same UUID-scoped teardown guard as prompts/controls.
	for _, command := range []string{"/history 10", "/history"} {
		env.plat.clearSent()
		env.userSends("resume-safety", command)
		if agent.historyCalls != 0 || !strings.Contains(strings.Join(env.plat.getSent(), "\n"), originalID) {
			t.Fatalf("history bypassed uncertain-writer protection: probes=%d replies=%v", agent.historyCalls, env.plat.getSent())
		}
	}
	card := env.engine.renderHistoryCard(key)
	var cardText strings.Builder
	for _, element := range card.Elements {
		if markdown, ok := element.(CardMarkdown); ok {
			cardText.WriteString(markdown.Content)
		}
	}
	if agent.historyCalls != 0 || !strings.Contains(cardText.String(), originalID) {
		t.Fatalf("card history bypassed uncertain-writer protection: probes=%d card=%s", agent.historyCalls, cardText.String())
	}

	// Starting over is allowed only after the user explicitly asks. Keep the
	// native UUID tracked and ensure the warning does not poison another UUID.
	env.plat.clearSent()
	env.userSends("resume-safety", "/new")
	env.userSends("resume-safety", "/model")
	if len(agent.calls) != 1 || agent.calls[0] != "" {
		t.Fatalf("explicit /new did not start exactly one new conversation: %v", agent.calls)
	}
	got := strings.Join(env.plat.getSent(), "\n")
	if !strings.Contains(got, "provider/model-a") {
		t.Fatalf("new conversation model menu was not delivered: %s", got)
	}
	if _, tracked := env.engine.sessions.KnownAgentSessionIDs()[originalID]; !tracked {
		t.Fatal("explicit /new lost the original native UUID from session tracking")
	}

	// A stopped, confirmed-dead replacement can resume normally. Uncertainty
	// about the original UUID must not affect another conversation in the chat.
	env.engine.cleanupInteractiveState(key)
	agent.session = newNativeControlTestSession("explicit-new-uuid")
	env.plat.clearSent()
	env.userSends("resume-safety", "/model")
	if len(agent.calls) != 2 || agent.calls[1] != "explicit-new-uuid" || !strings.Contains(strings.Join(env.plat.getSent(), "\n"), "provider/model-a") {
		t.Fatalf("uncertainty leaked into a different UUID: starts=%v replies=%v", agent.calls, env.plat.getSent())
	}
	env.plat.clearSent()
	env.userSends("resume-safety", "/history 10")
	if agent.historyCalls != 1 || !strings.Contains(strings.Join(env.plat.getSent(), "\n"), "native history probe result") {
		t.Fatalf("original UUID's guard blocked the replacement's history: probes=%d replies=%v", agent.historyCalls, env.plat.getSent())
	}
}
