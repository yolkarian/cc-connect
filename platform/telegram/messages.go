package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	htmltoken "golang.org/x/net/html"
)

type telegramTextChunk struct {
	html string
	text string
}

// splitTelegramHTML counts decoded UTF-16 units, not markup bytes. Each
// fragment closes/reopens its formatting stack, including code and folds.
func splitTelegramHTML(source string, limit int) []telegramTextChunk {
	var chunks []telegramTextChunk
	var markup, plain strings.Builder
	var stack []htmltoken.Token
	units := 0
	flush := func() {
		if units == 0 {
			return
		}
		for i := len(stack) - 1; i >= 0; i-- {
			fmt.Fprintf(&markup, "</%s>", stack[i].Data)
		}
		chunks = append(chunks, telegramTextChunk{markup.String(), plain.String()})
		markup.Reset()
		plain.Reset()
		units = 0
		for _, tag := range stack {
			markup.WriteString(tag.String())
		}
	}
	z := htmltoken.NewTokenizer(strings.NewReader(source))
	for {
		kind := z.Next()
		if kind == htmltoken.ErrorToken {
			break
		}
		token := z.Token()
		switch kind {
		case htmltoken.StartTagToken:
			markup.WriteString(token.String())
			stack = append(stack, token)
		case htmltoken.EndTagToken:
			markup.WriteString(token.String())
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case htmltoken.TextToken:
			for _, r := range token.Data {
				n := 1
				if r > 0xffff {
					n = 2
				}
				if units+n > limit {
					flush()
				}
				plain.WriteRune(r)
				markup.WriteString(html.EscapeString(string(r)))
				units += n
			}
		}
	}
	flush()
	return chunks
}

func sendTelegramChunk(ctx context.Context, bot telegramBot, params *tgbot.SendMessageParams, chunk telegramTextChunk) error {
	params.Text, params.ParseMode = chunk.html, models.ParseModeHTML
	if err := telegramRetryRejected(ctx, func() error { _, err := bot.SendMessage(ctx, params); return err }); err != nil {
		if !strings.Contains(err.Error(), "can't parse") {
			return err
		}
		// Telegram explicitly rejected the HTML, so retrying the same chunk
		// as decoded plain text cannot duplicate an accepted message.
		params.Text, params.ParseMode = chunk.text, ""
		if err := telegramRetryRejected(ctx, func() error { _, err := bot.SendMessage(ctx, params); return err }); err != nil {
			return err
		}
	}
	return nil
}

// Only an explicit 429 rejection is safe to retry. An HTTP timeout may mean
// the message was accepted, so it must never replay the current fragment.
func telegramRetryRejected(ctx context.Context, do func() error) error {
	for attempt := 0; ; attempt++ {
		err := do()
		var rate *tgbot.TooManyRequestsError
		if !errors.As(err, &rate) || attempt >= 3 || rate.RetryAfter < 0 || rate.RetryAfter > 120 {
			return err
		}
		timer := time.NewTimer(time.Duration(rate.RetryAfter) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// FinalizeMessage is synchronous: an accepted timer frame is not proof of
// final delivery. Seal the preview, wait for its in-flight edit, then update
// the first fragment and send the remaining fragments exactly once.
func (p *Platform) FinalizeMessage(ctx context.Context, handle any, content string) error {
	h, ok := handle.(*telegramPreviewHandle)
	if !ok {
		return fmt.Errorf("telegram: invalid preview handle type %T", handle)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	h.finished.Store(true)
	return p.edits.finalize(ctx, h, func() error {
		bot, err := p.connectedBot("finalize message")
		if err != nil {
			return err
		}
		chunks := splitTelegramHTML(core.MarkdownToSimpleHTML(content), telegramMaxMessageLen)
		if len(chunks) == 0 {
			return fmt.Errorf("telegram: empty final message")
		}
		start := 1
		if err := telegramRetryRejected(ctx, func() error { return p.performEdit(ctx, h, chunks[0].html, chunks[0].text) }); err != nil {
			if !telegramEditPermanent(err) {
				return err
			}
			start = 0 // A known-uneditable preview did not accept the first fragment.
		}
		for i := start; i < len(chunks); i++ {
			params := &tgbot.SendMessageParams{ChatID: h.chatID, MessageThreadID: h.threadID}
			if err := sendTelegramChunk(ctx, bot, params, chunks[i]); err != nil {
				return fmt.Errorf("telegram: final fragment %d/%d: %w", i+1, len(chunks), err)
			}
		}
		if start == 0 {
			_ = p.DeletePreviewMessage(ctx, h)
		}
		return nil
	})
}
