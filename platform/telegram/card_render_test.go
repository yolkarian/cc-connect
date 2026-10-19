package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Ensure the Telegram platform opts into the engine's Card 2.0 pipeline.
var _ core.RichCardSupporter = (*Platform)(nil)

var cardRule = strings.Repeat("—", 10)

func TestTelegramBuildRichCardStates(t *testing.T) {
	tests := []struct {
		name      string
		status    core.CardStatus
		steps     []core.ToolStep
		markdown  string
		streaming bool
		footer    string
		wantSubs  []string
	}{
		{
			name:      "streaming thinking placeholder",
			status:    core.CardStatusThinking,
			markdown:  "",
			streaming: true,
			wantSubs:  []string{"**🤔 正在思考…**", "> [!fold] 💭 推理\n> ⋯"},
		},
		{
			name:   "working with panels",
			status: core.CardStatusWorking,
			steps: []core.ToolStep{
				{Kind: core.ToolStepKindThinking, Name: "Thinking", Summary: "先检查目录", Done: true},
				{Name: "Bash", Summary: "git status --short", Status: "running"},
				{Name: "Read", Summary: "文件读取", Done: true, Success: boolPtr(true)},
			},
			markdown:  "正文内容",
			streaming: true,
			wantSubs: []string{
				"**⏳ 正在运行…**", "> [!fold] 💭 推理 (1)", "> 先检查目录",
				"> [!fold] 🔧 工具 (2)", "> 🔧 **Bash** — git status --short ⏳",
				"> 🔧 **Read** — 文件读取 ✓", "\n正文内容",
			},
		},
		{
			name:      "done with failed tool and footer",
			status:    core.CardStatusDone,
			steps:     []core.ToolStep{{Name: "Bash", Summary: "make test", Done: true, ExitCode: intPtr(2)}},
			markdown:  "结果正文",
			streaming: false,
			footer:    "⏱ 用时 1分23秒\ngpt-5.5 · xhigh · out 1.2k",
			wantSubs:  []string{"**✅ 已完成**", "❌ exit 2", cardRule, "*⏱ 用时 1分23秒*", "*gpt-5.5 · xhigh · out 1.2k*"},
		},
		{
			name:     "error status",
			status:   core.CardStatusError,
			markdown: "部分输出",
			wantSubs: []string{"**❌ 出现错误**", "部分输出"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Platform{}
			out := p.BuildRichCard(tt.status, "", tt.steps, tt.markdown, tt.streaming, tt.footer)
			for _, want := range tt.wantSubs {
				if !strings.Contains(out, want) {
					t.Fatalf("card missing %q\ncard:\n%s", want, out)
				}
			}
		})
	}
}

func TestTelegramReasoningPanel_PreservesParagraphsWithoutPerLineEmoji(t *testing.T) {
	p := &Platform{}
	thought := "I should respond\nbriefly with confirmation.\n\nSecond paragraph."
	steps := []core.ToolStep{{Kind: core.ToolStepKindThinking, Summary: thought, Done: true}}
	for _, streaming := range []bool{true, false} {
		card := p.BuildRichCard(core.CardStatusWorking, "", steps, "ANSWER_ONLY", streaming, "")
		html := core.MarkdownToSimpleHTML(card)
		reasoning := strings.Split(html, "</blockquote>")[0]
		if !strings.Contains(reasoning, thought) || strings.Count(reasoning, "💭") != 1 || strings.Contains(reasoning, "🤔") {
			t.Fatalf("reasoning paragraphs were flattened or decorated per line: %s", html)
		}
		if strings.Contains(reasoning, "ANSWER_ONLY") {
			t.Fatal("answer leaked into reasoning")
		}
	}
}

func TestTelegramBuildRichCardHasSeparateReasoningAndToolFolds(t *testing.T) {
	p := &Platform{}
	steps := []core.ToolStep{
		{Kind: core.ToolStepKindThinking, Summary: "THOUGHT_1", Done: true},
		{Kind: core.ToolStepKindTool, Name: "Bash", Summary: "TOOL_CALL", Result: "TOOL_RESULT", Done: true},
		{Kind: core.ToolStepKindThinking, Summary: "THOUGHT_2", Done: true},
	}
	for _, streaming := range []bool{true, false} {
		card := p.BuildRichCard(core.CardStatusWorking, "", steps, "ANSWER_ONLY", streaming, "")
		html := core.MarkdownToSimpleHTML(card)
		if strings.Count(html, "<blockquote expandable>") != 2 {
			t.Fatalf("want one reasoning fold and one tool fold, got %s", html)
		}
		parts := strings.Split(html, "</blockquote>")
		if len(parts) != 3 {
			t.Fatalf("unexpected panel structure: %s", html)
		}
		if !strings.Contains(parts[0], "THOUGHT_1") || !strings.Contains(parts[0], "THOUGHT_2") ||
			strings.Contains(parts[0], "TOOL_CALL") || strings.Contains(parts[0], "ANSWER_ONLY") {
			t.Fatalf("reasoning panel contains the wrong channel: %s", html)
		}
		if !strings.Contains(parts[1], "TOOL_CALL") || !strings.Contains(parts[1], "TOOL_RESULT") ||
			strings.Contains(parts[1], "THOUGHT_") || strings.Contains(parts[1], "ANSWER_ONLY") {
			t.Fatalf("tool panel contains the wrong channel: %s", html)
		}
		if !strings.Contains(parts[2], "ANSWER_ONLY") || strings.Contains(parts[2], "THOUGHT_") ||
			strings.Contains(parts[2], "TOOL_") {
			t.Fatalf("answer body contains panel content: %s", html)
		}
	}
}

func TestTelegramBuildRichCardCollapsesOldSteps(t *testing.T) {
	p := &Platform{}
	steps := make([]core.ToolStep, 0, 16)
	for i := 0; i < 16; i++ {
		steps = append(steps, core.ToolStep{Name: fmt.Sprintf("Tool%d", i), Summary: "s", Done: true, Success: boolPtr(true)})
	}
	out := p.BuildRichCard(core.CardStatusDone, "", steps, "body", false, "")
	if !strings.Contains(out, "> [!fold] 🔧 工具 (16)") {
		t.Fatalf("panel count missing:\n%s", out)
	}
	if !strings.Contains(out, "> … 前 6 步已省略") {
		t.Fatalf("hidden placeholder missing:\n%s", out)
	}
	if strings.Contains(out, "**Tool5**") {
		t.Fatalf("oldest steps should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "**Tool15**") {
		t.Fatalf("newest steps should be kept:\n%s", out)
	}
	if got := strings.Count(out, "> 🔧"); got != cardPanelStepCap {
		t.Fatalf("rendered tool rows = %d, want %d", got, cardPanelStepCap)
	}
}

func TestTelegramBuildRichCardTruncatesAndStreamCapsBody(t *testing.T) {
	p := &Platform{}
	long := strings.Repeat("长", cardStepRunes+50)
	out := p.BuildRichCard(core.CardStatusWorking, "", []core.ToolStep{{Name: "Bash", Summary: long}}, "正文", true, "")
	if strings.Contains(out, strings.Repeat("长", cardStepRunes+1)) {
		t.Fatal("step summary not truncated")
	}
	if !strings.Contains(out, "> 🔧 **Bash** — "+strings.Repeat("长", cardStepRunes)+"…") {
		t.Fatalf("truncated step text unexpected:\n%s", out)
	}

	body := strings.Repeat("流", cardBodyStreamRunes+300)
	streamed := p.BuildRichCard(core.CardStatusWorking, "", nil, body, true, "")
	if got := strings.Count(streamed, "流"); got != cardBodyStreamRunes {
		t.Fatalf("streaming body runes = %d, want %d", got, cardBodyStreamRunes)
	}
	if !strings.HasSuffix(streamed, strings.Repeat("流", cardBodyStreamRunes)) {
		t.Fatal("streaming body must keep the newest suffix")
	}

	final := p.BuildRichCard(core.CardStatusDone, "", nil, body, false, "")
	if got := strings.Count(final, "流"); got != cardBodyStreamRunes+300 {
		t.Fatalf("final body was truncated (%d)", got)
	}
}

func TestTelegramBuildRichCardConvertsToSafeHTML(t *testing.T) {
	p := &Platform{}
	out := p.BuildRichCard(core.CardStatusWorking, "", []core.ToolStep{{Name: "Bash", Summary: "echo '<script>' | x"}}, "<b>注入</b>正文", true, "")
	html := core.MarkdownToSimpleHTML(out)
	if strings.Contains(html, "<script>") {
		t.Fatalf("unescaped HTML leaked: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expect escaped script tag: %s", html)
	}
	if !strings.Contains(html, "<blockquote expandable>") {
		t.Fatalf("expandable panel missing: %s", html)
	}
	if !strings.Contains(html, "<b") {
		t.Fatalf("bold markup missing: %s", html)
	}
}

func TestTelegramEditThrottlerCoalescesAndConverges(t *testing.T) {
	rec := &recordingEditBot{stubTelegramBot: newStubTelegramBot()}
	p := &Platform{edits: newTelegramEditThrottler(250 * time.Millisecond)}
	p.bot = rec
	h := &telegramPreviewHandle{chatID: -100, threadID: 0, messageID: 42}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First edit goes through immediately.
	if err := p.UpdateMessage(ctx, h, "frame-1"); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	// Rapid second edit is coalesced onto the trailing timer.
	if err := p.UpdateMessage(ctx, h, "frame-2"); err != nil {
		t.Fatalf("second edit: %v", err)
	}
	// A duplicate frame queued again must not queue a second edit.
	if err := p.UpdateMessage(ctx, h, "frame-2"); err != nil {
		t.Fatalf("dup edit: %v", err)
	}
	if got := rec.editCount(); got != 1 {
		t.Fatalf("edits executed before trailing window = %d, want 1", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for rec.editCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got, texts := rec.editCount(), rec.texts(); got != 2 || texts[len(texts)-1] != core.MarkdownToSimpleHTML("frame-2") {
		t.Fatalf("trailing flush delivered edits=%d texts=%q", got, texts)
	}

	// Dedupe: identical content must not trigger another API call.
	time.Sleep(300 * time.Millisecond)
	if err := p.UpdateMessage(ctx, h, "frame-2"); err != nil {
		t.Fatalf("dedup edit: %v", err)
	}
	if got := rec.editCount(); got != 2 {
		t.Fatalf("dedup edit passed through, edits=%d", got)
	}

	// Drop cleans up state; a later edit runs immediately (fresh state).
	p.edits.drop(h)
	if err := p.UpdateMessage(ctx, h, "frame-3"); err != nil {
		t.Fatalf("post-drop edit: %v", err)
	}
	if got, texts := rec.editCount(), rec.texts(); got != 3 || texts[len(texts)-1] != core.MarkdownToSimpleHTML("frame-3") {
		t.Fatalf("post-drop edit missing, edits=%d texts=%q", got, texts)
	}
}

func TestMarkdownFoldCalloutToExpandableQuote(t *testing.T) {
	out := core.MarkdownToSimpleHTML("> [!fold] 💭 推理 (2)\n> 第一段\n> 第二段")
	if !strings.Contains(out, "<blockquote expandable><b>💭 推理 (2)</b>\n第一段\n第二段</blockquote>") {
		t.Fatalf("fold callout not converted: %s", out)
	}
	// Other callout types keep the upstream rendering.
	info := core.MarkdownToSimpleHTML("> [!info] Note\n> body")
	if !strings.Contains(info, "<blockquote><b>info: Note</b>") || strings.Contains(info, "expandable") {
		t.Fatalf("non-fold callouts must stay plain: %s", info)
	}
}

func TestTelegramBuildRichCardMasksToolCallIDs(t *testing.T) {
	p := &Platform{}
	out := p.BuildRichCard(core.CardStatusDone, "", []core.ToolStep{{ToolCallID: "toolu_01AbCdEfGh123456", Name: "toolu_01AbCdEfGh123456", Summary: "列出文件", Done: true, Success: boolPtr(true)}, {ToolCallID: "call_999xyz", Name: "call_999xyz", Done: false}, {ToolCallID: "ollama:1:bash", Name: "ollama:1:bash", Done: true}, {ToolCallID: "arbitrary-opaque-id", Name: "arbitrary-opaque-id", Done: true}}, "正文", false, "")
	if strings.Contains(out, "toolu_01AbCdEfGh123456") || strings.Contains(out, "call_999xyz") || strings.Contains(out, "ollama:1:bash") || strings.Contains(out, "arbitrary-opaque-id") {
		t.Fatalf("tool-call id leaked into card:\n%s", out)
	}
	if !strings.Contains(out, "> 🔧 **工具** — 列出文件 ✓") || !strings.Contains(out, "> 🔧 **工具** ⏳") {
		t.Fatalf("generic tool rows missing:\n%s", out)
	}
}

func TestTelegramEditPermanentDetection(t *testing.T) {
	if !telegramEditPermanent(fmt.Errorf("telegram: edit message: Bad Request: message to edit not found")) {
		t.Fatal("message to edit not found should be permanent")
	}
	if telegramEditPermanent(fmt.Errorf("telegram: edit message: Too Many Requests: retry after 3")) {
		t.Fatal("rate limit should not be permanent")
	}
}

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

// recordingEditBot records EditMessageText payloads while delegating to the
// shared stub for the rest of the telegramBot surface.
type recordingEditBot struct {
	*stubTelegramBot

	mu    sync.Mutex
	plots []string
}

func (r *recordingEditBot) EditMessageText(_ context.Context, params *tgbot.EditMessageTextParams) (*models.Message, error) {
	r.mu.Lock()
	r.plots = append(r.plots, params.Text)
	r.mu.Unlock()
	return &models.Message{ID: params.MessageID}, nil
}

func (r *recordingEditBot) editCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.plots)
}

func (r *recordingEditBot) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.plots...)
}
