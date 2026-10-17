package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Opt in inside the official OMP image. A local provider fixture supplies all
// inference; no cloud credentials, production profile, or Telegram poller.
func TestOMPContextUsage_NativeACPAndResume(t *testing.T) {
	if os.Getenv("CC_OMP_NATIVE_USAGE_TEST") != "1" {
		t.Skip("requires official OMP 18.8.6; uses local inference fixture only")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "meter" {
			t.Errorf("unexpected fixture request/model: %q (%v)", request.Model, err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		n := requests.Add(1)
		input, output := 100, 10
		var delta any = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "usage_bash", "type": "function", "function": map[string]string{"name": "bash", "arguments": `{"command":"printf CONTEXT_TOOL_OK"}`}}}}
		finish := "tool_calls"
		if n >= 2 {
			input, output = 150, 20
			if n >= 3 {
				input, output = 200, 30
			}
			delta = map[string]string{"role": "assistant", "content": "CONTEXT_USAGE_OK"}
			finish = "stop"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(d any, stop any, usage any) {
			value := map[string]any{"id": "usage-fixture", "object": "chat.completion.chunk", "created": 0, "model": "meter", "choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": stop}}}
			if usage != nil {
				value["usage"] = usage
			}
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		frame(delta, nil, nil)
		frame(map[string]any{}, finish, map[string]int{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	profile := filepath.Join(home, ".omp", "agent")
	workDir := filepath.Join(dir, "workspace")
	for _, p := range []string{profile, workDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := "setupVersion: 2\nmodelRoles:\n  default: fixture/meter:low\n  smol: fixture/meter:low\nextensions: []\nstartup:\n  checkUpdate: false\ntools:\n  approvalMode: yolo\nmemory:\n  backend: mnemopi\nmnemopi:\n  noEmbeddings: true\n  llmMode: none\nautolearn:\n  enabled: false\n"
	if err := os.WriteFile(filepath.Join(profile, "config.yml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	models := fmt.Sprintf("providers:\n  fixture:\n    baseUrl: %s/v1\n    api: openai-completions\n    apiKey: fixture-not-a-secret\n    models:\n      - id: meter\n        reasoning: true\n        input: [text]\n        contextWindow: 1000000\n        maxTokens: 4096\n", server.URL)
	if err := os.WriteFile(filepath.Join(profile, "models.yml"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := acpSessionConfig{command: "omp", args: []string{"acp"}, workDir: workDir, authMethod: "agent", extraEnv: []string{
		"HOME=" + home, "PI_CODING_AGENT_DIR=" + profile, "PI_CONFIG_FILES=" + filepath.Join(profile, "config.yml"),
		"OMP_PROFILE=default", "PI_PROFILE=", "TMPDIR=" + dir,
		"XDG_DATA_HOME=" + filepath.Join(dir, "data"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"MNEMOSYNE_HOME=" + filepath.Join(dir, "memory"), "MNEMOSYNE_DATA_DIR=" + filepath.Join(dir, "memory", "data"),
	}}
	first, err := newACPSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	id := first.CurrentSessionID()
	run := func(s *acpSession, wantUsed, wantInput, wantOutput int) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- s.Send("Run the local context fixture.", "", nil, nil) }()
		for {
			select {
			case event := <-s.Events():
				if event.Type == core.EventPermissionRequest {
					t.Fatal("native yolo fixture requested permission")
				}
				if event.Type == core.EventError {
					t.Fatalf("native fixture error: %v", event.Error)
				}
				if event.Type != core.EventResult {
					continue
				}
				usage := s.GetContextUsage()
				if usage == nil || !usage.UsedTokensKnown || usage.UsedTokens != wantUsed || usage.ContextWindow != 1000000 || usage.InputTokens != wantInput || usage.OutputTokens != wantOutput || usage.TotalTokens != 0 {
					t.Fatalf("native context and turn totals were not kept distinct: %+v", usage)
				}
			case <-ctx.Done():
				t.Fatal("native context fixture timed out")
			}
			break
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// OMP's authoritative meter anchors on prompt context, not prompt+output.
	run(first, 150, 250, 30)
	if requests.Load() != 2 {
		t.Fatalf("unexpected first-turn model requests: %d", requests.Load())
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
	if second.CurrentSessionID() != id || len(second.events) != 0 {
		t.Fatal("resume changed identity or replayed historical events")
	}
	run(second, 200, 200, 30)
	if requests.Load() != 3 {
		t.Fatalf("unexpected resumed model requests: %d", requests.Load())
	}
	t.Logf("official OMP context usage passed: UUID preserved, current=200/1000000, resumed turn=200+30, requests=%d", requests.Load())
}
