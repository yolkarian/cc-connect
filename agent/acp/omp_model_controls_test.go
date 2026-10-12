package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if err := os.WriteFile(config, []byte("modelRoles:\n  default: ollama-cloud/glm-5.3-flash:max\n  smol: ollama-cloud/glm-5.3-flash:max\n  task: ollama-cloud/deepseek-v4.1-flash:max\nmemory:\n  backend: mnemopi\nmnemopi:\n  llmMode: none\nautolearn:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := acpSessionConfig{command: "omp", args: []string{"acp"}, workDir: workDir, authMethod: "agent", extraEnv: []string{
		"HOME=" + home, "PI_CODING_AGENT_DIR=" + filepath.Dir(config), "PI_CONFIG_FILES=" + config,
		"MNEMOSYNE_HOME=" + filepath.Join(dir, "memory"), "MNEMOSYNE_DATA_DIR=" + filepath.Join(dir, "memory", "data"),
	}}
	first, err := newACPSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	id := first.CurrentSessionID()
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
		t.Fatalf("native resume lost configuration: %+v", controls)
	}
	if err := second.SetSessionModel(ctx, "ollama-cloud/glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}
	t.Logf("official OMP native model controls passed: uuid=%s models=%d efforts=%v", id, len(controls.Models), controls.ReasoningEfforts)
}
