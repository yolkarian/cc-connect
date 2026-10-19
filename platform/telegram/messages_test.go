package telegram

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type longAnswerBot struct {
	*stubTelegramBot
	mu         sync.Mutex
	sent       []*tgbot.SendMessageParams
	edited     []*tgbot.EditMessageTextParams
	failSend   error
	rejectHTML bool
	editError  error
}

func (b *longAnswerBot) SendMessage(_ context.Context, p *tgbot.SendMessageParams) (*models.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	copy := *p
	b.sent = append(b.sent, &copy)
	if b.rejectHTML && p.ParseMode == models.ParseModeHTML {
		return nil, errors.New("Bad Request: can't parse entities")
	}
	if b.failSend != nil {
		return nil, b.failSend
	}
	return &models.Message{ID: 91}, nil
}

func (b *longAnswerBot) EditMessageText(_ context.Context, p *tgbot.EditMessageTextParams) (*models.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	copy := *p
	b.edited = append(b.edited, &copy)
	if b.editError != nil {
		return nil, b.editError
	}
	return &models.Message{ID: p.MessageID}, nil
}

func decodedChunk(t *testing.T, source string) string {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader("<root>" + source + "</root>"))
	var plain strings.Builder
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unbalanced HTML fragment: %v", err)
		}
		if data, ok := token.(xml.CharData); ok {
			plain.Write(data)
		}
	}
	return plain.String()
}

func TestTelegramLongAnswer_SplitsEntitiesEmojiCodeAndFolds(t *testing.T) {
	body := strings.Repeat("中文🚀 & <tag>\n", 1500)
	source := `<blockquote expandable=""><b>思考</b>` + strings.Repeat("推理", 2500) + `</blockquote><pre><code class="language-go">` + core.MarkdownToSimpleHTML(body) + `</code></pre>`
	want := decodedChunk(t, source)
	var got strings.Builder
	chunks := splitTelegramHTML(source, telegramMaxMessageLen)
	if len(chunks) < 3 {
		t.Fatalf("only %d fragments", len(chunks))
	}
	for _, chunk := range chunks {
		plain := decodedChunk(t, chunk.html)
		if plain != chunk.text || len(utf16.Encode([]rune(plain))) > telegramMaxMessageLen {
			t.Fatal("fragment text changed or exceeded UTF-16 limit")
		}
		got.WriteString(plain)
	}
	if got.String() != want {
		t.Fatal("split response lost or repeated text")
	}
}

func TestTelegramLongAnswer_ReplyAndButtonsPreserveRouting(t *testing.T) {
	bot := &longAnswerBot{stubTelegramBot: newStubTelegramBot()}
	p := &Platform{bot: bot}
	rc := replyContext{chatID: -100, threadID: 7, messageID: 19}
	if err := p.Reply(context.Background(), rc, strings.Repeat("🚀", 5000)); err != nil {
		t.Fatal(err)
	}
	if len(bot.sent) != 3 || bot.sent[0].ReplyParameters.MessageID != 19 {
		t.Fatal("reply lost fragments or original target")
	}
	for i, params := range bot.sent {
		if params.ChatID != int64(-100) || params.MessageThreadID != 7 {
			t.Fatal("wrong chat/topic")
		}
		if i > 0 && params.ReplyParameters != nil {
			t.Fatal("continuation replied again")
		}
	}
	bot.sent = nil
	if err := p.SendWithButtons(context.Background(), rc, strings.Repeat("文", 9000), [][]core.ButtonOption{{{Text: "next", Data: "next"}}}); err != nil {
		t.Fatal(err)
	}
	if len(bot.sent) != 3 || bot.sent[0].ReplyMarkup == nil || bot.sent[1].ReplyMarkup != nil {
		t.Fatal("buttons must appear once")
	}
}

func TestTelegramLongAnswer_FinalizationSupersedesQueuedPreview(t *testing.T) {
	bot := &longAnswerBot{stubTelegramBot: newStubTelegramBot()}
	p := &Platform{bot: bot, edits: newTelegramEditThrottler(40 * time.Millisecond)}
	h := &telegramPreviewHandle{chatID: 1, threadID: 4, messageID: 20}
	ctx := context.Background()
	if err := p.UpdateMessage(ctx, h, "working"); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateMessage(ctx, h, "stale queued frame"); err != nil {
		t.Fatal(err)
	}
	final := "✅ 已完成\n" + strings.Repeat("🚀长回答", 2500)
	if err := p.FinalizeMessage(ctx, h, final); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateMessage(ctx, h, "late stale frame"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.edited) != 2 {
		t.Fatalf("finalization still queued or stale frame escaped: %d edits", len(bot.edited))
	}
	got := decodedChunk(t, bot.edited[1].Text)
	for _, sent := range bot.sent {
		if sent.MessageThreadID != 4 {
			t.Fatal("lost forum topic")
		}
		got += decodedChunk(t, sent.Text)
	}
	if got != final {
		t.Fatal("final answer was truncated or duplicated")
	}
	if len(p.edits.msgs) != 0 {
		t.Fatal("final preview state leaked")
	}
}

func TestTelegramLongAnswer_FinalizationReportsPartialFailure(t *testing.T) {
	bot := &longAnswerBot{stubTelegramBot: newStubTelegramBot(), failSend: errors.New("transport outcome unknown")}
	p := &Platform{bot: bot, edits: newTelegramEditThrottler(time.Second)}
	h := &telegramPreviewHandle{chatID: 1, messageID: 2}
	err := p.FinalizeMessage(context.Background(), h, strings.Repeat("x", 12000))
	if err == nil || len(bot.sent) != 1 || len(bot.edited) != 1 {
		t.Fatal("failed final delivery was swallowed or replayed")
	}
}

func TestTelegramLongAnswer_RejectedHTMLUsesDecodedPlainText(t *testing.T) {
	bot := &longAnswerBot{stubTelegramBot: newStubTelegramBot(), rejectHTML: true}
	p := &Platform{bot: bot}
	if err := p.Send(context.Background(), replyContext{chatID: 1}, "**正文 & 符号**"); err != nil {
		t.Fatal(err)
	}
	if len(bot.sent) != 2 || bot.sent[1].ParseMode != "" || bot.sent[1].Text != "正文 & 符号" {
		t.Fatal("HTML fallback leaked raw markup or lost entities")
	}
}

func TestTelegramLongAnswer_UneditablePreviewDeliversOnceAndCleansUp(t *testing.T) {
	bot := &longAnswerBot{stubTelegramBot: newStubTelegramBot(), editError: errors.New("Bad Request: message to edit not found")}
	p := &Platform{bot: bot, edits: newTelegramEditThrottler(time.Millisecond)}
	h := &telegramPreviewHandle{chatID: 1, messageID: 20, threadID: 3}
	if err := p.FinalizeMessage(context.Background(), h, strings.Repeat("文", 9000)); err != nil {
		t.Fatal(err)
	}
	if len(bot.sent) != 3 || bot.deleteMessageCalls != 1 {
		t.Fatal("uneditable preview caused duplicate/lost response")
	}
}

func TestTelegramLongAnswer_FinalizationWaitsForInFlightEdit(t *testing.T) {
	throttle := newTelegramEditThrottler(time.Millisecond)
	h := &telegramPreviewHandle{chatID: 1, messageID: 2}
	entered, release := make(chan struct{}), make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_ = throttle.edit(context.Background(), h, "old", "old", func(context.Context, *telegramPreviewHandle, string, string) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	finished := make(chan struct{})
	go func() { _ = throttle.finalize(context.Background(), h, func() error { close(finished); return nil }) }()
	select {
	case <-finished:
		t.Fatal("final answer overtook old edit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("final delivery stuck")
	}
	<-oldDone
}
