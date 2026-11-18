package acp

import (
	"log/slog"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Mode switching is shared behaviour with upstream: the engine treats
// ModeSwitcher as the point of truth for both the `/mode` picker and applying a
// selection, and the Key stays the ACP modeId so `session/set_mode` needs no
// translation. Kept in its own file so upstream's protocol work in agent.go
// does not collide with it.

// SetMode stores a permission mode to apply to future sessions started
// via StartSession. If the caller-provided mode matches a known cached
// mode id (case-insensitive), it is normalised to that id. Otherwise
// it is stored as-is — some IM users may configure modes before the
// agent has started any session and thus advertised its mode list.
func (a *Agent) SetMode(mode string) {
	normalised := mode
	if m := a.matchModeID(mode); m != "" {
		normalised = m
	}
	a.mu.Lock()
	a.mode = normalised
	a.mu.Unlock()
	slog.Info("acp: mode changed for future sessions", "mode", normalised)
}

// GetMode returns the mode cc-connect will treat as "current" when
// rendering the `/mode` picker or applying SetLiveMode.
//
// Precedence: the most recent explicit SetMode wins (that's the user's
// intent — `/mode plan` should immediately be reflected in the next
// `/mode` listing even before the session/set_mode RPC has returned).
// Only if no one has ever called SetMode for this Agent do we fall
// back to whatever the server advertised as currentModeId during the
// last handshake.
func (a *Agent) GetMode() string {
	a.mu.RLock()
	pending := a.mode
	a.mu.RUnlock()
	if pending != "" {
		return pending
	}
	a.modesMu.RLock()
	defer a.modesMu.RUnlock()
	return a.modesCurrent
}

// PermissionModes returns the modes this ACP agent offers. The list is
// populated from the latest `modes.availableModes` observed on
// session/new or session/load; before the first successful handshake
// it returns an empty slice, and the engine will hide the mode picker.
//
// ACP doesn't send per-mode Desc/NameZh, so Description (if the server
// sent one) maps to Desc for both locales. IM-side translators are
// free to map well-known ids to localised strings later.
func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	a.modesMu.RLock()
	defer a.modesMu.RUnlock()
	out := make([]core.PermissionModeInfo, len(a.modesCache))
	copy(out, a.modesCache)
	return out
}

// matchModeID returns the canonical mode id for a user-typed string
// (case-insensitive match on id or display name). Empty string if no
// match or if we haven't observed modes yet.
func (a *Agent) matchModeID(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	lower := strings.ToLower(input)
	a.modesMu.RLock()
	defer a.modesMu.RUnlock()
	for _, m := range a.modesCache {
		if strings.ToLower(m.Key) == lower || strings.ToLower(m.Name) == lower {
			return m.Key
		}
	}
	return ""
}

// -- sessionCallbacks impl --

func (a *Agent) reportModes(block acpModesBlock) {
	infos := make([]core.PermissionModeInfo, 0, len(block.AvailableModes))
	for _, m := range block.AvailableModes {
		infos = append(infos, core.PermissionModeInfo{
			Key:    m.ID,
			Name:   m.Name,
			NameZh: m.Name,
			Desc:   m.Description,
			DescZh: m.Description,
		})
	}
	a.modesMu.Lock()
	a.modesCache = infos
	a.modesCurrent = block.CurrentModeID
	a.modesMu.Unlock()
}

func (a *Agent) reportListSupported(supported bool) {
	if !supported {
		a.listUnsupported.Store(true)
	} else {
		a.listUnsupported.Store(false)
	}
}
