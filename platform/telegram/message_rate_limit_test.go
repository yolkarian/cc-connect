package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTelegramOutbound_RateLimitBudgetIsNotMultiplied(t *testing.T) {
	for _, operation := range []string{"send", "finalize"} {
		t.Run(operation, func(t *testing.T) {
			recordSleeps(t)
			inner := &rateLimitedBot{stubTelegramBot: newStubTelegramBot(), failures: 100, retryAfter: 0}
			p := &Platform{bot: newRateLimitRetryBot(inner), edits: newTelegramEditThrottler(time.Millisecond)}
			var err error
			if operation == "send" {
				err = p.Send(context.Background(), replyContext{chatID: 42}, "final reply")
			} else {
				err = p.FinalizeMessage(context.Background(), &telegramPreviewHandle{chatID: 42, messageID: 7}, "final reply")
			}
			if err == nil || inner.calls != 3 {
				t.Fatalf("%s made %d calls, want exactly 3 bounded attempts; err=%v", operation, inner.calls, err)
			}
		})
	}
}

func TestTelegramOutbound_UncertainWritesAreNeverReplayed(t *testing.T) {
	for _, operation := range []string{"send", "buttons", "preview", "finalize"} {
		t.Run(operation, func(t *testing.T) {
			inner := newStubTelegramBot()
			unknown := errors.New("timeout awaiting response: acceptance unknown")
			inner.sendErr = unknown
			p := &Platform{bot: inner, edits: newTelegramEditThrottler(time.Millisecond)}
			ctx := context.Background()
			rc := replyContext{chatID: 42}
			var err error
			switch operation {
			case "send":
				err = p.Send(ctx, rc, "final reply")
			case "buttons":
				err = p.SendWithButtons(ctx, rc, "session picker", nil)
			case "preview":
				_, err = p.SendPreviewStart(ctx, rc, "preview")
			case "finalize":
				err = p.FinalizeMessage(ctx, &telegramPreviewHandle{chatID: 42, messageID: 7}, "final reply")
			}
			if !errors.Is(err, unknown) || inner.sendMessageCalls+inner.editMessageTextCalls != 1 {
				t.Fatalf("uncertain %s was replayed: sends=%d edits=%d err=%v", operation, inner.sendMessageCalls, inner.editMessageTextCalls, err)
			}
		})
	}
}
