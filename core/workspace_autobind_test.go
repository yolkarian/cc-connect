package core

import (
	"context"
	"path/filepath"
	"testing"
)

// autoBindPlatform reports a channel name so the binding records a label.
type autoBindPlatform struct{ platformNameOnly }

func (autoBindPlatform) ResolveChannelName(string) (string, error) { return "general", nil }
func (autoBindPlatform) Reply(context.Context, any, string) error  { return nil }
func (autoBindPlatform) Send(context.Context, any, string) error   { return nil }

// A channel with no binding must stay unbound unless the opt-in default is
// enabled; enabling it binds the channel to base_dir and records the channel
// name the platform resolved.
func TestResolveWorkspace_AutoBindBaseDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		auto bool
	}{
		{"opt-in disabled leaves channel unbound", false},
		{"opt-in enabled binds base_dir", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			e := newTestEngineWithMultiWorkspace(t, base)
			e.SetWorkspaceAutoBindBaseDir(tc.auto)

			workspace, channelName, err := e.resolveWorkspace(autoBindPlatform{}, "unbound-channel")
			if err != nil {
				t.Fatalf("resolveWorkspace() error = %v", err)
			}

			b, _, usable := e.lookupEffectiveWorkspaceBinding(workspaceChannelKey("discord", "unbound-channel"))
			if !tc.auto {
				if workspace != "" || b != nil {
					t.Fatalf("auto-bind disabled but got workspace=%q binding=%+v", workspace, b)
				}
				return
			}
			want, symErr := filepath.EvalSymlinks(base)
			if symErr != nil {
				want = base
			}
			if workspace != want {
				t.Fatalf("workspace = %q, want %q", workspace, want)
			}
			if channelName != "general" {
				t.Fatalf("channel name = %q, want general", channelName)
			}
			if b == nil || !usable {
				t.Fatalf("expected a usable binding, got %+v usable=%v", b, usable)
			}
		})
	}
}
