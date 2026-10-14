package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/go-telegram/bot/models"
)

const (
	telegramPollTimeout     = 59
	telegramHTTPTimeout     = 90 * time.Second // Headroom above server-side polling.
	telegramMaxPollResponse = 16 << 20
)

// Clone both client settings and transport configuration, never connection state.
// The template is immutable; outbound SDK requests use the same generation's
// client, while file downloads keep their separate, platform-owned client.
func cloneTelegramHTTPClient(template *http.Client) *http.Client {
	client := *template
	transport := template.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = transport.(*http.Transport).Clone()
	return &client
}

// Poll synchronously so the next offset confirms only dispatched updates, not
// updates buffered inside the SDK. A failure exits to the platform recovery loop
// before any replacement poller starts; cancellation also aborts the HTTP request.
func pollTelegramUpdates(ctx context.Context, client *http.Client, token string, lastUpdateID int64, onUpdate func(context.Context, *models.Update), onPollSuccess func()) error {
	timeout := 0 // Check receive readiness immediately without discarding updates.
	for ctx.Err() == nil {
		updates, err := getTelegramUpdates(ctx, client, token, lastUpdateID+1, timeout)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		onPollSuccess()
		for _, update := range updates {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			onUpdate(ctx, update)
			lastUpdateID = update.ID
		}
		timeout = telegramPollTimeout
	}
	return ctx.Err()
}

func getTelegramUpdates(ctx context.Context, client *http.Client, token string, offset int64, timeout int) ([]*models.Update, error) {
	params := struct {
		Offset  int64 `json:"offset"`
		Limit   int   `json:"limit"`
		Timeout int   `json:"timeout"`
	}{Offset: offset, Limit: 100, Timeout: timeout}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("telegram: encode getUpdates: %w", err)
	}
	// A bounded, fixed-length JSON request avoids the SDK's streaming multipart
	// pipe and gives cancellation a single HTTP operation to terminate.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/getUpdates", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("telegram: create getUpdates: %w", redactTelegramError(err, token))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: getUpdates request: %w", redactTelegramError(err, token))
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, telegramMaxPollResponse+1))
	if err != nil {
		return nil, fmt.Errorf("telegram: read getUpdates: %w", redactTelegramError(err, token))
	}
	if len(payload) > telegramMaxPollResponse {
		return nil, fmt.Errorf("telegram: getUpdates response exceeds %d bytes", telegramMaxPollResponse)
	}

	var response struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		// Never echo raw bodies, which may contain private messages or credentials.
		return nil, fmt.Errorf("telegram: invalid getUpdates response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || !response.OK {
		code := response.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		description := core.RedactToken(response.Description, token)
		if runes := []rune(description); len(runes) > 500 {
			description = string(runes[:500]) + "…"
		}
		return nil, &telegramPollingAPIError{code: code, description: description, retryAfter: response.Parameters.RetryAfter}
	}
	var updates []*models.Update
	if err := json.Unmarshal(response.Result, &updates); err != nil || updates == nil {
		return nil, fmt.Errorf("telegram: getUpdates result is not an update array")
	}
	for _, update := range updates {
		if update == nil || update.ID <= 0 {
			return nil, fmt.Errorf("telegram: getUpdates returned an invalid update")
		}
	}
	return updates, nil
}

type telegramPollingAPIError struct {
	code        int
	description string
	retryAfter  int
}

func (e *telegramPollingAPIError) Error() string {
	return fmt.Sprintf("telegram: getUpdates: API error %d: %s", e.code, e.description)
}

type telegramRedactedError struct {
	err     error
	message string
}

func (e *telegramRedactedError) Error() string { return e.message }
func (e *telegramRedactedError) Unwrap() error { return e.err }

func redactTelegramError(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return &telegramRedactedError{err: err, message: core.RedactToken(err.Error(), token)}
}
