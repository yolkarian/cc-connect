package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/chenhg5/cc-connect/core"
)

// ACP config options are session-scoped. Keep the wire format here rather than
// making core depend on ACP or on a particular backend's config IDs.
type acpConfigOption struct {
	ID           string                  `json:"id"`
	Category     string                  `json:"category"`
	Type         string                  `json:"type"`
	CurrentValue json.RawMessage         `json:"currentValue"`
	Options      []acpConfigSelectOption `json:"options"`
}

type acpConfigSelectOption struct {
	Value       string                  `json:"value"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Options     []acpConfigSelectOption `json:"options"`
}

type acpConfigCache struct {
	mu      sync.RWMutex
	options []acpConfigOption
}

var _ core.SessionModelController = (*acpSession)(nil)

func (s *acpSession) absorbConfigOptions(options []acpConfigOption) {
	if options == nil {
		return
	}
	s.config.mu.Lock()
	s.config.options = options
	s.config.mu.Unlock()
}

func (s *acpSession) maybeAbsorbConfigOptionUpdate(params json.RawMessage) {
	var wrap struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string            `json:"sessionUpdate"`
			Options []acpConfigOption `json:"configOptions"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &wrap) != nil || wrap.Update.Kind != "config_option_update" {
		return
	}
	if wrap.SessionID != s.currentACPSessionID() {
		return
	}
	s.absorbConfigOptions(wrap.Update.Options)
}

func configValue(option acpConfigOption) string {
	var value string
	_ = json.Unmarshal(option.CurrentValue, &value)
	return value
}

func flattenConfigOptions(options []acpConfigSelectOption) []acpConfigSelectOption {
	var flat []acpConfigSelectOption
	for _, option := range options {
		if option.Value != "" {
			flat = append(flat, option)
		}
		flat = append(flat, flattenConfigOptions(option.Options)...)
	}
	return flat
}

func (s *acpSession) ModelControls() core.ModelControls {
	s.config.mu.RLock()
	defer s.config.mu.RUnlock()
	var controls core.ModelControls
	for _, option := range s.config.options {
		if option.Type != "select" {
			continue
		}
		switch option.Category {
		case "model":
			controls.CurrentModel = configValue(option)
			for _, model := range flattenConfigOptions(option.Options) {
				controls.Models = append(controls.Models, core.ModelOption{Name: model.Value, Desc: model.Name})
			}
		case "thought_level":
			controls.CurrentReasoningEffort = configValue(option)
			for _, effort := range flattenConfigOptions(option.Options) {
				controls.ReasoningEfforts = append(controls.ReasoningEfforts, effort.Value)
			}
		}
	}
	// A short model ID is an alias only when it is unambiguous across providers.
	aliases := make(map[string]int)
	for _, model := range controls.Models {
		_, short, found := strings.Cut(model.Name, "/")
		if found {
			aliases[short]++
		}
	}
	for i := range controls.Models {
		_, short, found := strings.Cut(controls.Models[i].Name, "/")
		if found && aliases[short] == 1 {
			controls.Models[i].Alias = short
		}
	}
	return controls
}

// Footer/status readers use the live backend state, never a startup preference.
func (s *acpSession) GetModel() string { return s.ModelControls().CurrentModel }

func (s *acpSession) GetReasoningEffort() string { return s.ModelControls().CurrentReasoningEffort }

func (s *acpSession) SetSessionModel(ctx context.Context, model string) error {
	return s.setConfigOption(ctx, "model", model)
}

func (s *acpSession) SetSessionReasoningEffort(ctx context.Context, effort string) error {
	return s.setConfigOption(ctx, "thought_level", effort)
}

func (s *acpSession) setConfigOption(ctx context.Context, category, value string) error {
	if !s.Alive() || s.currentACPSessionID() == "" {
		return fmt.Errorf("acp: session is not running")
	}
	// Send holds this lock until session/prompt finishes. Reject a change during
	// a turn instead of waiting indefinitely behind tool permission requests.
	if !s.sendMu.TryLock() {
		return fmt.Errorf("acp: cannot change configuration during an active prompt")
	}
	defer s.sendMu.Unlock()
	s.config.mu.RLock()
	var selected acpConfigOption
	for _, option := range s.config.options {
		if option.Category == category && option.Type == "select" {
			selected = option
			break
		}
	}
	s.config.mu.RUnlock()
	if selected.ID == "" {
		return fmt.Errorf("acp: server does not advertise %s selection", category)
	}
	valid := false
	for _, option := range flattenConfigOptions(selected.Options) {
		if option.Value == value {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("acp: unknown %s option %q", category, value)
	}
	result, err := s.tr.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": s.currentACPSessionID(), "configId": selected.ID, "value": value,
	})
	if err != nil {
		return fmt.Errorf("acp: set %s: %w", category, err)
	}
	var response struct {
		Options []acpConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("acp: parse config response: %w", err)
	}
	if response.Options == nil {
		return fmt.Errorf("acp: config response has no configOptions")
	}
	s.absorbConfigOptions(response.Options)
	for _, option := range response.Options {
		if option.ID == selected.ID && configValue(option) == value {
			return nil
		}
	}
	return fmt.Errorf("acp: server did not confirm %s %q", category, value)
}
