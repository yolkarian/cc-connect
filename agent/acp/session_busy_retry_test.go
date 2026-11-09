package acp

import (
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// shrinkBusyRetry keeps the retry policy observable in tests without waiting
// the production window.
func shrinkBusyRetry(t *testing.T) {
	t.Helper()
	window, min, max := agentBusyRetryWindow, agentBusyRetryMin, agentBusyRetryMax
	agentBusyRetryWindow, agentBusyRetryMin, agentBusyRetryMax = 2*time.Second, time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		agentBusyRetryWindow, agentBusyRetryMin, agentBusyRetryMax = window, min, max
	})
}

func busyRejection() *rpcErrPayload {
	return &rpcErrPayload{
		Code:    -32003,
		Message: "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion.",
	}
}

// A message sent while the peer is still unwinding a cancelled turn must be
// delivered once the peer frees the slot, not dropped with the raw RPC error.
func TestSend_WaitsOutPeerStillFinishingCancelledTurn(t *testing.T) {
	shrinkBusyRetry(t)
	tr, server, cancel := startACPScriptedServer(t, []any{
		busyRejection(),
		busyRejection(),
		map[string]any{"stopReason": "end_turn"},
	})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	s.acpSessID = "native-1"

	if err := s.Send("keep going", "", nil, nil); err != nil {
		t.Fatalf("prompt rejected by a winding-down turn was not retried: %v", err)
	}
	if got := len(server.called()); got != 3 {
		t.Fatalf("want three prompt attempts, got %d: %v", got, server.called())
	}
	results, failures := drainACPTestEvents(s)
	if failures != 0 {
		t.Fatalf("busy rejection must not surface as a turn error")
	}
	if results != 1 {
		t.Fatalf("want exactly one result after the retried prompt, got %d", results)
	}
}

// A peer that never frees the slot must fail with an actionable message
// instead of blocking the turn forever.
func TestSend_BusyPeerFailsAfterBoundedWait(t *testing.T) {
	shrinkBusyRetry(t)
	agentBusyRetryWindow = 30 * time.Millisecond
	tr, server, cancel := startACPScriptedServer(t, []any{
		busyRejection(), busyRejection(), busyRejection(), busyRejection(), busyRejection(),
		busyRejection(), busyRejection(), busyRejection(), busyRejection(), busyRejection(),
	})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	s.acpSessID = "native-1"

	err := s.Send("never delivered", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "previous turn still running after") {
		t.Fatalf("want an actionable bounded-wait error, got %v", err)
	}
	if len(server.called()) < 2 {
		t.Fatalf("want repeated attempts before giving up, got %v", server.called())
	}
	if _, failures := drainACPTestEvents(s); failures != 1 {
		t.Fatalf("exhausted retries must surface one turn error")
	}
}

// Non-busy rejections are terminal: retrying them could duplicate work.
func TestSend_NonBusyErrorIsNotRetried(t *testing.T) {
	shrinkBusyRetry(t)
	tr, server, cancel := startACPScriptedServer(t, []any{
		&rpcErrPayload{Code: -32603, Message: "internal error"},
	})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	s.acpSessID = "native-1"

	if err := s.Send("do work", "", nil, nil); err == nil {
		t.Fatal("internal error must fail the turn")
	}
	if got := len(server.called()); got != 1 {
		t.Fatalf("non-busy error must not be retried, got %v", server.called())
	}
	if _, failures := drainACPTestEvents(s); failures != 1 {
		t.Fatalf("terminal error must surface one turn error")
	}
}

// /stop must abandon a prompt that is still waiting for the peer's previous
// turn, and must not report it as an agent error on top of the stop reply.
func TestSend_CancelAbandonsWaitingPrompt(t *testing.T) {
	window, min, max := agentBusyRetryWindow, agentBusyRetryMin, agentBusyRetryMax
	agentBusyRetryWindow, agentBusyRetryMin, agentBusyRetryMax = 5*time.Second, 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		agentBusyRetryWindow, agentBusyRetryMin, agentBusyRetryMax = window, min, max
	})
	tr, server, cancel := startACPScriptedServer(t, []any{
		busyRejection(), busyRejection(), busyRejection(), map[string]any{"stopReason": "end_turn"},
	})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	s.acpSessID = "native-1"

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.turnCancelRequested.Store(true)
	}()
	started := time.Now()
	err := s.Send("cancelled while waiting", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "previous turn is still running") {
		t.Fatalf("want the wait to be abandoned, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancel did not end the wait promptly: %s", elapsed)
	}
	if len(server.called()) != 1 {
		t.Fatalf("no prompt may be delivered after the cancel, got %v", server.called())
	}
	if _, failures := drainACPTestEvents(s); failures != 0 {
		t.Fatalf("a cancelled wait must not add an error card")
	}
}

func TestCancelTurn_MarksTurnCancelled(t *testing.T) {
	tr, _, cancel := startACPScriptedServer(t, []any{map[string]any{}})
	defer cancel()
	s := newHandshakeTestSession(t, tr)
	s.acpSessID = "native-1"

	if err := s.CancelTurn(); err != nil {
		t.Fatalf("cancel turn: %v", err)
	}
	if !s.turnCancelRequested.Load() {
		t.Fatal("CancelTurn must abandon a prompt still waiting out the previous turn")
	}
}

// drainACPTestEvents counts terminal events a turn would have produced.
func drainACPTestEvents(s *acpSession) (results int, failures int) {
	for {
		select {
		case event := <-s.events:
			switch event.Type {
			case core.EventResult:
				results++
			case core.EventError:
				failures++
			}
		default:
			return results, failures
		}
	}
}
