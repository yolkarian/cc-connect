package discord

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/chenhg5/cc-connect/core"
)

type fakeCardAPI struct {
	sent    []string
	edits   []string
	sendErr error
	editErr error
	nextID  int
}

func (f *fakeCardAPI) cardSend(_ string, msg *discordgo.MessageSend) (*discordgo.Message, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.nextID++
	if len(msg.Embeds) > 0 {
		f.sent = append(f.sent, msg.Embeds[0].Description)
	} else {
		f.sent = append(f.sent, msg.Content)
	}
	return &discordgo.Message{ID: fmt.Sprintf("m%d", f.nextID)}, nil
}

func (f *fakeCardAPI) cardEdit(_ string, _ string, edit *discordgo.MessageEdit) (*discordgo.Message, error) {
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.edits = append(f.edits, (*edit.Embeds)[0].Description)
	return &discordgo.Message{}, nil
}

func newCardWithFake(api *fakeCardAPI) *discordStreamingCard {
	return &discordStreamingCard{api: api, channelID: "ch-1"}
}

// A card-mode platform must implement the streaming interface; the compact and
// legacy paths must not, so their preview behaviour is unchanged.
func TestCreateStreamingCardOnlyInCardMode(t *testing.T) {
	for _, tc := range []struct {
		style string
		want  bool
	}{
		{"card", true},
		{"compact", false},
		{"legacy", false},
	} {
		t.Run(tc.style, func(t *testing.T) {
			pAny, err := New(map[string]any{"token": "test-token", "progress_style": tc.style})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			_, ok := pAny.(core.StreamingCardPlatform)
			if ok != tc.want {
				t.Fatalf("StreamingCardPlatform = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestStreamingCardPostsOnceThenEdits(t *testing.T) {
	api := &fakeCardAPI{}
	card := newCardWithFake(api)
	ctx := context.Background()

	if err := card.Update(ctx, "first frame"); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 1 || len(api.edits) != 0 {
		t.Fatalf("first frame must post once: sent=%d edits=%d", len(api.sent), len(api.edits))
	}
	// The throttle coalesces an immediate second frame; Finalize ignores it.
	if err := card.Update(ctx, "second frame"); err != nil {
		t.Fatal(err)
	}
	if len(api.edits) != 0 {
		t.Fatalf("throttled update must not edit, got %d edits", len(api.edits))
	}
	if err := card.Finalize(ctx, "final frame"); err != nil {
		t.Fatal(err)
	}
	if len(api.edits) != 1 || api.edits[0] != "final frame" {
		t.Fatalf("finalize must edit in place with the final content, got %v", api.edits)
	}
}

func TestStreamingCardSkipsDuplicateContent(t *testing.T) {
	api := &fakeCardAPI{}
	card := newCardWithFake(api)
	ctx := context.Background()
	_ = card.Update(ctx, "same")
	if err := card.Finalize(ctx, "same"); err != nil {
		t.Fatal(err)
	}
	if len(api.edits) != 0 {
		t.Fatalf("unchanged content must not edit, got %v", api.edits)
	}
}

// Discord rejects an embed description over 4096 characters, so a long final
// answer keeps a completed card and delivers the remainder as follow-ups.
func TestFinalizeSplitsOverlongCard(t *testing.T) {
	api := &fakeCardAPI{}
	card := newCardWithFake(api)
	body := strings.Repeat("推理与答复内容 ", 900)
	if err := card.Finalize(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) < 2 {
		t.Fatalf("want the card plus follow-ups, got %d sends", len(api.sent))
	}
	for i, part := range api.sent {
		if n := len([]rune(part)); n > discordCardDescriptionLimit {
			t.Fatalf("part %d has %d runes, over the embed budget", i, n)
		}
	}
	if !strings.Contains(api.sent[0], "推理") {
		t.Fatal("card must carry the head of the answer")
	}
}

func TestStreamingCardSendFailureFallsBack(t *testing.T) {
	api := &fakeCardAPI{sendErr: fmt.Errorf("404 Unknown Channel")}
	card := newCardWithFake(api)
	if err := card.Update(context.Background(), "frame"); err != nil {
		t.Fatalf("transient send failure must not abort the turn: %v", err)
	}
	if card.Failed() {
		t.Fatal("card must stay retryable after a transient failure")
	}
}

func TestCardStatusColor(t *testing.T) {
	if got := buildDiscordCardEmbed("x", false).Color; got == 0x57F287 {
		t.Fatal("a running card must not use the completed colour")
	}
	if got := buildDiscordCardEmbed("x", true).Color; got != 0x57F287 {
		t.Fatalf("completed card colour = %#x", got)
	}
}
