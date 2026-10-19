package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Telegram renders no native card widgets, so Card 2.0 (display card_mode =
// "rich") is expressed as structured markdown that the existing HTML
// conversion maps onto Telegram's blockquote formatting. The layout mirrors
// the Feishu rich card: status-colored header, collapsible Reasoning/Tools
// panels, streaming markdown body, and a dim status footer.

const (
	// cardPanelStepCap caps tool rows; reasoning is a continuous text panel,
	// not a list of wire fragments or commands.
	cardPanelStepCap   = 10
	cardReasoningRunes = 1100
	// cardStepRunes caps one step row so wide tool inputs stay scannable.
	cardStepRunes = 110
	// cardResultRunes caps the optional tool-result echo line.
	cardResultRunes = 160
	// cardBodyStreamRunes caps the streaming body of intermediate frames.
	// Final frames keep the full body; synchronous FinalizeMessage delivers
	// all overflow fragments instead of queueing an oversized final edit.
	cardBodyStreamRunes = 2400
	// cardEditMinInterval coalesces engine-driven card edits below this pace.
	// Thinking/tool events bypass upstream throttling (Feishu tolerates it),
	// so the Telegram adapter must rate-limit editMessageText itself.
	cardEditMinInterval = 1200 * time.Millisecond
)

// BuildRichCard implements core.RichCardSupporter. statusFooter is a
// pre-composed multi-line string (engine-owned); each line is rendered as its
// own dim (italic) row, separated from the body by a rule.
func (p *Platform) BuildRichCard(status core.CardStatus, title string, steps []core.ToolStep, markdown string, streaming bool, statusFooter string) string {
	_ = title // turn cards carry no custom title; the status header is the title

	var b strings.Builder
	b.WriteString("**")
	b.WriteString(cardStatusHeader(status, streaming))
	b.WriteString("**\n")

	reasoning, tools := cardSplitSteps(steps)

	cardWriteReasoningPanel(&b, reasoning)
	if len(reasoning) == 0 && len(tools) == 0 && streaming {
		// Mirror the Feishu streaming placeholder so the card never starts
		// as an empty frame.
		b.WriteString("\n> [!fold] 💭 推理\n> ⋯\n")
	}
	cardWriteToolPanel(&b, tools)

	if body := strings.TrimSpace(markdown); body != "" {
		b.WriteString("\n")
		b.WriteString(cardStreamBody(body, streaming))
		b.WriteString("\n")
	}

	if footer := strings.TrimSpace(statusFooter); footer != "" {
		b.WriteString("\n")
		b.WriteString(strings.Repeat("—", 10))
		b.WriteString("\n")
		for _, line := range strings.Split(footer, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				b.WriteString("*")
				b.WriteString(line)
				b.WriteString("*\n")
			}
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// cardStatusHeader maps a CardStatus to the bold header line shown at the top
// of every card frame.
func cardStatusHeader(status core.CardStatus, streaming bool) string {
	switch status {
	case core.CardStatusThinking:
		return "🤔 正在思考…"
	case core.CardStatusDone:
		return "✅ 已完成"
	case core.CardStatusError:
		return "❌ 出现错误"
	default:
		if streaming {
			return "⏳ 正在运行…"
		}
		return "⏳ 运行结束"
	}
}

// cardSplitSteps splits the mixed step list into the two collapsible lanes
// the Feishu renderer uses (reasoning first, then tools).
func cardSplitSteps(steps []core.ToolStep) (reasoning []core.ToolStep, tools []core.ToolStep) {
	for _, step := range steps {
		if step.Kind == core.ToolStepKindThinking {
			reasoning = append(reasoning, step)
			continue
		}
		tools = append(tools, step)
	}
	return reasoning, tools
}

// Reasoning retains paragraphs/newlines and has one icon in its heading, not
// a repeated thinking emoji before every line or streamed word fragment.
func cardWriteReasoningPanel(b *strings.Builder, steps []core.ToolStep) {
	var blocks []string
	for _, step := range steps {
		if text := strings.TrimSpace(step.Summary); text != "" {
			blocks = append(blocks, text)
		}
	}
	if len(blocks) == 0 {
		return
	}
	text := strings.Join(blocks, "\n\n")
	if runes := []rune(text); len(runes) > cardReasoningRunes {
		text = string(runes[:cardReasoningRunes]) + "…"
	}
	fmt.Fprintf(b, "\n> [!fold] 💭 推理 (%d)\n", len(blocks))
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// Tool calls remain individually counted/status-marked in a separate fold.
func cardWriteToolPanel(b *strings.Builder, steps []core.ToolStep) {
	if len(steps) == 0 {
		return
	}
	b.WriteString("\n> [!fold] ")
	b.WriteString("🔧 工具")
	b.WriteString(fmt.Sprintf(" (%d)\n", len(steps)))
	hidden := len(steps) - cardPanelStepCap
	if hidden > 0 {
		b.WriteString(fmt.Sprintf("> … 前 %d 步已省略\n", hidden))
		steps = steps[len(steps)-cardPanelStepCap:]
	}
	for _, step := range steps {
		cardWriteToolStep(b, step)
	}
}

func cardWriteToolStep(b *strings.Builder, step core.ToolStep) {
	b.WriteString("> 🔧 **")
	b.WriteString(cardInline(cardToolName(step.Name, step.Summary), 40))
	b.WriteString("**")
	if summary := cardInline(step.Summary, cardStepRunes); summary != "" {
		b.WriteString(" — ")
		b.WriteString(summary)
	}
	switch {
	case step.Success != nil && !*step.Success, step.Status == "failed":
		b.WriteString(" ❌")
	case step.ExitCode != nil && *step.ExitCode != 0:
		b.WriteString(fmt.Sprintf(" ❌ exit %d", *step.ExitCode))
	case !step.Done:
		b.WriteString(" ⏳")
	default:
		b.WriteString(" ✓")
	}
	b.WriteString("\n")

	if result := strings.TrimSpace(step.Result); result != "" {
		b.WriteString("> ↳ ")
		b.WriteString(cardInline(result, cardResultRunes))
		b.WriteString("\n")
	}
}

// reToolCallID matches tool-call identifiers that leak into ACP tool names
// for synthesized sub-calls (Anthropic "toolu_…", OpenAI "call_…" ids); those
// rows render as a generic 工具 row keyed by their summary.
var reToolCallID = regexp.MustCompile("^(toolu_|call_|tooluse_)")

// cardToolName falls back to a generic label when the ACP layer forwarded a
// synthetic tool-call ID instead of a human tool name.
func cardToolName(name, summary string) string {
	if name != "" && !reToolCallID.MatchString(name) {
		return name
	}
	return "工具"
}

// cardInline flattens a step's possibly multi-line text into one quote-safe
// line, capped at maxRunes with an ellipsis.
func cardInline(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return s
}

// cardStreamBody caps intermediate frames to the newest cardBodyStreamRunes
// runes so every frame stays inside Telegram's edit limit.
func cardStreamBody(body string, streaming bool) string {
	if !streaming {
		return body
	}
	runes := []rune(body)
	if len(runes) <= cardBodyStreamRunes {
		return body
	}
	return "…\n" + string(runes[len(runes)-cardBodyStreamRunes:])
}

// telegramEditThrottler coalesces per-message edits so frequent engine card
// updates (thinking/tool events are not throttled upstream) stay inside
// Telegram's comfortable editMessageText pace while always converging to the
// newest frame: an edit above the pace runs immediately; others queue and a
// trailing timer delivers the newest frame once the window reopens.
type telegramEditThrottler struct {
	minInterval time.Duration

	mu   sync.Mutex
	msgs map[string]*telegramEditState
}

type telegramEditState struct {
	lastHTML string
	lastEdit time.Time
	busy     bool
	sealed   bool
	idle     chan struct{}
	pendHTML string
	pendRaw  string
	timer    *time.Timer
}

type telegramEditFn func(ctx context.Context, h *telegramPreviewHandle, html, raw string) error

func newTelegramEditThrottler(minInterval time.Duration) *telegramEditThrottler {
	return &telegramEditThrottler{minInterval: minInterval, msgs: map[string]*telegramEditState{}}
}

func telegramEditKey(h *telegramPreviewHandle) string {
	return fmt.Sprintf("%d/%d/%d", h.chatID, h.threadID, h.messageID)
}

// edit routes one edit attempt: dedupe identical frames, run immediately
// when the pace window is open, otherwise queue the frame behind a trailing
// timer. A nil throttler (not initialized, e.g. in tests) passes through.
func (t *telegramEditThrottler) edit(ctx context.Context, h *telegramPreviewHandle, html, raw string, do telegramEditFn) error {
	if t == nil {
		return do(ctx, h, html, raw)
	}
	key := telegramEditKey(h)
	t.mu.Lock()
	if h.finished.Load() { t.mu.Unlock(); return nil }
	st := t.msgs[key]
	if st == nil {
		st = &telegramEditState{}
		t.msgs[key] = st
	}
	if st.sealed || html == st.lastHTML || html == st.pendHTML {
		t.mu.Unlock()
		return nil
	}
	if st.busy {
		st.pendHTML, st.pendRaw = html, raw
		t.ensureTimerLocked(h, st, t.minInterval, do)
		t.mu.Unlock()
		return nil
	}
	if wait := st.lastEdit.Add(t.minInterval).Sub(time.Now()); wait > 0 {
		st.pendHTML, st.pendRaw = html, raw
		t.ensureTimerLocked(h, st, wait, do)
		t.mu.Unlock()
		return nil
	}
	// Supersede any queued frame: this direct edit carries newer content.
	st.pendHTML, st.pendRaw = "", ""
	st.lastEdit = time.Now()
	st.busy = true
	st.idle = make(chan struct{})
	t.mu.Unlock()

	err := do(ctx, h, html, raw)

	return t.finishEdit(key, html, err)
}

// finishEdit records the outcome of an in-flight edit and removes the handle
// state when the message became permanently uneditable.
func (t *telegramEditThrottler) finishEdit(key, html string, err error) error {
	t.mu.Lock()
	st := t.msgs[key]
	if st != nil {
		st.busy = false
		if st.idle != nil {
			close(st.idle)
			st.idle = nil
		}
		if err == nil {
			st.lastHTML = html
		}
	}
	t.mu.Unlock()
	if err != nil {
		if telegramEditPermanent(err) {
			t.mu.Lock()
			t.dropStateLocked(key)
			t.mu.Unlock()
		}
	}
	return err
}

// flush delivers the newest queued frame for a handle.
func (t *telegramEditThrottler) flush(h *telegramPreviewHandle, do telegramEditFn) {
	key := telegramEditKey(h)
	t.mu.Lock()
	st := t.msgs[key]
	if st == nil {
		t.mu.Unlock()
		return
	}
	st.timer = nil
	if st.sealed || st.pendHTML == "" {
		t.mu.Unlock()
		return
	}
	if st.busy {
		// Wait for the in-flight edit to finish, then try again.
		t.ensureTimerLocked(h, st, t.minInterval, do)
		t.mu.Unlock()
		return
	}
	html := st.pendHTML
	raw := st.pendRaw
	st.pendHTML, st.pendRaw = "", ""
	st.lastEdit = time.Now()
	st.busy = true
	st.idle = make(chan struct{})
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := do(ctx, h, html, raw)
	t.finishEdit(key, html, err)
	if err != nil {
		slog.Warn("telegram: deferred preview edit failed", "error", err)
	}
}

// finalize seals queued frames before waiting for the in-flight request. The
// final caller receives delivery errors instead of a timer's early nil result.
func (t *telegramEditThrottler) finalize(ctx context.Context, h *telegramPreviewHandle, do func() error) error {
	if t == nil {
		return do()
	}
	key := telegramEditKey(h)
	t.mu.Lock()
	st := t.msgs[key]
	if st != nil {
		st.sealed = true
		st.pendHTML, st.pendRaw = "", ""
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
	}
	var idle <-chan struct{}
	if st != nil && st.busy {
		idle = st.idle
	}
	t.mu.Unlock()
	defer t.drop(h)
	if idle != nil {
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return do()
}

// ensureTimerLocked starts the trailing timer if none is active.
func (t *telegramEditThrottler) ensureTimerLocked(h *telegramPreviewHandle, st *telegramEditState, wait time.Duration, do telegramEditFn) {
	if st.timer != nil {
		return
	}
	st.timer = time.AfterFunc(wait, func() { t.flush(h, do) })
}

// dropStateLocked removes and stops all state for a handle key.
func (t *telegramEditThrottler) dropStateLocked(key string) {
	if st, ok := t.msgs[key]; ok {
		if st.timer != nil {
			st.timer.Stop()
		}
		delete(t.msgs, key)
	}
}

// drop forgets a handle (message deleted or permanently uneditable).
func (t *telegramEditThrottler) drop(h *telegramPreviewHandle) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropStateLocked(telegramEditKey(h))
}

// telegramEditPermanent reports whether an edit failure means the target
// message can no longer be edited at all (deleted, blocked, or aged out).
func telegramEditPermanent(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "message can't be edited") ||
		strings.Contains(msg, "message to edit not found") ||
		strings.Contains(msg, "message is not modifiable") ||
		strings.Contains(msg, "MESSAGE_ID_INVALID")
}

// performEdit executes one editMessageText call with the upstream HTML
// fallbacks (plain text on parse errors, not-modified dedupe).
func (p *Platform) performEdit(ctx context.Context, h *telegramPreviewHandle, html, raw string) error {
	bot, err := p.connectedBot("update message")
	if err != nil {
		return err
	}

	params := &tgbot.EditMessageTextParams{
		ChatID:    h.chatID,
		MessageID: h.messageID,
		Text:      html,
		ParseMode: models.ParseModeHTML,
	}

	if _, err := bot.EditMessageText(ctx, params); err != nil {
		errMsg := err.Error()
		slog.Debug("telegram: UpdateMessage HTML failed", "error", errMsg)
		if strings.Contains(errMsg, "not modified") {
			return nil
		}
		if strings.Contains(errMsg, "can't parse") {
			slog.Warn("telegram: HTML rejected by Telegram, editing as plain text",
				"method", "UpdateMessage",
				"error", errMsg,
				"html_prefix", truncateForLog(html, 200),
				"html_len", len(html),
			)
			params.Text = raw
			params.ParseMode = ""
			if _, err2 := bot.EditMessageText(ctx, params); err2 != nil {
				if strings.Contains(err2.Error(), "not modified") {
					return nil
				}
				return fmt.Errorf("telegram: edit message: %w", err2)
			}
			return nil
		}
		return fmt.Errorf("telegram: edit message: %w", err)
	}
	slog.Debug("telegram: UpdateMessage HTML success")
	return nil
}
