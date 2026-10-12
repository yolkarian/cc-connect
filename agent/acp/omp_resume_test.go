package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestACPExplicitResume(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		wantError bool
	}{
		{name: "standard_response"},
		{name: "server_error", wantError: true},
		{name: "unsupported", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			callbacks := &fakeCallbacks{}
			session, err := newACPSession(ctx, acpSessionConfig{
				command:         executable,
				args:            []string{"-test.run=^TestACPResumeProtocolFixture$"},
				extraEnv:        []string{"CC_ACP_RESUME_FIXTURE=" + test.name},
				workDir:         t.TempDir(),
				resumeSessionID: "saved-session",
				callbacks:       callbacks,
			})
			if test.wantError {
				if err == nil {
					session.Close()
					t.Fatal("explicit resume must surface its load failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if got := session.CurrentSessionID(); got != "saved-session" {
				t.Fatalf("session ID = %q, want saved-session", got)
			}
			if got := len(session.events); got != 0 {
				t.Fatalf("load replay emitted %d new-reply events", got)
			}
			session.onNotification("session/update", json.RawMessage(`{"sessionId":"saved-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"live reply"}}}`))
			event := <-session.events
			if event.Content != "live reply" || event.SessionID != "saved-session" {
				t.Fatalf("live reply event = %+v", event)
			}
			modes, ok := callbacks.lastModes()
			if !ok || modes.CurrentModeID != "default" {
				t.Fatalf("loaded mode was not retained: %+v", modes)
			}
		})
	}
}

// The actual session constructor starts this test executable as an ACP server.
func TestACPResumeProtocolFixture(t *testing.T) {
	mode := os.Getenv("CC_ACP_RESUME_FIXTURE")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(1)
		}
		switch request.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":%t}}}`+"\n", request.ID, mode != "unsupported")
		case "session/load":
			if mode == "server_error" || request.Params.SessionID != "saved-session" {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"load rejected"}}`+"\n", request.ID)
			} else {
				// More updates than the production event buffer can hold.
				for range 140 {
					fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"saved-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"historical reply"}}}}`)
				}
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"modes":{"currentModeId":"default","availableModes":[{"id":"default","name":"Default"}]}}}`+"\n", request.ID)
			}
		case "session/new":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"fresh-session"}}`+"\n", request.ID)
		default:
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected method"}}`+"\n", request.ID)
		}
	}
	os.Exit(0)
}
