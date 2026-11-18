package acp

// The ACP adapter keeps model state session-owned: a live acpSession reads its
// model and reasoning choices from the server's configOptions, and only that
// session may change them. The accessors here expose the constructor defaults
// the agent was created with, which apply to a brand new conversation.
//
// Merge note: this file is fork policy. Upstream's ACP agent caches the model
// at agent level instead, so conflicts are resolved by keeping the
// session-owned design and re-exposing any new default the constructor gains.

func (a *Agent) SupportsSessionModelControl() bool { return true }

// These getters expose configured new-session defaults, not live selections.
// Actual model/effort state belongs exclusively to each native acpSession.
func (a *Agent) GetModel() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.model
}

func (a *Agent) GetReasoningEffort() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reasoningEffort
}

func (a *Agent) WorkspaceAgentOptions() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()

	opts := map[string]any{
		"cmd": a.cmd,
	}
	if len(a.args) > 0 {
		opts["args"] = append([]string(nil), a.args...)
	}
	if len(a.staticEnv) > 0 {
		env := make(map[string]string, len(a.staticEnv))
		for k, v := range a.staticEnv {
			env[k] = v
		}
		opts["env"] = env
	}
	if a.authMethod != "" {
		opts["auth_method"] = a.authMethod
	}
	if a.displayName != "" {
		opts["display_name"] = a.displayName
	}
	if a.model != "" {
		opts["model"] = a.model
	}
	if a.reasoningEffort != "" {
		opts["reasoning_effort"] = a.reasoningEffort
	}
	return opts
}
