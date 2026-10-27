package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type acpScriptedServer struct {
	mu      sync.Mutex
	methods []string
}

func (s *acpScriptedServer) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
}

func (s *acpScriptedServer) calledMethod(method string) bool {
	for _, called := range s.called() {
		if called == method {
			return true
		}
	}
	return false
}

type acpReplayResponse struct {
	updates []map[string]any
	result  any
}

// Each scripted result answers one request. Nil means an explicit RPC error;
// replay updates precede the corresponding response on the same transport.
func startACPScriptedServer(t *testing.T, results []any) (*transport, *acpScriptedServer, context.CancelFunc) {
	t.Helper()
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = clientIn.Close()
		_ = serverOut.Close()
		_ = serverIn.Close()
		_ = clientOut.Close()
	})
	srv := &acpScriptedServer{}
	tr := newTransport(clientIn, clientOut, nil, nil)
	go tr.readLoop(ctx)
	go func() {
		defer func() { _ = serverOut.Close() }()
		reader := bufio.NewReader(serverIn)
		encoder := json.NewEncoder(serverOut)
		for _, result := range results {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(line, &request) != nil || request.Method == "" {
				return
			}
			srv.mu.Lock()
			srv.methods = append(srv.methods, request.Method)
			srv.mu.Unlock()
			if replay, ok := result.(acpReplayResponse); ok {
				for _, update := range replay.updates {
					if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": update}); err != nil {
						return
					}
				}
				result = replay.result
			}
			response := rpcResponseMsg{JSONRPC: "2.0", ID: request.ID, Result: result}
			if result == nil {
				response.Error = &rpcErrPayload{Code: -32601, Message: "method not found: " + request.Method}
			}
			if err := encoder.Encode(response); err != nil {
				return
			}
		}
	}()
	return tr, srv, cancel
}

func initializeResult(loadSession, resume, list bool) map[string]any {
	sessionCaps := map[string]any{}
	if list {
		sessionCaps["list"] = map[string]any{}
	}
	if resume {
		sessionCaps["resume"] = map[string]any{}
	}
	return map[string]any{
		"protocolVersion": 1,
		"agentCapabilities": map[string]any{
			"loadSession":         loadSession,
			"sessionCapabilities": sessionCaps,
		},
	}
}

func newHandshakeTestSession(t *testing.T, tr *transport) *acpSession {
	t.Helper()
	s := &acpSession{workDir: t.TempDir(), events: make(chan core.Event, 8), ctx: t.Context(), tr: tr}
	s.alive.Store(true)
	tr.onNotif = s.onNotification
	return s
}

func TestHandshake_RestoreCapabilitiesPreserveNativeIdentityAndSettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		load   bool
		resume bool
		method string
	}{
		{"LoadOnly", true, false, "session/load"},
		{"ResumeOnly", false, true, "session/resume"},
		{"BothPreferHistoryReplay", true, true, "session/load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, server, cancel := startACPScriptedServer(t, []any{
				initializeResult(tc.load, tc.resume, true),
				map[string]any{"configOptions": modelFixtureOptions("provider/model-b", "max")},
			})
			defer cancel()
			s := newHandshakeTestSession(t, tr)
			if err := s.handshake("saved-session", ""); err != nil {
				t.Fatal(err)
			}
			controls := s.ModelControls()
			if s.CurrentSessionID() != "saved-session" || controls.CurrentModel != "provider/model-b" || controls.CurrentReasoningEffort != "max" {
				t.Fatalf("native identity/settings were replaced: id=%q controls=%+v", s.CurrentSessionID(), controls)
			}
			if got := server.called(); len(got) != 2 || got[1] != tc.method {
				t.Fatalf("restore called extra lifecycle/setter methods: %v", got)
			}
		})
	}
}

func TestHandshake_ExplicitRestoreNeverCreatesReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		load   bool
		resume bool
		result any
	}{
		{"LoadRPCError", true, false, nil},
		{"LoadErrorDoesNotTryResume", true, true, nil},
		{"ResumeRPCError", false, true, nil},
		{"Unsupported", false, false, nil},
		{"LoadWrongIdentity", true, false, map[string]any{"sessionId": "replacement"}},
		{"ResumeWrongIdentity", false, true, map[string]any{"sessionId": "replacement"}},
		{"LoadNullResponse", true, false, json.RawMessage("null")},
		{"ResumeNullResponse", false, true, json.RawMessage("null")},
		{"LoadInvalidState", true, false, "not a session state"},
		{"ResumeInvalidState", false, true, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := []any{initializeResult(tc.load, tc.resume, false)}
			if tc.load || tc.resume {
				results = append(results, tc.result)
			}
			tr, server, cancel := startACPScriptedServer(t, results)
			defer cancel()
			s := newHandshakeTestSession(t, tr)
			if err := s.handshake("saved-session", ""); err == nil {
				t.Fatal("explicit restore failure was hidden")
			}
			if s.CurrentSessionID() != "" || server.calledMethod("session/new") {
				t.Fatalf("failed restore created/bound a replacement: id=%q calls=%v", s.CurrentSessionID(), server.called())
			}
			if tc.load && server.calledMethod("session/resume") {
				t.Fatalf("failed load retried a different restoration primitive: %v", server.called())
			}
		})
	}
}

func TestHandshake_OnlyFreshRequestsCreateSessions(t *testing.T) {
	for _, id := range []string{"", core.ContinueSession} {
		t.Run(fmt.Sprintf("ID=%q", id), func(t *testing.T) {
			tr, server, cancel := startACPScriptedServer(t, []any{initializeResult(true, true, false), map[string]any{"sessionId": "fresh-session"}})
			defer cancel()
			s := newHandshakeTestSession(t, tr)
			if err := s.handshake(id, ""); err != nil {
				t.Fatal(err)
			}
			if got := server.called(); len(got) != 2 || got[1] != "session/new" || s.CurrentSessionID() != "fresh-session" {
				t.Fatalf("fresh request did not create an independent session: id=%q calls=%v", s.CurrentSessionID(), got)
			}
		})
	}
}

func TestHandshake_LoadReplayDrainsWithoutEmittingAnswersOrSyntheticResults(t *testing.T) {
	var updates []map[string]any
	for i := 0; i < 90; i++ {
		for _, kind := range []string{"user_message_chunk", "agent_message_chunk"} {
			updates = append(updates, map[string]any{"sessionId": "saved-session", "update": map[string]any{
				"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": fmt.Sprintf("history %d %s", i, kind)},
			}})
		}
	}
	tr, _, cancel := startACPScriptedServer(t, []any{initializeResult(true, true, false), acpReplayResponse{updates: updates, result: map[string]any{}}})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	if err := s.handshake("saved-session", ""); err != nil {
		t.Fatal(err)
	}
	history, err := s.cachedHistory(0)
	if err != nil || len(history) != len(updates) || len(s.events) != 0 {
		t.Fatalf("replay was lost or emitted as new output: history=%d events=%d err=%v", len(history), len(s.events), err)
	}
}

func TestHandshake_ResumeOnlyDoesNotReportPartialHistoryAsComplete(t *testing.T) {
	tr, _, cancel := startACPScriptedServer(t, []any{initializeResult(false, true, false), map[string]any{}})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	if err := s.handshake("saved-session", ""); err != nil {
		t.Fatal(err)
	}
	a := &Agent{liveSessions: map[string]*acpSession{"saved-session": s}}
	if history, err := a.GetSessionHistory(t.Context(), "saved-session", 10); err == nil || history != nil {
		t.Fatalf("resume-only backend fabricated a complete history: history=%v err=%v", history, err)
	}
}

func TestACPCapabilities_OnlyObjectsAdvertiseMethods(t *testing.T) {
	for _, raw := range []string{"", "null", "false", "true", "1", `"resume"`, "[]", "{"} {
		if hasACPCapability(json.RawMessage(raw)) {
			t.Errorf("invalid capability %q was treated as supported", raw)
		}
	}
	for _, raw := range []string{"{}", `{"vendor":true}`} {
		if !hasACPCapability(json.RawMessage(raw)) {
			t.Errorf("object capability %q was treated as unsupported", raw)
		}
	}
}
