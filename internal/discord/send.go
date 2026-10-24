package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxMessageChars is Discord's per-message content limit in Unicode code
	// points.
	MaxMessageChars = 2000

	// maxRateLimitRetries bounds 429 retries for one chunk.
	maxRateLimitRetries = 5
	// defaultRetryDelay is used when a 429 response carries no usable delay.
	defaultRetryDelay = time.Second
	// maxErrorBodyRead bounds how much of an error response is quoted.
	maxErrorBodyRead = 64 << 10
	// userAgent is Discord's recommended bot user agent shape.
	userAgent = "DiscordBot (https://github.com/umi008/hermes-agent-go, 0)"
)

// Send posts text to a Discord channel as one or more messages, splitting at
// Discord's 2000-character limit. It is safe for concurrent use: sends are
// serialised so replies reach the channel in order and concurrent callers do
// not trip the per-channel rate limit together. Empty text is a no-op because
// Discord rejects empty message content.
func (a *Adapter) Send(ctx context.Context, chatID, text string) error {
	if strings.TrimSpace(chatID) == "" {
		return errors.New("discord: empty channel id")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	chunks := splitMessage(text, MaxMessageChars)
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	for _, chunk := range chunks {
		if err := a.postMessage(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// postMessage performs one POST /channels/{id}/messages, retrying while Discord
// answers 429.
func (a *Adapter) postMessage(ctx context.Context, chatID, content string) error {
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("discord: encode message: %w", err)
	}
	endpoint := a.apiBase + "/channels/" + url.PathEscape(chatID) + "/messages"

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("discord: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bot "+a.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, err := a.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("discord: send message to %s: %w", chatID, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		_ = resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("discord: read send response: %w", readErr)
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			delay := rateLimitDelay(resp, data)
			if attempt >= maxRateLimitRetries {
				return fmt.Errorf("discord: rate limited on channel %s after %d retries", chatID, attempt)
			}
			a.log.Warn("discord: rate limited; waiting before retrying", "channel_id", chatID, "delay", delay)
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
		default:
			return fmt.Errorf("discord: send to channel %s: %s: %s", chatID, resp.Status, errorSnippet(data))
		}
	}
}

// rateLimitDelay reads retry_after from a 429 body, falling back to the
// Retry-After header, then to a fixed delay.
func rateLimitDelay(resp *http.Response, body []byte) time.Duration {
	var payload struct {
		RetryAfter *float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.RetryAfter != nil && *payload.RetryAfter > 0 {
		return time.Duration(*payload.RetryAfter * float64(time.Second))
	}
	if header := strings.TrimSpace(resp.Header.Get("Retry-After")); header != "" {
		if seconds, err := strconv.ParseFloat(header, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	return defaultRetryDelay
}

func errorSnippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(empty response)"
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}

// splitMessage cuts text into chunks of at most limit runes, preferring a
// newline and then a space near the limit so words and lines survive the cut.
// No characters are dropped.
func splitMessage(text string, limit int) []string {
	if limit <= 0 || text == "" {
		return []string{text}
	}
	runes := []rune(text)
	minimumBreak := limit / 2
	var chunks []string
	for len(runes) > limit {
		window := runes[:limit]
		cut := limit
		for i := len(window) - 1; i >= minimumBreak; i-- {
			if window[i] == '\n' {
				cut = i + 1
				break
			}
		}
		if cut == limit {
			for i := len(window) - 1; i >= minimumBreak; i-- {
				if window[i] == ' ' {
					cut = i + 1
					break
				}
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
