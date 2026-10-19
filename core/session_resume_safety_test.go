package core

import (
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

func TestNativeSession_UnconfirmedTeardownPreservesConversation(t *testing.T) {
	runUnconfirmedTeardownJourney(t)
}

func runUnconfirmedTeardownJourney(t *testing.T) {
	t.Helper()
	env := newCUJEnv(t)
	key := "test:resume-safety"
	const originalID = "original-uuid"
	agent := &nativeControlTestAgent{session: newNativeControlTestSession("explicit-new-uuid")}
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
}
