package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestACPContextUsageReporter_BackendMeterRegression(t *testing.T) {
	s := &acpSession{events: make(chan core.Event, 16)}
	s.setACPSessionID("context-session")
	s.onNotification("session/update", json.RawMessage(`{"sessionId":"context-session","update":{"sessionUpdate":"usage_update","used":40960,"size":262144}}`))
	reporter, ok := any(s).(core.ContextUsageReporter)
	if !ok {
		t.Fatal("ACP discards the native context meter: ContextUsageReporter is missing")
	}
	usage := reporter.GetContextUsage()
	if usage == nil || usage.UsedTokens != 40960 || usage.ContextWindow != 262144 {
		t.Fatalf("native context meter not preserved: %+v", usage)
	}
	if len(s.events) != 0 {
		t.Fatal("usage metadata must not become answer events")
	}
}

func TestACPContextUsage_ValidationAndSnapshotIsolation(t *testing.T) {
	s := &acpSession{events: make(chan core.Event, 16)}
	s.setACPSessionID("context-session")
	seed := json.RawMessage(`{"sessionId":"context-session","update":{"sessionUpdate":"usage_update","used":40960,"size":262144}}`)
	s.onNotification("session/update", seed)
	invalid := []string{
		`{"sessionUpdate":"usage_update","size":100}`,
		`{"sessionUpdate":"usage_update","used":1}`,
		`{"sessionUpdate":"usage_update","used":-1,"size":100}`,
		`{"sessionUpdate":"usage_update","used":1,"size":0}`,
		`{"sessionUpdate":"usage_update","used":1,"size":-100}`,
		`{"sessionUpdate":"usage_update","used":1.5,"size":100}`,
		`{"sessionUpdate":"usage_update","used":999999999999999999999,"size":100}`,
		`{"sessionUpdate":"usage_update","used":null,"size":100,"text":"not an answer"}`,
	}
	for _, update := range invalid {
		s.onNotification("session/update", json.RawMessage(`{"sessionId":"context-session","update":`+update+`}`))
		if got := s.GetContextUsage(); got == nil || got.UsedTokens != 40960 || !got.UsedTokensKnown {
			t.Fatalf("invalid update overwrote the context meter: %+v", got)
		}
	}
	s.onNotification("session/update", json.RawMessage(`{"sessionId":"foreign-session","update":{"sessionUpdate":"usage_update","used":99,"size":100}}`))
	s.onNotification("session/update", json.RawMessage(`{"update":{"sessionUpdate":"usage_update","used":99,"size":100}}`))
	copy := s.GetContextUsage()
	copy.UsedTokens = 1
	if got := s.GetContextUsage(); got.UsedTokens != 40960 {
		t.Fatalf("foreign update or mutated snapshot leaked: %+v", got)
	}
	if len(s.events) != 0 {
		t.Fatal("usage metadata leaked into the event stream")
	}
}

func TestACPContextUsage_PreHandshakeDoesNotReplayOrCrossSessions(t *testing.T) {
	s := &acpSession{events: make(chan core.Event, 16)}
	s.onNotification("session/update", json.RawMessage(`{"sessionId":"saved-session","update":{"sessionUpdate":"usage_update","used":900,"size":10000}}`))
	s.onNotification("session/update", json.RawMessage(`{"sessionId":"saved-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"historical answer"}}}`))
	if s.GetContextUsage() != nil || len(s.events) != 0 {
		t.Fatal("unbound usage/history became visible before the handshake")
	}
	s.setACPSessionID("saved-session")
	if got := s.GetContextUsage(); got == nil || got.UsedTokens != 900 {
		t.Fatalf("lost metadata delivered before session/load response: %+v", got)
	}
	s.setACPSessionID("another-session")
	if s.GetContextUsage() != nil {
		t.Fatal("a different session inherited the previous context meter")
	}
}

func TestACPContextUsage_ConcurrentSnapshots(t *testing.T) {
	s := &acpSession{}
	s.setACPSessionID("context-session")
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.absorbContextUsageUpdate(json.RawMessage(`{"sessionId":"context-session","update":{"sessionUpdate":"usage_update","used":123,"size":1000}}`))
				if usage := s.GetContextUsage(); usage != nil {
					usage.UsedTokens = -1 // only the caller's copy changes
				}
			}
		}()
	}
	wg.Wait()
	if s.GetContextUsage().UsedTokens != 123 {
		t.Fatal("snapshot mutation changed the shared usage cache")
	}
}

func TestACPContextUsage_PromptTotalsAreNotContextAndMissingDataResets(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := newACPSession(ctx, acpSessionConfig{
		command: executable, args: []string{"-test.run=^TestACPUsageProtocolFixture$"},
		extraEnv: []string{"CC_ACP_USAGE_FIXTURE=1"}, workDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for turn := 1; turn <= 3; turn++ {
		done := make(chan error, 1)
		go func() { done <- s.Send("turn", "", nil, nil) }()
		for {
			select {
			case ev := <-s.Events():
				if ev.Type != core.EventResult {
					continue
				}
				got := s.GetContextUsage()
				if got == nil {
					t.Fatal("usage missing when final result becomes visible")
				}
				switch turn {
				case 1:
					if got.UsedTokens != 40960 || got.ContextWindow != 262144 || !got.UsedTokensKnown || got.TotalTokens != 0 || got.InputTokens != 640000 || got.OutputTokens != 33000 || got.CachedInputTokens != 500000 || got.CacheCreationInputTokens != 110000 {
						t.Fatalf("turn totals replaced the native meter or were lost: %+v", got)
					}
				case 2:
					if got.UsedTokensKnown || got.ContextWindow != 0 || got.TotalTokens != 0 || got.InputTokens != 500 || got.OutputTokens != 20 || got.CachedInputTokens != 0 || got.CacheCreationInputTokens != 0 {
						t.Fatalf("missing native meter reused prior-turn data: %+v", got)
					}
				case 3:
					if !got.UsedTokensKnown || got.UsedTokens != 0 || got.ContextWindow != 131072 || got.InputTokens != 0 || got.OutputTokens != 0 {
						t.Fatalf("explicit zero or missing turn counters were misrepresented: %+v", got)
					}
				}
			case <-ctx.Done():
				t.Fatal("fixture turn timed out")
			}
			break
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestACPUsageProtocolFixture(t *testing.T) {
	if os.Getenv("CC_ACP_USAGE_FIXTURE") != "1" {
		return
	}
	write := func(frame any) { data, _ := json.Marshal(frame); fmt.Println(string(data)) }
	turn := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1}
		case "session/new":
			result = map[string]any{"sessionId": "context-session"}
		case "session/prompt":
			turn++
			result = map[string]any{"stopReason": "end_turn"}
			if turn != 2 {
				used, size := 40960, 262144
				if turn == 3 {
					used, size = 0, 131072
				}
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "context-session", "update": map[string]any{"sessionUpdate": "usage_update", "used": used, "size": size}}})
			}
			if turn == 1 {
				result = map[string]any{"stopReason": "end_turn", "usage": map[string]int{"inputTokens": 640000, "outputTokens": 33000, "cachedReadTokens": 500000, "cachedWriteTokens": 110000, "totalTokens": 1283000}}
			} else if turn == 2 {
				result = map[string]any{"stopReason": "end_turn", "usage": map[string]int{"inputTokens": 500, "outputTokens": 20, "totalTokens": 520}}
			}
		}
		if len(req.ID) != 0 {
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
	}
	os.Exit(0)
}
