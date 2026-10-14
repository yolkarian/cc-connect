package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/go-telegram/bot/models"
)

func TestTelegramRecovery_TimeoutRebuildsTransportAndReceivesMessage(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			var polls atomic.Int32
			p := newPollingTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = pollingRequest(t, r)
				switch polls.Add(1) {
				case 1:
					fmt.Fprint(w, `{"ok":true,"result":[]}`)
				case 2:
					if phase == "body" {
						fmt.Fprint(w, `{"ok":true,"result":[`)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				case 3:
					fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":201,"message":{"message_id":20,"date":%d,"text":"timeout recovered","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}}]}`, time.Now().Unix())
				default:
					<-r.Context().Done()
				}
			})
			p.httpClient.Timeout = 200 * time.Millisecond
			clients := make(chan *http.Client, 8)
			p.newBot = func(ctx context.Context, token string, onUpdate func(context.Context, *models.Update), client *http.Client, offset int64, onPollSuccess func()) (telegramBot, *models.User, func(context.Context) error, error) {
				clients <- client
				return defaultNewBot(ctx, token, onUpdate, client, offset, onPollSuccess)
			}
			lifecycle := make(chan string, 8)
			pollErrors := make(chan error, 8)
			p.SetLifecycleHandler(testLifecycleHandler{
				onReady: func(core.Platform) { lifecycle <- "ready" },
				onUnavailable: func(_ core.Platform, err error) {
					lifecycle <- "unavailable"
					pollErrors <- err
				},
			})
			messages := make(chan string, 8)
			if err := p.Start(func(_ core.Platform, msg *core.Message) { messages <- msg.Content }); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"ready", "unavailable", "ready"} {
				if got := waitPollingEvent(t, lifecycle); got != want {
					t.Fatalf("lifecycle = %q, want %q", got, want)
				}
			}
			if err := waitPollingEvent(t, pollErrors); !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "TEST_TOKEN") {
				t.Fatalf("unsafe or non-timeout error: %v", err)
			}
			if got := waitPollingEvent(t, messages); got != "timeout recovered" {
				t.Fatalf("message = %q", got)
			}
			first, second := waitPollingEvent(t, clients), waitPollingEvent(t, clients)
			if first == second || first.Transport == second.Transport || first.Transport == p.httpClient.Transport || second.Transport == p.httpClient.Transport {
				t.Fatal("reconnect reused a client or connection pool")
			}
			if first.Timeout != p.httpClient.Timeout || second.Timeout != p.httpClient.Timeout {
				t.Fatal("reconnect lost HTTP timeout settings")
			}
		})
	}
}

func TestTelegramRecovery_ReconnectPreservesDispatchedOffset(t *testing.T) {
	type poll struct {
		offset  int64
		timeout int
	}
	requests := make(chan poll, 8)
	var polls atomic.Int32
	p := newPollingTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
		offset, timeout := pollingRequest(t, r)
		requests <- poll{offset, timeout}
		switch polls.Add(1) {
		case 1:
			fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":101,"message":{"message_id":10,"date":%d,"text":"queued one","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}},{"update_id":102,"message":{"message_id":11,"date":%d,"text":"queued two","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}}]}`, time.Now().Unix(), time.Now().Unix())
		case 2:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"ok":false,"error_code":502,"description":"temporary outage"}`)
		case 3:
			fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":103,"message":{"message_id":12,"date":%d,"text":"queued three","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}}]}`, time.Now().Unix())
		default:
			<-r.Context().Done()
		}
	})
	messages := make(chan string, 8)
	if err := p.Start(func(_ core.Platform, msg *core.Message) { messages <- msg.Content }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"queued one", "queued two", "queued three"} {
		if got := waitPollingEvent(t, messages); got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	}
	for _, want := range []poll{{1, 0}, {103, telegramPollTimeout}, {103, 0}, {104, telegramPollTimeout}} {
		if got := waitPollingEvent(t, requests); got != want {
			t.Fatalf("poll = %+v, want %+v", got, want)
		}
	}
	select {
	case got := <-messages:
		t.Fatalf("duplicate dispatch: %q", got)
	default:
	}
}

func TestTelegramRecovery_StopDoesNotConfirmUndispatchedBatch(t *testing.T) {
	var polls atomic.Int32
	p := newPollingTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = pollingRequest(t, r)
		polls.Add(1)
		fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":201,"message":{"message_id":20,"date":%d,"text":"first","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}},{"update_id":202,"message":{"message_id":21,"date":%d,"text":"undispatched","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}}]}`, time.Now().Unix(), time.Now().Unix())
	})
	messages := make(chan string, 8)
	if err := p.Start(func(_ core.Platform, msg *core.Message) {
		messages <- msg.Content
		_ = p.Stop()
	}); err != nil {
		t.Fatal(err)
	}
	waitPollingEvent(t, p.loopDone)
	if got := waitPollingEvent(t, messages); got != "first" {
		t.Fatalf("message = %q", got)
	}
	if p.lastUpdateID != 201 || polls.Load() != 1 {
		t.Fatalf("confirmed undispatched batch: lastUpdateID=%d polls=%d", p.lastUpdateID, polls.Load())
	}
	select {
	case got := <-messages:
		t.Fatalf("dispatch after Stop: %q", got)
	default:
	}
}

func TestTelegramRecovery_RepeatedFailuresBackOffAndRespectRetryAfter(t *testing.T) {
	waits := make(chan time.Duration, 8)
	var attempts atomic.Int32
	p := &Platform{
		token:      "TEST_TOKEN",
		httpClient: &http.Client{},
		newBackoffTimer: func(wait time.Duration) backoffTimer {
			waits <- wait
			return immediateTimer(wait)
		},
		newBot: func(_ context.Context, _ string, _ func(context.Context, *models.Update), _ *http.Client, _ int64, onPollSuccess func()) (telegramBot, *models.User, func(context.Context) error, error) {
			attempt := attempts.Add(1)
			return newStubTelegramBot(), &models.User{Username: "testbot"}, func(ctx context.Context) error {
				onPollSuccess()
				if attempt <= 3 {
					return context.DeadlineExceeded
				}
				if attempt == 4 {
					return &telegramPollingAPIError{code: 429, retryAfter: 13}
				}
				<-ctx.Done()
				return ctx.Err()
			}, nil
		},
	}
	t.Cleanup(func() {
		_ = p.Stop()
		waitPollingEvent(t, p.loopDone)
	})
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 13 * time.Second} {
		if got := waitPollingEvent(t, waits); got != want {
			t.Fatalf("backoff = %s, want %s", got, want)
		}
	}
}

func TestTelegramRecovery_StopCancelsAuthenticationAndDuplicateStartRejected(t *testing.T) {
	connectStarted := make(chan struct{})
	var factories atomic.Int32
	p := &Platform{
		token:      "TEST_TOKEN",
		httpClient: &http.Client{},
		newBot: func(ctx context.Context, _ string, _ func(context.Context, *models.Update), _ *http.Client, _ int64, _ func()) (telegramBot, *models.User, func(context.Context) error, error) {
			factories.Add(1)
			close(connectStarted)
			<-ctx.Done()
			return nil, nil, nil, ctx.Err()
		},
	}
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	waitPollingEvent(t, connectStarted)
	if err := p.Start(func(core.Platform, *core.Message) {}); err == nil {
		t.Fatal("duplicate Start would create a second poller")
	}
	_ = p.Stop()
	waitPollingEvent(t, p.loopDone)
	if factories.Load() != 1 {
		t.Fatalf("factories = %d, want 1", factories.Load())
	}
}

func TestTelegramRecovery_GetUpdatesValidatesResponseAndRedactsErrors(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		body  string
		match string
	}{
		{"unauthorized", 401, `{"ok":false,"error_code":401,"description":"Unauthorized TEST_TOKEN"}`, "API error 401"},
		{"conflict", 409, `{"ok":false,"error_code":409,"description":"another consumer"}`, "API error 409"},
		{"rate limit", 429, `{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`, "API error 429"},
		{"malformed", 200, `private TEST_TOKEN body`, "invalid getUpdates response"},
		{"missing result", 200, `{"ok":true}`, "not an update array"},
		{"null result", 200, `{"ok":true,"result":null}`, "not an update array"},
		{"nonarray result", 200, `{"ok":true,"result":{}}`, "not an update array"},
		{"null update", 200, `{"ok":true,"result":[null]}`, "invalid update"},
		{"empty", 200, `{"ok":true,"result":[]}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPollingTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
				if r.ContentLength <= 0 || r.Header.Get("Content-Type") != "application/json" || r.Method != http.MethodPost {
					t.Errorf("poll request is not fixed-length POST JSON")
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			})
			updates, err := getTelegramUpdates(context.Background(), p.httpClient, p.token, 1, 0)
			if tc.match == "" {
				if err != nil || len(updates) != 0 {
					t.Fatalf("updates=%v err=%v", updates, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.match) || strings.Contains(err.Error(), "TEST_TOKEN") || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe or unexpected error: %v", err)
			}
			if tc.code == 429 {
				var apiErr *telegramPollingAPIError
				if !errors.As(err, &apiErr) || apiErr.retryAfter != 7 {
					t.Fatalf("retry_after missing: %v", err)
				}
			}
		})
	}
}

func TestTelegramRecovery_ClientClonePreservesProxyAndHTTP2(t *testing.T) {
	platform, err := New(map[string]any{
		"token": "TEST_TOKEN", "allow_from": "7", "proxy": "socks5://localhost:1080",
		"proxy_username": "test-user", "proxy_password": "test-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	first, second := cloneTelegramHTTPClient(p.httpClient), cloneTelegramHTTPClient(p.httpClient)
	t.Cleanup(first.CloseIdleConnections)
	t.Cleanup(second.CloseIdleConnections)
	if first.Transport == second.Transport || first.Transport == p.httpClient.Transport {
		t.Fatal("client clones share a connection pool")
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.telegram.org", nil)
	for _, client := range []*http.Client{first, second} {
		transport := client.Transport.(*http.Transport)
		proxy, err := transport.Proxy(request)
		if err != nil || proxy.Host != "localhost:1080" || proxy.User.Username() != "test-user" {
			t.Fatalf("proxy config lost: %v", err)
		}
		if password, _ := proxy.User.Password(); password != "test-password" {
			t.Fatal("proxy authentication config lost")
		}
		if !transport.ForceAttemptHTTP2 || transport.TLSHandshakeTimeout == 0 || transport.DialContext == nil || client.Timeout != telegramHTTPTimeout {
			t.Fatal("standard HTTP2/dial/TLS/timeout settings lost")
		}
	}
}
