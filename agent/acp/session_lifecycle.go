package acp

import (
	"context"
	"encoding/json"
	"fmt"
)

// resumeExisting restores a saved conversation. Load is preferred over resume
// because only load replays authoritative history; resume is a separate
// capability, not a retry path after a failed load.
//
// Merge note: this file is fork policy. Upstream keeps a different
// resume/load preference order and error handling, so conflicts are resolved
// here rather than in the protocol plumbing in session.go.
func (s *acpSession) resumeExisting(ctx context.Context, resumeSessionID string, loadSupported, resumeSupported bool) error {
	method := "session/load"
	if !loadSupported {
		if !resumeSupported {
			return fmt.Errorf("acp: server does not support session/load or session/resume")
		}
		method = "session/resume"
	}
	s.history.bind(resumeSessionID)
	result, err := s.tr.call(ctx, method, map[string]any{
		"sessionId":  resumeSessionID,
		"cwd":        s.workDir,
		"mcpServers": []any{},
	})
	if err != nil {
		return fmt.Errorf("acp: %s: %w", method, err)
	}
	var restored *struct {
		SessionID     string            `json:"sessionId"`
		Modes         *acpModesBlock    `json:"modes"`
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(result, &restored); err != nil {
		return fmt.Errorf("acp: parse %s: %w", method, err)
	}
	if restored == nil {
		return fmt.Errorf("acp: invalid %s response: expected a session state object", method)
	}
	if restored.SessionID != "" && restored.SessionID != resumeSessionID {
		return fmt.Errorf("acp: %s returned session %q, requested %q", method, restored.SessionID, resumeSessionID)
	}
	// Only session/new allocates identity. Neither restore method may
	// silently replace the requested UUID, even if a vendor echoes one.
	s.setACPSessionID(resumeSessionID)
	s.absorbModes(restored.Modes)
	s.absorbConfigOptions(restored.ConfigOptions)
	s.historyComplete = method == "session/load"
	return nil
}

// startFresh allocates a new conversation and adopts the identity the server
// assigns; only session/new may allocate one.
func (s *acpSession) startFresh(ctx context.Context) error {
	newParams := map[string]any{
		"cwd":        s.workDir,
		"mcpServers": []any{},
	}
	newRes, err := s.tr.call(ctx, "session/new", newParams)
	if err != nil {
		return fmt.Errorf("acp: session/new: %w", err)
	}
	var sn struct {
		SessionID     string            `json:"sessionId"`
		Modes         *acpModesBlock    `json:"modes"`
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(newRes, &sn); err != nil {
		return fmt.Errorf("acp: parse session/new: %w", err)
	}
	if sn.SessionID == "" {
		return fmt.Errorf("acp: session/new: empty sessionId")
	}
	s.setACPSessionID(sn.SessionID)
	s.absorbModes(sn.Modes)
	s.absorbConfigOptions(sn.ConfigOptions)
	s.historyComplete = true
	return nil
}
