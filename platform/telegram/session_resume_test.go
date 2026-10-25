package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type sessionPickerBot struct {
	*stubTelegramBot
	messages []*tgbot.SendMessageParams
}

func (b *sessionPickerBot) SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	b.messages = append(b.messages, params)
	return b.stubTelegramBot.SendMessage(ctx, params)
}

type sessionPickerAgent struct {
	listed []core.AgentSessionInfo
}

func (a *sessionPickerAgent) Name() string { return "fixture" }
func (a *sessionPickerAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return append([]core.AgentSessionInfo(nil), a.listed...), nil
}
func (a *sessionPickerAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return nil, fmt.Errorf("session picker must not prompt or load a model")
}
func (a *sessionPickerAgent) Stop() error { return nil }

func TestTelegramNewThenResume_FromSessionPickerCallback(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, command := range []string{"/list", "/switch"} {
			t.Run(fmt.Sprintf("Group=%t%s", group, command), func(t *testing.T) {
				bot := &sessionPickerBot{stubTelegramBot: newStubTelegramBot()}
				p := &Platform{bot: bot, groupReplyAll: true}
				chat := models.Chat{ID: 123, Type: models.ChatTypePrivate}
				if group {
					chat.ID, chat.Type = -555, models.ChatTypeGroup
				}
				key := p.buildSessionKey(chat.ID, 0, 123)
				const original = "00000000-0000-7000-8000-000000000001"
				path := filepath.Join(t.TempDir(), "sessions.json")
				store := core.NewSessionManager(path)
				saved := store.GetOrCreateActive(key)
				saved.SetAgentSessionID(original, "fixture")
				saved.AddHistory("user", "original question")
				saved.AddHistory("assistant", "original retained answer")
				store.Save()
				a := &sessionPickerAgent{listed: []core.AgentSessionInfo{{ID: original, Summary: "original", MessageCount: 2}, {ID: "different-uuid", Summary: "other"}}}
				e := core.NewEngine("resume", a, []core.Platform{p}, path, core.LangEnglish)
				defer e.Stop()
				p.handler = e.ReceiveMessage
				sequence := 0
				send := func(text string) {
					sequence++
					p.handleMessage(context.Background(), &models.Message{ID: sequence, Date: int(time.Now().Unix()), Chat: chat, From: &models.User{ID: 123}, Text: text})
				}
				send("/new")
				send(command)
				if len(bot.messages) < 2 {
					t.Fatalf("no picker delivered: %v", bot.messages)
				}
				picker := bot.messages[len(bot.messages)-1]
				keyboard, ok := picker.ReplyMarkup.(*models.InlineKeyboardMarkup)
				if !ok || len(keyboard.InlineKeyboard) == 0 {
					t.Fatalf("picker has no inline keyboard: %#v", picker.ReplyMarkup)
				}
				choice := keyboard.InlineKeyboard[0][0].CallbackData
				if len(choice) > 64 {
					t.Fatalf("callback exceeds Telegram limit: %q", choice)
				}
				a.listed[0], a.listed[1] = a.listed[1], a.listed[0]
				// Decode a real Telegram callback envelope and use the adapter's
				// allowlist/session-key/command routing, not an engine shortcut.
				data, err := json.Marshal(map[string]any{"id": "fixture-callback", "from": models.User{ID: 123}, "data": choice, "message": models.Message{ID: 99, Date: int(time.Now().Unix()), Chat: chat, Text: picker.Text}})
				if err != nil {
					t.Fatal(err)
				}
				var callback models.CallbackQuery
				if err := json.Unmarshal(data, &callback); err != nil {
					t.Fatal(err)
				}
				p.handleCallbackQuery(context.Background(), &callback)
				send("/history 10")
				if !strings.Contains(bot.messages[len(bot.messages)-1].Text, "original retained answer") {
					t.Fatalf("callback resumed the wrong conversation: %s", bot.messages[len(bot.messages)-1].Text)
				}
				reloaded := core.NewSessionManager(path)
				if current := reloaded.GetOrCreateActive(key); current.ID != saved.ID || current.GetAgentSessionID() != original {
					t.Fatalf("callback lost original identity: %s/%s", current.ID, current.GetAgentSessionID())
				}
			})
		}
	}
}
