package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type unsolicitedDeliveryPlatform struct {
	stubPlatformEngine
	path      string
	answer    string
	once      sync.Once
	persisted chan bool
}

func (p *unsolicitedDeliveryPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	first := false
	if strings.Contains(content, p.answer) {
		p.once.Do(func() {
			first = true
			var saved struct {
				Sessions map[string]*Session `json:"sessions"`
			}
			data, err := os.ReadFile(p.path)
			found := false
			if err == nil && json.Unmarshal(data, &saved) == nil {
				for _, session := range saved.Sessions {
					for _, item := range session.History {
						found = found || item.Content == p.answer
					}
				}
			}
			p.persisted <- found
		})
	}
	if first {
		return errors.New("delivery outcome unknown; do not replay")
	}
	return p.stubPlatformEngine.Send(ctx, replyCtx, content)
}

func TestUnsolicitedDelivery_PersistsBeforeAttemptAndRecoversThroughHistory(t *testing.T) {
	runUnsolicitedDeliveryJourney(t)
}

func runUnsolicitedDeliveryJourney(t *testing.T) {
	t.Helper()
	for _, mode := range []string{"result", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.json")
			p := &unsolicitedDeliveryPlatform{
				stubPlatformEngine: stubPlatformEngine{n: "test"},
				path:               path, answer: "BACKGROUND_ANSWER_" + mode, persisted: make(chan bool, 1),
			}
			agent := &cujAgent{}
			e := NewEngine("background", agent, []Platform{p}, path, LangEnglish)
			defer func() { _ = e.Stop() }()
			const key = "test:background:user"
			sequence := 0
			send := func(content string) {
				sequence++
				e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "user", MessageID: fmt.Sprint(sequence), Content: content, ReplyCtx: "ctx"})
			}
			wait := func(ready func() bool) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for !ready() {
					if time.Now().After(deadline) {
						t.Fatal("background journey did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			}

			// Action 1 starts the real foreground/unsolicited-reader lifecycle.
			send("Start a background task")
			var state *interactiveState
			wait(func() bool {
				e.interactiveMu.Lock()
				state = e.interactiveStates[key]
				e.interactiveMu.Unlock()
				if state == nil {
					return false
				}
				state.mu.Lock()
				ready := state.unsolicitedDone != nil
				state.mu.Unlock()
				return ready && !e.sessions.GetOrCreateActive(key).Busy()
			})
			agent.mu.Lock()
			live := agent.sessions[0]
			agent.mu.Unlock()
			live.events <- Event{Type: EventText, Content: p.answer}
			switch mode {
			case "result":
				live.events <- Event{Type: EventResult, Done: true}
			case "error":
				live.events <- Event{Type: EventError, Error: errors.New("background task failed")}
			case "cancel":
				// The next event is a barrier: once consumed, the preceding
				// text has been buffered and cancellation must flush it.
				live.events <- Event{Type: EventToolUse, ToolName: "read"}
				wait(func() bool { return len(live.events) == 0 })
				e.stopUnsolicitedReader(state)
			}
			select {
			case saved := <-p.persisted:
				if !saved {
					t.Fatal("background answer reached transport before durable history")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no background delivery attempt")
			}

			// Action 2 recovers the generated answer without another prompt.
			p.clearSent()
			send("/history 20")
			if !strings.Contains(strings.Join(p.getSent(), "\n"), p.answer) {
				t.Fatalf("failed background delivery is absent from history: %v", p.getSent())
			}
			// Action 3 continues the same conversation without replaying work.
			send("Continue this conversation")
			wait(func() bool { return !e.sessions.GetOrCreateActive(key).Busy() })
			live.mu.Lock()
			prompts := append([]string(nil), live.sentPrompts...)
			live.mu.Unlock()
			if len(prompts) != 2 {
				t.Fatalf("history recovery regenerated background work: %v", prompts)
			}
		})
	}
}
