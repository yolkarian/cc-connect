package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// richMessageRecorder stands in for api.telegram.org so the rich-message
// opt-in can be observed without a live bot connection.
type richMessageRecorder struct {
	mu    sync.Mutex
	paths []string
	body  string
	fail  bool
}

func (r *richMessageRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()

	body := `{"ok":true,"result":{"message_id":7,"chat":{"id":1}}}`
	if r.fail {
		body = `{"ok":false,"error_code":400,"description":"Bad Request: unsupported"}`
	} else if req.Body != nil {
		payload, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		r.mu.Lock()
		r.body = string(payload)
		r.mu.Unlock()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (r *richMessageRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func newRichMessagePlatform(recorder *richMessageRecorder, richMessages bool) *Platform {
	return &Platform{
		token:        "token",
		richMessages: richMessages,
		httpClient:   &http.Client{Transport: recorder},
	}
}

// TestRichMessagesDisabledByDefault pins the opt-in contract: without
// rich_messages the adapter must not call the Bot API 10.x endpoint, so older
// Bot API deployments do not pay a failed request per message.
func TestRichMessagesDisabledByDefault(t *testing.T) {
	recorder := &richMessageRecorder{}
	p := newRichMessagePlatform(recorder, false)
	ctx := context.Background()
	rctx := replyContext{chatID: 1, messageID: 2}

	if err := p.Reply(ctx, rctx, "hello"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Reply error = %v, want not connected", err)
	}
	if _, err := p.SendPreviewStart(ctx, rctx, "preview"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("SendPreviewStart error = %v, want not connected", err)
	}
	if calls := recorder.calls(); len(calls) != 0 {
		t.Fatalf("rich message calls without opt-in = %v, want none", calls)
	}
}

func TestRichMessagesOptInSendsRichMessage(t *testing.T) {
	recorder := &richMessageRecorder{}
	p := newRichMessagePlatform(recorder, true)
	ctx := context.Background()
	rctx := replyContext{chatID: 1, messageID: 2}

	if err := p.Reply(ctx, rctx, "hello"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	calls := recorder.calls()
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "/sendRichMessage") {
		t.Fatalf("calls = %v, want one sendRichMessage", calls)
	}
	recorder.mu.Lock()
	payload := recorder.body
	recorder.mu.Unlock()
	if !strings.Contains(payload, `"rich_message"`) || !strings.Contains(payload, "hello") {
		t.Fatalf("payload = %q, want rich_message with markdown body", payload)
	}
}

func TestRichMessagesPreviewTracksRichHandle(t *testing.T) {
	recorder := &richMessageRecorder{}
	p := newRichMessagePlatform(recorder, true)

	handle, err := p.SendPreviewStart(context.Background(), replyContext{chatID: 1}, "preview")
	if err != nil {
		t.Fatalf("SendPreviewStart: %v", err)
	}
	rich, ok := handle.(*telegramPreviewHandle)
	if !ok {
		t.Fatalf("handle type = %T, want *telegramPreviewHandle", handle)
	}
	if !rich.rich {
		t.Fatal("preview created through sendRichMessage must stay on the rich edit path")
	}
}

func TestRichMessagesRejectionFallsBackToChunkedHTML(t *testing.T) {
	recorder := &richMessageRecorder{fail: true}
	p := newRichMessagePlatform(recorder, true)

	// The rich endpoint rejects the payload, so the adapter proceeds to its
	// own send path and reports the missing bot connection instead of the
	// rejected rich message.
	if err := p.Reply(context.Background(), replyContext{chatID: 1, messageID: 2}, "hello"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Reply error = %v, want not connected after rich fallback", err)
	}
	if calls := recorder.calls(); len(calls) != 1 {
		t.Fatalf("calls = %v, want one rejected rich attempt", calls)
	}
}
