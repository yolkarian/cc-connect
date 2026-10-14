package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Exercise the production factory and receive loop against a local HTTP/2 Bot
// API. No requests leave the test server and no real Telegram consumer exists.
func newPollingTestPlatform(t *testing.T, handler http.HandlerFunc) *Platform {
	t.Helper()
	var requests sync.WaitGroup
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		defer requests.Done()
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"Test","username":"testbot"}}`)
			return
		}
		handler(w, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() {
		server.Close()
		requests.Wait()
	})

	platform, err := New(map[string]any{"token": "TEST_TOKEN", "allow_from": "7"})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	transport := server.Client().Transport.(*http.Transport).Clone()
	// The production URL stays unchanged; only the test transport's dial target
	// and certificate verification are replaced for this loopback TLS server.
	transport.TLSClientConfig.InsecureSkipVerify = true
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	p.httpClient = &http.Client{Transport: transport, Timeout: 2 * time.Second}
	p.newBackoffTimer = immediateTimer
	t.Cleanup(func() {
		_ = p.Stop()
		p.mu.RLock()
		done := p.loopDone
		p.mu.RUnlock()
		if done != nil {
			waitPollingEvent(t, done)
		}
	})
	return p
}

func pollingRequest(t *testing.T, r *http.Request) (offset int64, timeout int) {
	t.Helper()
	var params struct {
		Offset  int64 `json:"offset"`
		Timeout int   `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil && r.Context().Err() == nil {
		t.Errorf("decode poll: %v", err)
	}
	return params.Offset, params.Timeout
}

func waitPollingEvent[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("polling event not observed")
		var zero T
		return zero
	}
}

func TestTelegramRecovery_ReadinessRequiresSuccessfulPoll(t *testing.T) {
	pollStarted := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	var polls atomic.Int32
	p := newPollingTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = pollingRequest(t, r)
		if polls.Add(1) > 1 {
			<-r.Context().Done()
			return
		}
		once.Do(func() { close(pollStarted) })
		select {
		case <-release:
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	ready := make(chan struct{}, 8)
	p.SetLifecycleHandler(testLifecycleHandler{onReady: func(core.Platform) { ready <- struct{}{} }})
	if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	waitPollingEvent(t, pollStarted)
	select {
	case <-ready:
		t.Fatal("getMe succeeded but getUpdates has not: platform must not be ready")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	waitPollingEvent(t, ready)
	select {
	case <-ready:
		t.Fatal("duplicate ready event for the same polling generation")
	default:
	}
}

func TestTelegramRecovery_PollFailureTriggersReconnect(t *testing.T) {
	var polls atomic.Int32
	p := newPollingTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = pollingRequest(t, r)
		switch polls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"ok":false,"error_code":502,"description":"temporary outage"}`)
		case 3:
			fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":101,"message":{"message_id":10,"date":%d,"text":"after recovery","from":{"id":7,"first_name":"Alice"},"chat":{"id":7,"type":"private"}}}]}`, time.Now().Unix())
		default:
			<-r.Context().Done()
		}
	})
	lifecycle := make(chan string, 8)
	p.SetLifecycleHandler(testLifecycleHandler{
		onReady:       func(core.Platform) { lifecycle <- "ready" },
		onUnavailable: func(core.Platform, error) { lifecycle <- "unavailable" },
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
	if got := waitPollingEvent(t, messages); got != "after recovery" {
		t.Fatalf("message = %q", got)
	}
}
