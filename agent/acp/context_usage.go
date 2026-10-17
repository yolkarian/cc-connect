package acp

import (
	"encoding/json"
	"sync"

	"github.com/chenhg5/cc-connect/core"
)

// ACP has two different measurements: usage_update.used/size describes current
// context occupancy; session/prompt.usage contains billable turn totals across
// possibly many model requests. Never derive one from the other.
type acpContextUsageCache struct {
	mu        sync.RWMutex
	sessionID string
	value     *core.ContextUsage
}

var _ core.ContextUsageReporter = (*acpSession)(nil)

func (s *acpSession) GetContextUsage() *core.ContextUsage {
	sid := s.currentACPSessionID()
	s.usage.mu.RLock()
	defer s.usage.mu.RUnlock()
	if sid == "" || sid != s.usage.sessionID || s.usage.value == nil {
		return nil
	}
	usage := *s.usage.value
	return &usage
}

func (s *acpSession) resetContextUsage(sid string) {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	s.usage.sessionID = sid
	s.usage.value = nil
}

// absorbContextUsageUpdate returns whether this was usage metadata, even when
// malformed. It must never fall through to best-effort answer text extraction.
func (s *acpSession) absorbContextUsageUpdate(params json.RawMessage) bool {
	var notification struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.Update.Kind != "usage_update" {
		return false
	}
	var payload struct {
		Update struct {
			Used *int `json:"used"`
			Size *int `json:"size"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &payload) != nil || notification.SessionID == "" {
		return true
	}
	used, size := payload.Update.Used, payload.Update.Size
	if used == nil || size == nil || *used < 0 || *size <= 0 {
		return true
	}
	if sid := s.currentACPSessionID(); sid != "" && sid != notification.SessionID {
		return true
	}
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	// A pre-handshake update stays private until the response binds its UUID.
	if s.usage.sessionID != notification.SessionID || s.usage.value == nil {
		s.usage.value = &core.ContextUsage{}
	}
	s.usage.sessionID = notification.SessionID
	s.usage.value.UsedTokens = *used
	s.usage.value.UsedTokensKnown = true
	s.usage.value.ContextWindow = *size
	return true
}

func (s *acpSession) absorbPromptUsage(sid string, response json.RawMessage) {
	var result struct {
		Usage *struct {
			Input      *int `json:"inputTokens"`
			Output     *int `json:"outputTokens"`
			CacheRead  *int `json:"cachedReadTokens"`
			CacheWrite *int `json:"cachedWriteTokens"`
			Total      *int `json:"totalTokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(response, &result) != nil || result.Usage == nil {
		return
	}
	counts := result.Usage
	for _, value := range []*int{counts.Input, counts.Output, counts.CacheRead, counts.CacheWrite, counts.Total} {
		if value != nil && *value < 0 {
			return
		}
	}
	if counts.Input == nil && counts.Output == nil && counts.CacheRead == nil && counts.CacheWrite == nil {
		return
	}
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	if s.usage.sessionID != sid || s.usage.value == nil {
		s.usage.value = &core.ContextUsage{}
	}
	s.usage.sessionID = sid
	value := func(count *int) int {
		if count == nil {
			return 0
		}
		return *count
	}
	s.usage.value.InputTokens = value(counts.Input)
	s.usage.value.OutputTokens = value(counts.Output)
	s.usage.value.CachedInputTokens = value(counts.CacheRead)
	s.usage.value.CacheCreationInputTokens = value(counts.CacheWrite)
	// TotalTokens intentionally stays unset. core's legacy fallback interprets
	// it as current context, whereas ACP's totalTokens is cumulative per turn.
}
