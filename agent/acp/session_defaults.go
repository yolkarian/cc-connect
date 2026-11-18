package acp

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// applyNewConversationDefaults applies the constructor's mode/model/reasoning
// options after the handshake. The mode is a session-level control and applies
// to resumed conversations too; model and effort are explicit defaults for a
// brand new conversation only, because a loaded conversation owns the
// selections persisted with it.
//
// Merge note: this file is fork policy. Upstream's ACP agent re-applies the
// model to every session; conflicts are resolved by keeping saved selections
// authoritative on resume.
func (s *acpSession) applyNewConversationDefaults(cfg acpSessionConfig, sessionCtx context.Context) error {
	// Apply the agent-level mode preference now that we have a session id. If
	// set_mode fails (e.g. modeId unknown to this backend) we log and carry on
	// with whatever mode the server defaulted to — the alternative would be to
	// reject the session entirely, which is worse UX for a non-critical control.
	if strings.TrimSpace(cfg.initialMode) != "" {
		if ok := s.SetLiveMode(cfg.initialMode); !ok {
			slog.Warn("acp: initial mode could not be applied",
				"mode", cfg.initialMode,
				"session_id", s.currentACPSessionID(),
			)
		}
	}

	// A successful load owns its saved model/effort; never reset it on resume.
	if cfg.resumeSessionID != "" {
		return nil
	}
	controlCtx, controlCancel := context.WithTimeout(sessionCtx, 30*time.Second)
	defer controlCancel()
	if cfg.initialModel != "" && cfg.initialModel != s.ModelControls().CurrentModel {
		if err := s.SetSessionModel(controlCtx, cfg.initialModel); err != nil {
			return err
		}
	}
	if cfg.initialReasoning != "" && cfg.initialReasoning != s.ModelControls().CurrentReasoningEffort {
		if err := s.SetSessionReasoningEffort(controlCtx, cfg.initialReasoning); err != nil {
			return err
		}
	}
	return nil
}
