package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const (
	resumeJourneyOldID = "00000000-0000-7000-8000-000000000001"
	resumeJourneyNewID = "00000000-0000-7000-8000-000000000002"
)

type resumeJourneyAgent struct {
	nativeControlTestAgent
	listed []AgentSessionInfo
}

func (a *resumeJourneyAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return append([]AgentSessionInfo(nil), a.listed...), nil
}

func (a *resumeJourneyAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	current := id
	if current == "" {
		current = resumeJourneyNewID
	}
	a.session = newNativeControlTestSession(current)
	if current == resumeJourneyOldID {
		a.session.controls.CurrentModel = "provider/model-b"
		a.session.controls.CurrentReasoningEffort = "max"
	}
	return a.nativeControlTestAgent.StartSession(ctx, id)
}

func TestNewSession_PreviousConversationCanResume(t *testing.T) {
	runNewSessionResumeJourney(t)
}

func runNewSessionResumeJourney(t *testing.T) {
	t.Helper()
	for _, mode := range []string{"TypedSwitch", "ListButton", "SwitchPicker", "CardButton"} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/Restart=%t", mode, restart), func(t *testing.T) {
				p := &stubInlineButtonPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
				a := &resumeJourneyAgent{listed: []AgentSessionInfo{
					{ID: resumeJourneyOldID, Summary: "original conversation", MessageCount: 2},
					{ID: resumeJourneyNewID, Summary: "independent new conversation", MessageCount: 0},
				}}
				path := filepath.Join(t.TempDir(), "sessions.json")
				e := NewEngine("resume", a, []Platform{p}, path, LangEnglish)
				defer func() { e.Stop() }()
				key := "test:alice"
				old := e.sessions.GetOrCreateActive(key)
				old.SetAgentSessionID(resumeJourneyOldID, a.Name())
				old.SetActiveProvider("original-provider")
				old.AddHistory("user", "original question")
				old.AddHistory("assistant", "original retained answer")
				e.sessions.Save()
				sequence := 0
				send := func(command string) {
					sequence++
					p.clearSent()
					p.buttonRows = nil
					p.buttonContent = ""
					e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "alice", MessageID: fmt.Sprint(sequence), Content: command, ReplyCtx: "ctx"})
				}

				// Open the original native UUID, then use the real /new boundary.
				send("/model")
				if mode == "CardButton" {
					e.handleCardNav("act:/new", key)
				} else {
					send("/new independent")
				}
				fresh := e.sessions.GetOrCreateActive(key)
				if fresh.ID == old.ID || fresh.GetAgentSessionID() != "" || len(fresh.GetHistory(0)) != 0 || fresh.GetActiveProvider() != "" {
					t.Fatal("/new did not create an independent empty conversation")
				}
				if old.GetAgentSessionID() != resumeJourneyOldID || len(old.GetHistory(0)) != 2 || old.GetActiveProvider() != "original-provider" {
					t.Fatal("/new erased the original UUID binding or history")
				}
				if restart {
					e.Stop()
					e = NewEngine("resume", a, []Platform{p}, path, LangEnglish)
				}
				send("/model")
				if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != resumeJourneyNewID {
					t.Fatalf("new conversation inherited the old UUID: %q", got)
				}
				starts := len(a.calls)
				send("/list")
				if len(a.calls) != starts || e.sessions.GetOrCreateActive(key).ID != fresh.ID {
					t.Fatal("listing opened or switched a conversation")
				}

				switch mode {
				case "TypedSwitch":
					send("/switch 1")
				case "ListButton", "SwitchPicker":
					if mode == "SwitchPicker" {
						send("/switch")
					}
					if !strings.Contains(p.buttonContent, "original conversation") || len(p.buttonRows) < 1 {
						t.Fatalf("session picker was not delivered: text=%q replies=%v", p.buttonContent, p.getSent())
					}
					choice := p.buttonRows[0][0].Data
					if !strings.HasPrefix(choice, "cmd:/switch ") || len(choice) > 64 {
						t.Fatalf("invalid inline callback: %q", choice)
					}
					// Native updatedAt ordering may change after a picker is sent.
					a.listed[0], a.listed[1] = a.listed[1], a.listed[0]
					send(strings.TrimPrefix(choice, "cmd:"))
				case "CardButton":
					card, err := e.renderListCard(key, 1)
					if err != nil {
						t.Fatal(err)
					}
					var choice string
					for _, element := range card.Elements {
						if item, ok := element.(CardListItem); ok {
							choice = item.BtnValue
							break
						}
					}
					if choice == "" {
						t.Fatal("card picker has no session action")
					}
					a.listed[0], a.listed[1] = a.listed[1], a.listed[0]
					e.handleCardNav(choice, key)
				}

				send("/history 10")
				if !strings.Contains(strings.Join(p.getSent(), "\n"), "original retained answer") {
					t.Fatalf("resumed history is missing: %v", p.getSent())
				}
				if active := e.sessions.GetOrCreateActive(key); active.ID != old.ID || active.GetAgentSessionID() != resumeJourneyOldID {
					t.Fatalf("picker resumed a replacement instead of the original: %s/%s", active.ID, active.GetAgentSessionID())
				}
				send("/model")
				if len(a.calls) != 3 || a.calls[0] != resumeJourneyOldID || a.calls[1] != "" || a.calls[2] != resumeJourneyOldID {
					t.Fatalf("wrong native new/load journey: %v", a.calls)
				}
				if !strings.Contains(p.buttonContent+strings.Join(p.getSent(), "\n"), "provider/model-b") || a.session.ModelControls().CurrentReasoningEffort != "max" {
					t.Fatal("resume did not retain the original model/effort")
				}
			})
		}
	}
}

func TestSessionPicker_LongIDsAndRemovedTargets(t *testing.T) {
	p := &stubInlineButtonPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	a := &stubListAgent{sessions: []AgentSessionInfo{{ID: strings.Repeat("opaque-session-", 30), Summary: "old"}}}
	e := NewEngine("resume", a, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	msg := &Message{SessionKey: "test:alice", Platform: "test", UserID: "alice", Content: "/switch", ReplyCtx: "ctx"}
	e.ReceiveMessage(p, msg)
	if len(p.buttonRows) != 1 || len(p.buttonRows[0][0].Data) > 64 {
		t.Fatalf("opaque session ID does not have a bounded callback: %v", p.buttonRows)
	}
	choice := p.buttonRows[0][0].Data
	active := e.sessions.GetOrCreateActive(msg.SessionKey)
	a.sessions = []AgentSessionInfo{{ID: "another-session", Summary: strings.TrimPrefix(choice, "cmd:/switch ")}}
	msg.Content = strings.TrimPrefix(choice, "cmd:")
	e.ReceiveMessage(p, msg)
	if e.sessions.GetOrCreateActive(msg.SessionKey).ID != active.ID || active.GetAgentSessionID() != "" {
		t.Fatal("removed picker target matched an unrelated conversation")
	}
	if !strings.Contains(strings.Join(p.getSent(), "\n"), e.i18n.Tf(MsgSwitchNoMatch, strings.TrimPrefix(choice, "cmd:/switch "))) {
		t.Fatalf("missing stale-choice error: %v", p.getSent())
	}
}
