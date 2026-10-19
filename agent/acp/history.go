package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Replay is retained for history inspection, never emitted as a new answer.
// ACP supplies roles/content but need not supply historical timestamps.
type acpHistory struct {
	mu        sync.Mutex
	sessionID string
	entries   []core.HistoryEntry
	role      string
	pending   strings.Builder
	echo      string
}

func (h *acpHistory) bind(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessionID != id {
		h.sessionID, h.entries, h.role, h.echo = id, nil, "", ""
		h.pending.Reset()
	}
}

func (h *acpHistory) beginPrompt(prompt string) {
	h.boundary()
	h.append("user", prompt)
	h.mu.Lock()
	h.echo = prompt
	h.mu.Unlock()
}

func (h *acpHistory) boundary() {
	h.mu.Lock()
	h.flushLocked()
	h.mu.Unlock()
}

func (h *acpHistory) flushLocked() {
	if h.pending.Len() > 0 {
		h.entries = append(h.entries, core.HistoryEntry{Role: h.role, Content: h.pending.String()})
	}
	h.pending.Reset()
	h.role = ""
}

func (h *acpHistory) append(role, text string) {
	if text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.role != role {
		h.flushLocked()
	}
	h.role = role
	h.pending.WriteString(text)
}

func (h *acpHistory) absorb(params json.RawMessage) {
	var wrap struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &wrap) != nil {
		return
	}
	h.mu.Lock()
	matches := h.sessionID != "" && wrap.SessionID == h.sessionID
	h.mu.Unlock()
	if !matches {
		return
	}
	var head struct {
		Kind string `json:"sessionUpdate"`
	}
	if json.Unmarshal(wrap.Update, &head) != nil {
		return
	}
	role := ""
	switch head.Kind {
	case "user_message_chunk":
		role = "user"
	case "agent_message_chunk":
		role = "assistant"
	case "tool_call", "tool_call_update":
		h.boundary()
		return
	default:
		return // thoughts, tools, plans, usage are not conversation text
	}
	for _, event := range mapTextChunk(wrap.SessionID, wrap.Update, core.EventText) {
		if role == "user" {
			h.mu.Lock()
			echo := h.echo != "" && strings.HasPrefix(h.echo, event.Content)
			if echo {
				h.echo = strings.TrimPrefix(h.echo, event.Content)
			}
			h.mu.Unlock()
			if echo {
				continue
			}
		}
		h.append(role, event.Content)
	}
}

func (h *acpHistory) snapshot(limit int) []core.HistoryEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := len(h.entries)
	if h.pending.Len() > 0 {
		count++
	}
	start := 0
	if limit > 0 && count > limit {
		start = count - limit
	}
	out := make([]core.HistoryEntry, 0, count-start)
	if start < len(h.entries) {
		out = append(out, h.entries[start:]...)
	}
	if h.pending.Len() > 0 {
		out = append(out, core.HistoryEntry{Role: h.role, Content: h.pending.String()})
	}
	return out
}

// GetSessionHistory uses the live session's replay/cache if it owns this UUID.
// Otherwise a metadata-only explicit load collects replay, then closes. The
// per-agent lifecycle lock prevents a concurrent load/start of the same UUID;
// no prompt, session/new, model setter or native JSONL parsing is involved.
func (a *Agent) GetSessionHistory(ctx context.Context, id string, limit int) ([]core.HistoryEntry, error) {
	if id == "" || id == core.ContinueSession {
		return nil, fmt.Errorf("acp: history requires an explicit session id")
	}
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	if live := a.liveSessions[id]; live != nil && live.Alive() {
		return live.history.snapshot(limit), nil
	}
	a.mu.RLock()
	cfg := acpSessionConfig{
		command: a.cmd, args: append(append([]string{}, a.cliExtraArgs...), a.args...),
		workDir: a.workDir, authMethod: a.authMethod, resumeSessionID: id,
		extraEnv: append(append([]string{}, a.extraEnv...), a.sessionEnv...),
	}
	a.mu.RUnlock()
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session, err := newACPSession(probeCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("acp: read session history: %w", err)
	}
	defer session.Close()
	return session.history.snapshot(limit), nil
}
