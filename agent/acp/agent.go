package acp

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("acp", New)
}

// Agent runs an ACP (Agent Client Protocol) agent subprocess over stdio JSON-RPC.
type Agent struct {
	workDir      string
	cmd          string
	cliExtraArgs []string // extra args from cmd, prepended before args
	args         []string
	staticEnv    map[string]string
	extraEnv     []string
	sessionEnv   []string
	authMethod   string // optional, e.g. "cursor_login" for Cursor CLI (see authenticate RPC)
	displayName  string // optional, for doctor (default "ACP")

	// mode is the pending permission mode to apply to new sessions.
	// When set, StartSession applies it via session/set_mode right after
	// session/new. Empty means "use whatever the agent selects by default".
	mode            string
	model           string
	reasoningEffort string

	// listUnsupported caches a negative result after we probe the agent
	// for sessionCapabilities.list once. Eliminates spawn cost on
	// subsequent `/ls` invocations against agents that don't implement
	// session/list (e.g. some Copilot/OpenClaw builds).
	listUnsupported atomic.Bool

	// modesCache holds the latest `modes` block we observed via
	// session/new or session/load. It's populated by the session
	// handshake so that future PermissionModes() calls can reflect the
	// actual modes this specific ACP agent offers (rather than a
	// hard-coded fallback that may not match).
	modesMu      sync.RWMutex
	modesCache   []core.PermissionModeInfo
	modesCurrent string

	mu           sync.RWMutex
	sessionsMu   sync.Mutex
	liveSessions map[string]*acpSession
}

// sessionCallbacks lets a running acpSession report what it learned
// during the handshake back to its parent Agent. The session is owned
// by cc-connect's engine (not the agent), so without this the agent
// would never see availableModes / capability advertisements.
type sessionCallbacks interface {
	reportModes(block acpModesBlock)
	reportListSupported(supported bool)
}

// Ensure *Agent satisfies sessionCallbacks at compile time.
var _ sessionCallbacks = (*Agent)(nil)

// New builds an acp agent from project options.
// Required: options["command"] — executable name or path for the ACP agent.
// Optional: options["args"], options["env"], options["auth_method"],
// options["display_name"], options["mode"].
func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	cmdStr, cliExtraArgs := core.ParseCmdOpts(opts, "")
	if cmdStr == "" {
		return nil, fmt.Errorf("acp: agent option \"cmd\" or \"command\" is required (path or name of the ACP agent binary)")
	}
	if _, err := exec.LookPath(cmdStr); err != nil {
		return nil, fmt.Errorf("acp: command %q not found in PATH: %w", cmdStr, err)
	}

	args := parseStringSlice(opts["args"])
	staticEnv := envMapFromOpts(opts)
	extra := envPairsFromOpts(opts)
	authMethod, _ := opts["auth_method"].(string)
	authMethod = strings.TrimSpace(authMethod)
	displayName, _ := opts["display_name"].(string)
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = "ACP"
	}
	mode, _ := opts["mode"].(string)
	mode = strings.TrimSpace(mode)
	model, _ := opts["model"].(string)
	reasoning, _ := opts["reasoning_effort"].(string)

	return &Agent{
		workDir:         workDir,
		cmd:             cmdStr,
		cliExtraArgs:    cliExtraArgs,
		args:            args,
		staticEnv:       staticEnv,
		extraEnv:        extra,
		authMethod:      authMethod,
		displayName:     displayName,
		mode:            mode,
		model:           strings.TrimSpace(model),
		reasoningEffort: strings.TrimSpace(reasoning),
	}, nil
}

func envMapFromOpts(opts map[string]any) map[string]string {
	raw, ok := opts["env"]
	if !ok || raw == nil {
		return nil
	}
	switch m := raw.(type) {
	case map[string]string:
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
		return out
	default:
		return nil
	}
}

func envPairsFromOpts(opts map[string]any) []string {
	raw, ok := opts["env"]
	if !ok || raw == nil {
		return nil
	}
	switch m := raw.(type) {
	case map[string]string:
		var out []string
		for k, v := range m {
			out = append(out, k+"="+v)
		}
		return out
	case map[string]any:
		var out []string
		for k, v := range m {
			out = append(out, fmt.Sprintf("%s=%v", k, v))
		}
		return out
	default:
		return nil
	}
}

func parseStringSlice(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			switch t := e.(type) {
			case string:
				out = append(out, t)
			default:
				out = append(out, fmt.Sprint(t))
			}
		}
		return out
	default:
		return nil
	}
}

func (a *Agent) Name() string { return "acp" }

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	a.workDir = dir
	a.mu.Unlock()
	slog.Info("acp: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workDir
}

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	a.sessionEnv = env
	a.mu.Unlock()
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	for id, live := range a.liveSessions {
		if !live.Alive() {
			delete(a.liveSessions, id)
		}
	}
	if live := a.liveSessions[sessionID]; live != nil {
		return nil, fmt.Errorf("acp: session %q is already open", sessionID)
	}
	a.mu.RLock()
	command := a.cmd
	allArgs := append(append([]string{}, a.cliExtraArgs...), a.args...)
	workDir := a.workDir
	authMethod := a.authMethod
	pendingMode := a.mode
	pendingModel := a.model
	pendingReasoning := a.reasoningEffort
	extra := append([]string(nil), a.extraEnv...)
	extra = append(extra, a.sessionEnv...)
	a.mu.RUnlock()

	session, err := newACPSession(ctx, acpSessionConfig{
		command:          command,
		args:             allArgs,
		extraEnv:         extra,
		workDir:          workDir,
		resumeSessionID:  sessionID,
		authMethod:       authMethod,
		initialMode:      pendingMode,
		initialModel:     pendingModel,
		initialReasoning: pendingReasoning,
		callbacks:        a,
	})
	if err != nil {
		return nil, err
	}
	if a.liveSessions == nil {
		a.liveSessions = make(map[string]*acpSession)
	}
	a.liveSessions[session.CurrentSessionID()] = session
	return session, nil
}

func (a *Agent) Stop() error {
	a.sessionsMu.Lock()
	live := a.liveSessions
	a.liveSessions = nil
	a.sessionsMu.Unlock()
	for _, session := range live {
		_ = session.Close()
	}
	return nil
}

// CompressCommand implements core.ContextCompressor. ACP has no dedicated
// compaction RPC; servers that support compaction (e.g. kimi acp) intercept
// "/compact" as a builtin slash command inside session/prompt. Servers
// without it answer with an "unknown command" notice, which is harmless.
func (a *Agent) CompressCommand() string { return "/compact" }

// -- AgentDoctorInfo --

func (a *Agent) CLIBinaryName() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return filepath.Base(a.cmd)
}

func (a *Agent) CLIDisplayName() string {
	a.mu.RLock()
	n := a.displayName
	a.mu.RUnlock()
	if n == "" {
		return "ACP"
	}
	return n
}
