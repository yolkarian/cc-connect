package telegram

import (
	"context"
	"errors"
	"log/slog"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	// rateLimitMaxAttempts bounds how often one request is sent when Telegram
	// keeps answering 429 Too Many Requests.
	rateLimitMaxAttempts = 3
	// rateLimitMaxWait is the longest retry_after we are willing to sleep for;
	// a longer one is returned to the caller as the error it is.
	rateLimitMaxWait = 120 * time.Second
)

// sleepCtx waits for d or until ctx is done. Replaced in tests.
var sleepCtx = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rateLimitRetryBot retries text sends and edits that Telegram rejects with
// 429 Too Many Requests, after the retry_after it asks for. Without this a
// rate-limited message, often the final reply of a turn, was dropped.
//
// Uploads (photos, documents, audio) are not retried: their params can carry
// an io.Reader that the first attempt has already consumed.
type rateLimitRetryBot struct {
	telegramBot
}

func newRateLimitRetryBot(b telegramBot) telegramBot {
	if _, ok := b.(*rateLimitRetryBot); ok {
		return b
	}
	return &rateLimitRetryBot{telegramBot: b}
}

func (b *rateLimitRetryBot) SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	return retryOnRateLimit(ctx, "sendMessage", func() (*models.Message, error) {
		return b.telegramBot.SendMessage(ctx, params)
	})
}

func (b *rateLimitRetryBot) EditMessageText(ctx context.Context, params *tgbot.EditMessageTextParams) (*models.Message, error) {
	return retryOnRateLimit(ctx, "editMessageText", func() (*models.Message, error) {
		return b.telegramBot.EditMessageText(ctx, params)
	})
}

func retryOnRateLimit[T any](ctx context.Context, method string, call func() (T, error)) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		res, err := call()
		var tooMany *tgbot.TooManyRequestsError
		if err == nil || !errors.As(err, &tooMany) || tooMany == nil || attempt >= rateLimitMaxAttempts {
			return res, err
		}
		// Only explicit, bounded rejections may be retried. Check before
		// converting seconds to Duration, which could otherwise overflow.
		if tooMany.RetryAfter < 0 || int64(tooMany.RetryAfter) > int64(rateLimitMaxWait/time.Second) {
			return res, err
		}
		wait := time.Duration(tooMany.RetryAfter) * time.Second
		slog.Warn("telegram: rate limited, retrying", "method", method, "retry_after", wait, "attempt", attempt)
		if sleepErr := sleepCtx(ctx, wait); sleepErr != nil {
			return zero, sleepErr
		}
	}
}
