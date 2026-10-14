package acp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/core"
)

// Run explicitly inside the production image (not the routine Go suite). Auth
// remains in environment variables; the profile/session files are disposable.
func TestOMPModelControls_NativeACPAcrossProcesses(t *testing.T) {
	if os.Getenv("CC_OMP_NATIVE_CONTROL_TEST") != "1" {
		t.Skip("requires official OMP and an authorized native cloud provider")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	workDir := filepath.Join(dir, "workspace")
	for _, p := range []string{filepath.Join(home, ".omp", "agent"), workDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(home, ".omp", "agent", "config.yml")
	if err := os.WriteFile(config, []byte("modelRoles:\n  default: ollama-cloud/glm-5.3-flash:max\n  smol: ollama-cloud/glm-5.3-flash:max\n  task: ollama-cloud/deepseek-v4.1-flash:max\n  custom: ollama-cloud/deepseek-v4.1-flash:low\nmemory:\n  backend: mnemopi\nmnemopi:\n  noEmbeddings: true\n  llmMode: none\nautolearn:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := acpSessionConfig{command: "omp", args: []string{"acp"}, workDir: workDir, authMethod: "agent", extraEnv: []string{
		"HOME=" + home, "PI_CODING_AGENT_DIR=" + filepath.Dir(config), "PI_CONFIG_FILES=" + config,
		"OMP_PROFILE=default", "PI_PROFILE=", "XDG_DATA_HOME=" + filepath.Join(dir, "data"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"), "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"MNEMOSYNE_HOME=" + filepath.Join(dir, "memory"), "MNEMOSYNE_DATA_DIR=" + filepath.Join(dir, "memory", "data"),
	}}
	// Optionally exercise the deployment's actual constructor preferences.
	// This verifies default delegation by behavior after the role change below,
	// rather than merely asserting that a configuration key is absent.
	if configFile := os.Getenv("CC_OMP_CONTAINER_CONFIG_TEST"); configFile != "" {
		var deployment struct {
			Projects []struct {
				Agent struct {
					Type    string
					Options map[string]any
				}
			}
		}
		if _, err := toml.DecodeFile(configFile, &deployment); err != nil {
			t.Fatalf("deployment config: %v", err)
		}
		found := false
		for _, project := range deployment.Projects {
			if project.Agent.Type == "acp" {
				cfg.initialModel, _ = project.Agent.Options["model"].(string)
				cfg.initialReasoning, _ = project.Agent.Options["reasoning_effort"].(string)
				found = true
				break
			}
		}
		if !found {
			t.Fatal("deployment config has no ACP project")
		}
	}
	first, err := newACPSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	id := first.CurrentSessionID()
	rolesBefore := readOMPModelRoles(t, ctx, cfg)
	controls := first.ModelControls()
	if controls.CurrentModel != "ollama-cloud/glm-5.3-flash" {
		t.Fatalf("native initial model=%q", controls.CurrentModel)
	}
	if err := first.SetSessionModel(ctx, "ollama-cloud/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	if err := first.SetSessionReasoningEffort(ctx, "max"); err != nil {
		t.Fatal(err)
	}
	if first.CurrentSessionID() != id || len(first.events) != 0 {
		t.Fatal("native control changed identity or emitted reply events")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.resumeSessionID = id
	second, err := newACPSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	controls = second.ModelControls()
	if second.CurrentSessionID() != id || controls.CurrentModel != "ollama-cloud/deepseek-v4.1-flash" || controls.CurrentReasoningEffort != "max" {
		t.Fatalf("native resume lost configuration: model=%q effort=%q", controls.CurrentModel, controls.CurrentReasoningEffort)
	}
	if err := second.SetSessionModel(ctx, "ollama-cloud/glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}
	if after := readOMPModelRoles(t, ctx, cfg); !reflect.DeepEqual(after, rolesBefore) {
		t.Fatalf("ACP conversation controls rewrote native roles: before=%v after=%v", rolesBefore, after)
	}
	savedEffort := second.GetReasoningEffort()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	// A changed role default belongs to new processes/conversations, not a
	// retroactive rewrite of a saved session's native selection.
	rolesBefore["default"] = "ollama-cloud/deepseek-v4.1-flash:low"
	rolesBefore["task"] = "ollama-cloud/glm-5.3-flash:max"
	encoded, err := json.Marshal(rolesBefore)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "omp", "config", "set", "modelRoles", string(encoded), "--json")
	command.Dir = cfg.workDir
	command.Env = core.MergeEnv(os.Environ(), cfg.extraEnv)
	if err := command.Run(); err != nil {
		t.Fatalf("native role update: %v", err)
	}
	cfg.resumeSessionID = ""
	third, err := newACPSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if current := third.ModelControls(); current.CurrentModel != "ollama-cloud/deepseek-v4.1-flash" || current.CurrentReasoningEffort != "low" {
		t.Fatalf("new conversation ignored native default role: model=%q effort=%q", current.CurrentModel, current.CurrentReasoningEffort)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.resumeSessionID = id
	fourth, err := newACPSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer fourth.Close()
	if current := fourth.ModelControls(); fourth.CurrentSessionID() != id || current.CurrentModel != "ollama-cloud/glm-5.3-flash" || current.CurrentReasoningEffort != savedEffort {
		t.Fatalf("role default reset the saved conversation: model=%q effort=%q", current.CurrentModel, current.CurrentReasoningEffort)
	}
	t.Logf("official OMP native model controls passed: uuid=%s models=%d efforts=%v rolesPreserved=true", id, len(controls.Models), controls.ReasoningEfforts)
}

func readOMPModelRoles(t *testing.T, ctx context.Context, cfg acpSessionConfig) map[string]string {
	t.Helper()
	command := exec.CommandContext(ctx, "omp", "config", "get", "modelRoles", "--json")
	command.Dir = cfg.workDir
	command.Env = core.MergeEnv(os.Environ(), cfg.extraEnv)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("native role read: %v", err)
	}
	var result struct {
		Key   string            `json:"key"`
		Value map[string]string `json:"value"`
	}
	if err := json.Unmarshal(output, &result); err != nil || result.Key != "modelRoles" || result.Value == nil {
		t.Fatal("native CLI did not return a modelRoles record")
	}
	return result.Value
}
