package telegram

import (
	"context"
	"errors"
	"testing"

	tgbot "github.com/go-telegram/bot"
)

func TestTelegramLongAnswer_RetriesOnlyExplicitRateRejection(t *testing.T) {
	attempts := 0
	err := telegramRetryRejected(context.Background(), func() error {
		attempts++
		if attempts == 1 {
			return &tgbot.TooManyRequestsError{Message: "rate limited", RetryAfter: 0}
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("rate rejection was not retried: %v attempts=%d", err, attempts)
	}
	attempts = 0
	unknown := errors.New("timeout awaiting response: acceptance unknown")
	err = telegramRetryRejected(context.Background(), func() error { attempts++; return unknown })
	if err != unknown || attempts != 1 {
		t.Fatal("uncertain write was replayed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts = 0
	err = telegramRetryRejected(ctx, func() error { attempts++; return &tgbot.TooManyRequestsError{Message: "rate limited", RetryAfter: 60} })
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatal("rate-limit wait ignored cancellation")
	}
	attempts = 0
	err = telegramRetryRejected(context.Background(), func() error { attempts++; return &tgbot.TooManyRequestsError{Message: "rate limited", RetryAfter: 0} })
	if err == nil || attempts != 4 {
		t.Fatal("repeated rate rejections were unbounded")
	}
}
