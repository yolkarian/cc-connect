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

	"github.com/chenhg5/cc-connect/core"
)

func TestACPHistory_ReplayAndLiveSessionWithoutSecondWriter(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cmd: executable, args: []string{"-test.run=^TestACPHistoryFixture$"},
		workDir: t.TempDir(), extraEnv: []string{"CC_ACP_HISTORY_FIXTURE=1"}}
	defer a.Stop()
	var agent core.Agent = a
	history, ok := agent.(core.HistoryProvider)
	if !ok {
		t.Fatal("ACP does not expose native session history")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	entries, err := history.GetSessionHistory(ctx, "saved-history", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Role != "user" || entries[2].Content != "final historical answer" {
		t.Fatalf("lost replay/boundaries: %+v", entries)
	}
	if strings.Contains(fmt.Sprint(entries), "PRIVATE_THOUGHT") || strings.Contains(fmt.Sprint(entries), "FOREIGN") {
		t.Fatal("non-answer metadata leaked into history")
	}
	live, err := a.StartSession(ctx, "saved-history")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if len(live.(*acpSession).events) != 0 {
		t.Fatal("historical replay became a live reply")
	}
	// A second load would fail to launch. Reading a live UUID must use its cache.
	a.mu.Lock()
	a.cmd = "/does-not-exist-must-not-probe-live-uuid"
	a.mu.Unlock()
	entries, err = history.GetSessionHistory(ctx, "saved-history", 2)
	if err != nil || len(entries) != 2 {
		t.Fatalf("live history opened a competing process: %v", err)
	}
	if err := live.Send("live user", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err = history.GetSessionHistory(ctx, "saved-history", 0)
	if err != nil || len(entries) != 5 || entries[3].Content != "live user" || entries[4].Content != "live answer" {
		t.Fatalf("live turn history duplicated/missing: %+v, %v", entries, err)
	}
	entries[0].Content = "mutated caller copy"
	again, _ := history.GetSessionHistory(ctx, "saved-history", 0)
	if again[0].Content == entries[0].Content {
		t.Fatal("history snapshot aliases internal state")
	}
}

func TestACPHistory_FailedExplicitLoadNeverCreatesSession(t *testing.T) {
	executable, _ := os.Executable()
	a := &Agent{cmd: executable, args: []string{"-test.run=^TestACPHistoryFixture$"}, workDir: t.TempDir(), extraEnv: []string{"CC_ACP_HISTORY_FIXTURE=1"}}
	_, err := a.GetSessionHistory(context.Background(), "missing", 10)
	if err == nil || !strings.Contains(err.Error(), "history missing") {
		t.Fatalf("history load error hidden: %v", err)
	}
}

func TestACPHistoryFixture(t *testing.T) {
	if os.Getenv("CC_ACP_HISTORY_FIXTURE") == "" {
		return
	}
	notify := func(id, kind, text string) {
		message := map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": id, "update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}}}}
		data, _ := json.Marshal(message)
		fmt.Println(string(data))
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			os.Exit(2)
		}
		switch r.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":{}}}}}`+"\n", r.ID)
		case "session/load":
			if r.Params.SessionID != "saved-history" {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"history missing"}}`+"\n", r.ID)
				continue
			}
			notify("saved-history", "user_message_chunk", "historical question")
			notify("saved-history", "agent_thought_chunk", "PRIVATE_THOUGHT")
			notify("foreign", "agent_message_chunk", "FOREIGN")
			for range 140 {
				notify("saved-history", "agent_message_chunk", "historical answer ")
			}
			notify("saved-history", "tool_call", "not conversation text")
			notify("saved-history", "agent_message_chunk", "final historical answer")
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", r.ID)
		case "session/prompt":
			notify("saved-history", "user_message_chunk", "live user")
			notify("saved-history", "agent_message_chunk", "live answer")
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`+"\n", r.ID)
		case "session/close":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", r.ID)
		default:
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected method: no new sessions or config writes permitted"}}`+"\n", r.ID)
		}
	}
	os.Exit(0)
}
