package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func modelControlFixture(t *testing.T, mode, resume string) *acpSession {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	session, err := newACPSession(ctx, acpSessionConfig{
		command: executable, args: []string{"-test.run=^TestACPModelControlProtocolFixture$"},
		extraEnv: []string{"CC_ACP_MODEL_FIXTURE=" + mode}, workDir: t.TempDir(), resumeSessionID: resume,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestACPModelControls_PreserveIdentityAndRefreshEfforts(t *testing.T) {
	for _, resume := range []string{"", "saved-model-session"} {
		t.Run("resume="+resume, func(t *testing.T) {
			s := modelControlFixture(t, "ok", resume)
			id := s.CurrentSessionID()
			before := s.ModelControls()
			if before.CurrentModel != "provider/model-a" || len(before.Models) != 2 || before.Models[0].Alias != "model-a" {
				t.Fatalf("initial controls: %+v", before)
			}
			if err := s.SetSessionModel(context.Background(), "provider/model-b"); err != nil {
				t.Fatal(err)
			}
			after := s.ModelControls()
			if after.CurrentModel != "provider/model-b" || after.CurrentReasoningEffort != "high" || strings.Join(after.ReasoningEfforts, ",") != "off,high,max" {
				t.Fatalf("model-specific controls were not refreshed: %+v", after)
			}
			if err := s.SetSessionReasoningEffort(context.Background(), "xhigh"); err == nil {
				t.Fatal("previous model's effort must be rejected")
			}
			if err := s.SetSessionReasoningEffort(context.Background(), "max"); err != nil {
				t.Fatal(err)
			}
			if got := s.ModelControls().CurrentReasoningEffort; got != "max" {
				t.Fatalf("effort=%q", got)
			}
			if s.CurrentSessionID() != id {
				t.Fatal("model control changed the conversation identity")
			}
			if len(s.events) != 0 {
				t.Fatal("configuration updates or load replay became answer events")
			}
		})
	}
}

func TestACPModelControls_ResumeKeepsNativeSelectionDespiteConfiguredDefaults(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := newACPSession(ctx, acpSessionConfig{
		command: executable, args: []string{"-test.run=^TestACPModelControlProtocolFixture$"},
		extraEnv: []string{"CC_ACP_MODEL_FIXTURE=ok"}, workDir: t.TempDir(), resumeSessionID: "saved-model-session",
		initialModel: "provider/model-b", initialReasoning: "max",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	controls := s.ModelControls()
	if controls.CurrentModel != "provider/model-a" || controls.CurrentReasoningEffort != "xhigh" || s.CurrentSessionID() != "saved-model-session" {
		t.Fatalf("resume overwrote native selections: %+v", controls)
	}
}

func TestACPModelControls_NewConversationUsesExplicitInitialSelection(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := newACPSession(context.Background(), acpSessionConfig{
		command: executable, args: []string{"-test.run=^TestACPModelControlProtocolFixture$"},
		extraEnv: []string{"CC_ACP_MODEL_FIXTURE=ok"}, workDir: t.TempDir(),
		initialModel: "provider/model-b", initialReasoning: "max",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if actual := s.ModelControls(); actual.CurrentModel != "provider/model-b" || actual.CurrentReasoningEffort != "max" {
		t.Fatalf("explicit new-session initialization failed: %+v", actual)
	}
}

func TestACPModelControls_FailureNeverReportsSuccess(t *testing.T) {
	for _, mode := range []string{"rejected", "unconfirmed", "malformed", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			s := modelControlFixture(t, mode, "saved-model-session")
			before := s.ModelControls().CurrentModel
			if err := s.SetSessionModel(context.Background(), "provider/model-b"); err == nil {
				t.Fatal("unacknowledged change reported success")
			}
			if s.ModelControls().CurrentModel != before || s.CurrentSessionID() != "saved-model-session" {
				t.Fatal("failed change replaced configuration or identity")
			}
		})
	}
}

func TestACPModelControls_ConfigNotificationsAreSessionScoped(t *testing.T) {
	s := modelControlFixture(t, "ok", "saved-model-session")
	for _, id := range []string{"foreign-session", "saved-model-session"} {
		params, _ := json.Marshal(map[string]any{"sessionId": id, "update": map[string]any{"sessionUpdate": "config_option_update", "configOptions": modelFixtureOptions("provider/model-b", "max")}})
		s.onNotification("session/update", params)
		want := "provider/model-a"
		if id == "saved-model-session" {
			want = "provider/model-b"
		}
		if got := s.ModelControls().CurrentModel; got != want {
			t.Fatalf("notification %q: model=%q", id, got)
		}
	}
	if len(s.events) != 0 {
		t.Fatal("metadata leaked into answer stream")
	}
}

func TestACPModelControls_RejectChangesDuringPrompt(t *testing.T) {
	s := modelControlFixture(t, "ok", "")
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err := s.SetSessionModel(context.Background(), "provider/model-b"); err == nil || !strings.Contains(err.Error(), "active prompt") {
		t.Fatalf("error=%v", err)
	}
}

func modelFixtureOptions(model, thinking string) []any {
	levels := []string{"off", "low", "high", "xhigh"}
	if model == "provider/model-b" {
		levels = []string{"off", "high", "max"}
	}
	var efforts []any
	for _, level := range levels {
		efforts = append(efforts, map[string]any{"value": level, "name": level})
	}
	return []any{
		map[string]any{"id": "backend-model-selector", "category": "model", "type": "select", "currentValue": model, "options": []any{map[string]any{"group": "provider", "name": "Provider", "options": []any{map[string]any{"value": "provider/model-a", "name": "Model A"}, map[string]any{"value": "provider/model-b", "name": "Model B"}}}}},
		map[string]any{"id": "backend-effort-selector", "category": "thought_level", "type": "select", "currentValue": thinking, "options": efforts},
		map[string]any{"id": "other-setting", "category": "other", "type": "boolean", "currentValue": true},
	}
}

func TestACPModelControlProtocolFixture(t *testing.T) {
	mode := os.Getenv("CC_ACP_MODEL_FIXTURE")
	if mode == "" {
		return
	}
	model, thinking, id := "provider/model-a", "xhigh", "new-model-session"
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
				ConfigID  string `json:"configId"`
				Value     string `json:"value"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			os.Exit(1)
		}
		var result any
		switch r.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}}
		case "session/new", "session/load":
			if r.Method == "session/load" {
				id = r.Params.SessionID
				for range 140 {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": id, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "replay"}}}})
				}
			}
			options := modelFixtureOptions(model, thinking)
			if mode == "unsupported" {
				options = nil
			}
			response := map[string]any{"configOptions": options}
			if r.Method == "session/new" {
				response["sessionId"] = id
			}
			result = response
		case "session/set_config_option":
			if r.Params.SessionID != id {
				os.Exit(2)
			}
			if mode == "rejected" {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32603, "message": "Internal error", "data": map[string]any{"details": "model rejected"}}})
				continue
			}
			if mode == "malformed" {
				result = map[string]any{}
				break
			}
			if mode != "unconfirmed" {
				switch r.Params.ConfigID {
				case "backend-model-selector":
					model = r.Params.Value
					thinking = "high"
				case "backend-effort-selector":
					thinking = r.Params.Value
				default:
					os.Exit(3)
				}
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": id, "update": map[string]any{"sessionUpdate": "config_option_update", "configOptions": modelFixtureOptions(model, thinking)}}})
			}
			result = map[string]any{"configOptions": modelFixtureOptions(model, thinking)}
		default:
			fmt.Fprintln(os.Stderr, "unexpected method", r.Method)
			os.Exit(4)
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result})
	}
	os.Exit(0)
}

func TestRPCError_ContainsActionableDetailsWithoutCredentials(t *testing.T) {
	t.Setenv("TEST_PROVIDER_API_KEY", "fixture-private-key")
	err := &rpcErrPayload{Code: -32603, Message: "Internal error; Authorization: Bearer private-stored-token", Data: json.RawMessage(`{"details":"Could not restore model provider/old-model; key=fixture-private-key","token":"not-for-output"}`)}
	got := err.Error()
	if !strings.Contains(got, "Could not restore model") || strings.Contains(got, "fixture-private-key") || strings.Contains(got, "not-for-output") || strings.Contains(got, "private-stored-token") {
		t.Fatalf("unsafe or incomplete error=%q", got)
	}
}
