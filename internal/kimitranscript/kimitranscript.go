// Package kimitranscript locates Kimi CLI session directories and parses the
// transcripts they persist.
//
// The adapters that drive a Kimi backend need this outside the Kimi CLI agent
// itself: the ACP adapter talks to `kimi acp`, whose server keeps the same
// on-disk sessions, and it must be able to answer /history after the bridge
// restarted or when the server no longer replays a session. Sharing the reader
// here keeps that dependency out of agent-to-agent imports.
package kimitranscript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// SessionsBaseDirs returns the session storage roots of both CLI flavors.
// Legacy kimi-cli uses ~/.kimi/sessions; Kimi Code uses
// $KIMI_CODE_HOME/sessions or ~/.kimi-code/sessions by default (#1561).
// extraEnv entries of the form "KIMI_CODE_HOME=/path" override the process
// environment, which adapters use to pass the agent's configured env through.
func SessionsBaseDirs(extraEnv ...string) []string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	codeHome := os.Getenv("KIMI_CODE_HOME")
	for _, entry := range extraEnv {
		if value, ok := strings.CutPrefix(entry, "KIMI_CODE_HOME="); ok {
			codeHome = value
		}
	}
	if codeHome == "" {
		codeHome = filepath.Join(homeDir, ".kimi-code")
	}
	return []string{
		filepath.Join(homeDir, ".kimi", "sessions"),
		filepath.Join(codeHome, "sessions"),
	}
}

// FindSessionDir locates a session directory by id across both storage roots.
// It returns an empty string when the session is not on disk.
func FindSessionDir(sessionID string, extraEnv ...string) string {
	for _, sessionsBase := range SessionsBaseDirs(extraEnv...) {
		entries, err := os.ReadDir(sessionsBase)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			projectDir := filepath.Join(sessionsBase, entry.Name())
			sessionEntries, err := os.ReadDir(projectDir)
			if err != nil {
				continue
			}
			for _, se := range sessionEntries {
				if !se.IsDir() {
					continue
				}
				if se.Name() == sessionID {
					return filepath.Join(projectDir, se.Name())
				}
			}
		}
	}
	return ""
}

// ReadSessionHistory locates a Kimi session directory by id and parses its
// transcript (legacy context.jsonl or Kimi Code CLI wire.jsonl).
func ReadSessionHistory(sessionID string, limit int, extraEnv ...string) ([]core.HistoryEntry, error) {
	sessionDir := FindSessionDir(sessionID, extraEnv...)
	if sessionDir == "" {
		return nil, fmt.Errorf("kimi: session not found: %s", sessionID)
	}

	contextPath := filepath.Join(sessionDir, "context.jsonl")
	entries, err := readHistoryFile(contextPath, readContextHistory)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("kimi: read legacy session history: %w", err)
	}
	if len(entries) == 0 {
		wirePath := filepath.Join(sessionDir, "agents", "main", "wire.jsonl")
		entries, err = readHistoryFile(wirePath, readWireHistory)
		if err != nil {
			return nil, fmt.Errorf("kimi: read session history: %w", err)
		}
	}

	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

func readHistoryFile(path string, parse func(io.Reader) ([]core.HistoryEntry, error)) ([]core.HistoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

func readContextHistory(r io.Reader) ([]core.HistoryEntry, error) {
	var entries []core.HistoryEntry
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 10*1024*1024)
	for scanner.Scan() {
		var raw struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Timestamp json.RawMessage `json:"timestamp"`
			Time      json.RawMessage `json:"time"`
		}
		if json.Unmarshal(scanner.Bytes(), &raw) != nil {
			continue
		}
		if raw.Role != "user" && raw.Role != "assistant" {
			continue
		}
		content := ExtractText(raw.Content)
		if content == "" {
			continue
		}
		timestamp := ParseTimestamp(raw.Timestamp)
		if timestamp.IsZero() {
			timestamp = ParseTimestamp(raw.Time)
		}
		entries = append(entries, core.HistoryEntry{
			Role:      raw.Role,
			Content:   content,
			Timestamp: timestamp,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan context.jsonl: %w", err)
	}
	return entries, nil
}

func readWireHistory(r io.Reader) ([]core.HistoryEntry, error) {
	var entries []core.HistoryEntry
	var assistantTurnID string
	var assistantText strings.Builder
	var assistantTimestamp time.Time

	flushAssistant := func() {
		content := strings.TrimSpace(assistantText.String())
		if content != "" {
			entries = append(entries, core.HistoryEntry{
				Role:      "assistant",
				Content:   content,
				Timestamp: assistantTimestamp,
			})
		}
		assistantTurnID = ""
		assistantText.Reset()
		assistantTimestamp = time.Time{}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 10*1024*1024)
	for scanner.Scan() {
		var raw struct {
			Type    string          `json:"type"`
			Time    json.RawMessage `json:"time"`
			Origin  json.RawMessage `json:"origin"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Origin  json.RawMessage `json:"origin"`
			} `json:"message"`
			Event struct {
				Type   string `json:"type"`
				TurnID string `json:"turnId"`
				Part   struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"part"`
			} `json:"event"`
		}
		if json.Unmarshal(scanner.Bytes(), &raw) != nil {
			continue
		}

		switch raw.Type {
		case "context.append_message":
			if raw.Message.Role != "user" && raw.Message.Role != "assistant" {
				continue
			}
			if raw.Message.Role == "user" && !IsUserOrigin(raw.Origin, raw.Message.Origin) {
				continue
			}
			content := ExtractText(raw.Message.Content)
			if content == "" {
				continue
			}
			flushAssistant()
			entries = append(entries, core.HistoryEntry{
				Role:      raw.Message.Role,
				Content:   content,
				Timestamp: ParseTimestamp(raw.Time),
			})
		case "context.append_loop_event":
			if raw.Event.Type != "content.part" || raw.Event.Part.Type != "text" || raw.Event.Part.Text == "" {
				continue
			}
			if assistantText.Len() > 0 && raw.Event.TurnID != assistantTurnID {
				flushAssistant()
			}
			if assistantText.Len() == 0 {
				assistantTurnID = raw.Event.TurnID
				assistantTimestamp = ParseTimestamp(raw.Time)
			}
			assistantText.WriteString(raw.Event.Part.Text)
		}
	}
	flushAssistant()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan wire.jsonl: %w", err)
	}
	return entries, nil
}

// ExtractText pulls the visible text out of a Kimi message content field,
// which is either a plain string or a list of typed parts.
func ExtractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(texts, "\n")
}

// ParseTimestamp accepts the millisecond integer or RFC3339 string forms used
// by the two CLI flavors.
func ParseTimestamp(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}

	var millis int64
	if json.Unmarshal(raw, &millis) == nil {
		return time.UnixMilli(millis)
	}

	var value string
	if json.Unmarshal(raw, &value) != nil {
		return time.Time{}
	}
	if timestamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return timestamp
	}
	return time.Time{}
}

// IsUserOrigin distinguishes prompts the user typed from injected context
// (skill bodies, cron wrappers). Older wire records omit origin, in which case
// the user role is the only available signal; current records identify
// injected prompts explicitly.
func IsUserOrigin(topLevel, nested json.RawMessage) bool {
	origin := nested
	if len(origin) == 0 || string(origin) == "null" {
		origin = topLevel
	}
	if len(origin) == 0 || string(origin) == "null" {
		return true
	}
	var value struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(origin, &value) != nil {
		return false
	}
	switch value.Kind {
	case "user", "user-slash", "user_slash":
		return true
	default:
		return false
	}
}
