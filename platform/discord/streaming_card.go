package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/chenhg5/cc-connect/core"
)

const (
	// discordCardEditMinInterval coalesces in-place card edits. Discord applies a
	// per-message edit budget (about five edits per five seconds); streaming
	// thinking/tool events arrive far faster than that.
	discordCardEditMinInterval = 1500 * time.Millisecond
	// discordCardDescriptionLimit is the embed description budget minus headroom
	// for the status line and fence repair performed by the splitter.
	discordCardDescriptionLimit = 3900
)

// discordCardAPI is the subset of *discordgo.Session the streaming card needs.
// Keeping it narrow lets tests drive the card without a live gateway.
type discordCardAPI interface {
	cardSend(channelID string, msg *discordgo.MessageSend) (*discordgo.Message, error)
	cardEdit(channelID, messageID string, edit *discordgo.MessageEdit) (*discordgo.Message, error)
}

type sessionCardAPI struct{ session *discordgo.Session }

func (a sessionCardAPI) cardSend(channelID string, msg *discordgo.MessageSend) (*discordgo.Message, error) {
	return a.session.ChannelMessageSendComplex(channelID, msg)
}

func (a sessionCardAPI) cardEdit(channelID, messageID string, edit *discordgo.MessageEdit) (*discordgo.Message, error) {
	return a.session.ChannelMessageEditComplex(edit)
}

// discordStreamingCard aggregates one agent turn into a single embed message
// that updates in place — the Discord counterpart of the Telegram Card 2.0:
// a coloured status header, a body carrying reasoning/tool panels and the
// streamed answer, and a dim footer. Discord has no collapsible panels, so the
// panels are emitted as spoilers (`||…||`) that the reader can reveal.
// Implements core.StreamingCard.
type discordStreamingCard struct {
	api       discordCardAPI
	channelID string

	mu         sync.Mutex
	messageID  string
	failed     bool
	lastUpdate time.Time
	lastSent   string
}

// cardProgressPlatform exposes core.StreamingCardPlatform only for
// progress_style = "card", so the compact/legacy preview paths stay untouched.
type cardProgressPlatform struct{ *progressPlatform }

// CreateStreamingCard implements core.StreamingCardPlatform for card mode.
func (p *cardProgressPlatform) CreateStreamingCard(ctx context.Context, rctx any) (core.StreamingCard, error) {
	return p.createStreamingCard(ctx, rctx)
}

// CreateStreamingCard prepares a lazy streaming card; the Discord message is
// posted on the first non-empty content so the native typing indicator stays
// visible until there is something to show. Implements
// core.StreamingCardPlatform — when present the engine routes the whole turn
// through this card instead of the per-message preview path.
func (p *Platform) createStreamingCard(ctx context.Context, rctx any) (core.StreamingCard, error) {
	channelID, err := discordCardChannel(rctx)
	if err != nil {
		return nil, err
	}
	if p.session == nil {
		return nil, fmt.Errorf("discord: session not available for streaming card")
	}
	return &discordStreamingCard{
		api:       sessionCardAPI{session: p.session},
		channelID: channelID,
	}, nil
}

func discordCardChannel(rctx any) (string, error) {
	switch rc := rctx.(type) {
	case replyContext:
		return rc.targetChannelID(), nil
	case *interactionReplyCtx:
		return rc.channelID, nil
	default:
		return "", fmt.Errorf("discord: invalid reply context type %T", rctx)
	}
}

// Update renders the latest aggregated turn state. The first post is immediate;
// later edits are coalesced to discordCardEditMinInterval. Transient failures
// are swallowed because Finalize retries with the full content.
func (c *discordStreamingCard) Update(ctx context.Context, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed || strings.TrimSpace(content) == "" {
		return nil
	}
	if c.messageID != "" && time.Since(c.lastUpdate) < discordCardEditMinInterval {
		return nil
	}
	if content == c.lastSent {
		return nil
	}
	if err := c.render(ctx, content, false); err != nil {
		slog.Debug("discord: streaming card update failed (finalize retries)", "error", err)
		return nil
	}
	c.lastUpdate = time.Now()
	c.lastSent = content
	return nil
}

// Finalize writes the final state without throttling. Content that exceeds one
// embed description is delivered as a completed card plus follow-up messages so
// nothing is dropped; the engine sees success and sends no duplicate.
func (c *discordStreamingCard) Finalize(ctx context.Context, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return nil
	}
	if strings.TrimSpace(content) == "" || content == c.lastSent {
		return nil
	}
	head, overflow := splitDiscordCardBody(content, discordCardDescriptionLimit)
	if err := c.renderParts(ctx, head, true); err != nil {
		return err
	}
	c.lastSent = content
	c.lastUpdate = time.Now()
	for _, chunk := range overflow {
		if _, err := c.api.cardSend(c.channelID, &discordgo.MessageSend{Content: chunk}); err != nil {
			return fmt.Errorf("discord: send card overflow: %w", err)
		}
	}
	return nil
}

func (c *discordStreamingCard) Failed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

func (c *discordStreamingCard) render(ctx context.Context, content string, done bool) error {
	head, _ := splitDiscordCardBody(content, discordCardDescriptionLimit)
	return c.renderParts(ctx, head, done)
}

func (c *discordStreamingCard) renderParts(_ context.Context, body string, done bool) error {
	embed := buildDiscordCardEmbed(body, done)
	if c.messageID == "" {
		msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}}
		sent, err := c.api.cardSend(c.channelID, msg)
		if err != nil {
			// Stay retryable: Update swallows this, Finalize retries with the
			// full content and reports a real failure to the engine.
			return fmt.Errorf("discord: send streaming card: %w", err)
		}
		if sent != nil {
			c.messageID = sent.ID
		}
		return nil
	}
	edit := discordgo.NewMessageEdit(c.channelID, c.messageID)
	edit.SetContent("")
	edit.SetEmbeds([]*discordgo.MessageEmbed{embed})
	if _, err := c.api.cardEdit(c.channelID, c.messageID, edit); err != nil {
		return fmt.Errorf("discord: edit streaming card: %w", err)
	}
	return nil
}

// buildDiscordCardEmbed renders the card frame. Discord has no collapsible
// sections, so anything the engine already wrapped in ``||spoiler||`` (or that
// looks like a panel heading) stays reader-revealable; the status colour and
// title carry the turn state that the Telegram card shows as a header line.
func buildDiscordCardEmbed(body string, done bool) *discordgo.MessageEmbed {
	body = strings.TrimSpace(body)
	if body == "" {
		body = "…"
	}
	embed := &discordgo.MessageEmbed{
		Description: trimDiscordRunes(body, discordCardDescriptionLimit),
	}
	if done {
		embed.Color = 0x57F287 // green
	} else {
		embed.Color = 0x5865F2 // discord blurple while working
	}
	return embed
}

// splitDiscordCardBody separates the part that fits one embed description from
// the remainder, splitting code-fence aware so a follow-up message never starts
// with a dangling fence.
func splitDiscordCardBody(content string, limit int) (string, []string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", nil
	}
	if len([]rune(content)) <= limit {
		return content, nil
	}
	chunks := core.SplitMessageCodeFenceAware(content, limit)
	if len(chunks) == 0 {
		return content, nil
	}
	return chunks[0], chunks[1:]
}
