package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type nativeControlTestAgent struct {
	mu       sync.Mutex
	session  *nativeControlTestSession
	workDir  string
	model    string
	effort   string
	calls    []string
	startErr error
}

func (a *nativeControlTestAgent) Name() string                      { return "native-test" }
func (a *nativeControlTestAgent) SupportsSessionModelControl() bool { return true }
func (a *nativeControlTestAgent) StartSession(_ context.Context, id string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, id)
	if a.startErr != nil {
		return nil, a.startErr
	}
	return a.session, nil
}
func (a *nativeControlTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *nativeControlTestAgent) Stop() error                      { return nil }
func (a *nativeControlTestAgent) GetWorkDir() string               { return a.workDir }
func (a *nativeControlTestAgent) SetModel(model string)            { a.model = model }
func (a *nativeControlTestAgent) GetModel() string                 { return a.model }
func (a *nativeControlTestAgent) SetReasoningEffort(effort string) { a.effort = effort }

type nativeControlTestSession struct {
	*controllableAgentSession
	mu        sync.Mutex
	controls  ModelControls
	changeErr error
}

func newNativeControlTestSession(id string) *nativeControlTestSession {
	return &nativeControlTestSession{controllableAgentSession: newControllableSession(id), controls: ModelControls{
		CurrentModel: "provider/model-a", Models: []ModelOption{{Name: "provider/model-a"}, {Name: "provider/model-b"}}, CurrentReasoningEffort: "high", ReasoningEfforts: []string{"off", "high", "max"},
	}}
}
func (s *nativeControlTestSession) ModelControls() ModelControls {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.controls
	c.Models = append([]ModelOption(nil), c.Models...)
	c.ReasoningEfforts = append([]string(nil), c.ReasoningEfforts...)
	return c
}
func (s *nativeControlTestSession) SetSessionModel(_ context.Context, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changeErr != nil {
		return s.changeErr
	}
	for _, m := range s.controls.Models {
		if m.Name == model {
			s.controls.CurrentModel = model
			return nil
		}
	}
	return errors.New("unknown model")
}
func (s *nativeControlTestSession) SetSessionReasoningEffort(_ context.Context, effort string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changeErr != nil {
		return s.changeErr
	}
	for _, value := range s.controls.ReasoningEfforts {
		if value == effort {
			s.controls.CurrentReasoningEffort = effort
			return nil
		}
	}
	return errors.New("unknown effort")
}

func TestNativeModelControl_TextAndCardsPreserveConversation(t *testing.T) {
	for _, card := range []bool{false, true} {
		t.Run(fmt.Sprint(card), func(t *testing.T) {
			p := &stubPlatformEngine{n: "telegram"}
			live := newNativeControlTestSession("original-uuid")
			agent := &nativeControlTestAgent{session: live}
			e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
			defer e.cleanupInteractiveState("telegram:c:u")
			msg := &Message{SessionKey: "telegram:c:u", ReplyCtx: "ctx"}
			saved := e.sessions.GetOrCreateActive(msg.SessionKey)
			saved.SetAgentSessionID("original-uuid", agent.Name())
			saved.AddHistory("user", "original history")
			e.cmdModel(p, msg, nil)
			if len(p.sent) != 1 || !strings.Contains(p.sent[0], "provider/model-a") || !strings.Contains(p.sent[0], "provider/model-b") {
				t.Fatalf("native model menu=%v", p.sent)
			}
			if card {
				result := e.handleCardNav("act:/model switch "+modelChoiceToken("provider/model-b"), msg.SessionKey)
				if result == nil || result.Header == nil || result.Header.Color != "green" {
					t.Fatalf("model result=%+v", result)
				}
				e.handleCardNav("act:/reasoning max", msg.SessionKey)
			} else {
				e.cmdModel(p, msg, []string{"switch", "provider/model-b"})
				e.cmdReasoning(p, msg, []string{"max"})
			}
			if live.ModelControls().CurrentModel != "provider/model-b" || live.ModelControls().CurrentReasoningEffort != "max" {
				t.Fatal("native selection was not applied")
			}
			if len(agent.calls) != 1 || agent.calls[0] != "original-uuid" {
				t.Fatalf("model commands restarted the session: %v", agent.calls)
			}
			if saved.GetAgentSessionID() != "original-uuid" || len(saved.GetHistory(0)) != 1 || !live.Alive() {
				t.Fatal("model controls replaced identity, history or process")
			}
		})
	}
}

func TestNativeModelControl_FailedChangePreservesState(t *testing.T) {
	p := &stubPlatformEngine{n: "telegram"}
	live := newNativeControlTestSession("original-uuid")
	live.changeErr = errors.New("provider authorization expired")
	agent := &nativeControlTestAgent{session: live, model: "provider/model-a", effort: "high"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	defer e.cleanupInteractiveState("telegram:c:u")
	msg := &Message{SessionKey: "telegram:c:u", ReplyCtx: "ctx"}
	saved := e.sessions.GetOrCreateActive(msg.SessionKey)
	saved.SetAgentSessionID("original-uuid", agent.Name())
	saved.AddHistory("user", "keep me")
	e.cmdModel(p, msg, []string{"provider/model-b"})
	e.cmdReasoning(p, msg, []string{"max"})
	if agent.model != "provider/model-a" || agent.effort != "high" || saved.GetAgentSessionID() != "original-uuid" || len(saved.GetHistory(0)) != 1 {
		t.Fatal("failed change modified persisted selection or history")
	}
	for _, text := range p.sent {
		if !strings.Contains(text, "expired") || strings.Contains(text, "switched to") {
			t.Fatalf("failure was hidden: %q", text)
		}
	}
}

func TestNativeModelControl_ResumeFailureNeverCreatesAnotherConversation(t *testing.T) {
	p := &stubPlatformEngine{n: "telegram"}
	agent := &nativeControlTestAgent{startErr: errors.New("Could not restore model provider/old-model")}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	msg := &Message{SessionKey: "telegram:c:u", ReplyCtx: "ctx"}
	saved := e.sessions.GetOrCreateActive(msg.SessionKey)
	saved.SetAgentSessionID("original-uuid", agent.Name())
	saved.AddHistory("user", "keep me")
	e.cmdModel(p, msg, nil)
	if len(agent.calls) != 1 || agent.calls[0] != "original-uuid" || saved.GetAgentSessionID() != "original-uuid" {
		t.Fatalf("resume failure replaced the conversation: %v", agent.calls)
	}
	if len(p.sent) != 1 || !strings.Contains(p.sent[0], "Could not restore model") {
		t.Fatalf("resume error was hidden: %v", p.sent)
	}
}

func TestNativeModelControl_MultiWorkspaceSelectionsBelongToNativeSessions(t *testing.T) {
	p := &stubPlatformEngine{n: "telegram"}
	global := &nativeControlTestAgent{}
	e := NewEngine("test", global, []Platform{p}, "", LangEnglish)
	base := t.TempDir()
	e.SetMultiWorkspace(base, filepath.Join(base, "bindings.json"))
	store := filepath.Join(base, "state.json")
	e.projectState = NewProjectStateStore(store)
	key := "telegram:chat:u"
	dirs := []string{normalizeWorkspacePath(filepath.Join(base, "a")), normalizeWorkspacePath(filepath.Join(base, "b"))}
	for i, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		ws := e.workspacePool.GetOrCreate(dir)
		live := newNativeControlTestSession("uuid-" + fmt.Sprint(i))
		agent := &nativeControlTestAgent{session: live, workDir: dir}
		ws.agent = agent
		ws.sessions = NewSessionManager("")
		saved := ws.sessions.GetOrCreateActive(key)
		saved.SetAgentSessionID(live.CurrentSessionID(), agent.Name())
		saved.AddHistory("user", "history-"+fmt.Sprint(i))
		e.workspaceBindings.Bind("project:test", "telegram:chat", "", dir)
		model, effort := "provider/model-b", "max"
		if i == 1 {
			model, effort = "provider/model-a", "off"
		}
		msg := &Message{SessionKey: key, ReplyCtx: "ctx"}
		e.cmdModel(p, msg, []string{model})
		e.cmdReasoning(p, msg, []string{effort})
		if saved.GetAgentSessionID() != live.CurrentSessionID() || len(saved.GetHistory(0)) != 1 {
			t.Fatal("workspace history was reset")
		}
		if agent.model != "" || agent.effort != "" {
			t.Fatal("live selections became new-conversation defaults")
		}
		actual := live.ModelControls()
		if actual.CurrentModel != model || actual.CurrentReasoningEffort != effort {
			t.Fatal("workspace native selections were mixed")
		}
		defer e.cleanupInteractiveState(dir + ":" + key)
	}
	loaded := NewProjectStateStore(store)
	if loaded.HasLegacyNativeModelOverrides() {
		t.Fatal("native selections created a second persistence owner")
	}
}

func TestNativeModelControl_RequiresExplicitMigrationBeforePlatformStart(t *testing.T) {
	for _, field := range []string{"workspace_model_overrides", "workspace_reasoning_overrides", "malformed"} {
		t.Run(field, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "project.state.json")
			original := fmt.Sprintf(`{"%s":{"/workspace":"old-preference"}}`, field)
			if field == "malformed" {
				original = "{"
			}
			if err := os.WriteFile(file, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			live := newNativeControlTestSession("saved-uuid")
			agent := &nativeControlTestAgent{session: live}
			e := NewEngine("test", agent, nil, "", LangEnglish)
			e.projectState = NewProjectStateStore(file)
			if err := e.Start(); err == nil || !strings.Contains(err.Error(), "offline migration") {
				t.Fatalf("migration gate=%v", err)
			}
			if _, err := e.sessionModelController("telegram:c:u", nil, nil); err == nil {
				t.Fatal("native command bypassed migration gate")
			}
			if _, err := e.HandleRelay(context.Background(), "other", "telegram:c:u", "test"); err == nil {
				t.Fatal("relay bypassed migration gate")
			}
			saved := e.sessions.GetOrCreateActive("telegram:c:u")
			saved.SetAgentSessionID("saved-uuid", agent.Name())
			state := e.getOrCreateInteractiveStateWith("telegram:c:u", nil, nil, saved, e.sessions, nil, "")
			if state.startError == nil {
				t.Fatal("ordinary session start bypassed migration gate")
			}
			if len(agent.calls) != 0 {
				t.Fatal("a conflicting saved session was opened")
			}
			if data, _ := os.ReadFile(file); string(data) != original {
				t.Fatal("runtime silently migrated preferences")
			}
		})
	}
}

func TestNativeModelChoiceTokensSurviveCatalogReordering(t *testing.T) {
	name := strings.Repeat("provider/very-long-model-", 8)
	token := modelChoiceToken(name)
	if len("cmd:/model switch "+token) > 64 {
		t.Fatal("Telegram callback payload exceeds limit")
	}
	models := []ModelOption{{Name: "another/model"}, {Name: name}}
	if got := resolveNativeModelTarget(token, models); got != name {
		t.Fatalf("choice resolved to %q", got)
	}
}
