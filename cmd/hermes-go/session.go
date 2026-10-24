package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/state"
)

// recorder persists one chat session. A nil recorder (no store) records
// nothing, and write failures are logged, never fatal: losing history must
// not lose the reply.
type recorder struct {
	store  *state.Store
	id     string
	names  map[string]string // tool call id -> tool name
	logger *slog.Logger
}

// statePath is the Go port's own store. It never writes upstream's state.db.
func statePath(hermes string) string { return filepath.Join(hermes, "hermes-go", "state.db") }

func openRecorder(ctx context.Context, hermes string, sess state.Session, now func() time.Time, logger *slog.Logger) *recorder {
	st, err := state.Open(ctx, statePath(hermes), state.Options{Now: now, Logger: logger})
	if err != nil {
		logger.Warn("session store unavailable; history is not saved", "err", err)
		return nil
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		logger.Warn("create session", "err", err)
		_ = st.Close()
		return nil
	}
	return &recorder{store: st, id: sess.ID, names: map[string]string{}, logger: logger}
}

func (r *recorder) message(ctx context.Context, m llm.Message) {
	if r == nil {
		return
	}
	row := state.Message{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
	if len(m.ToolCalls) > 0 {
		type fn struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		type call struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function fn     `json:"function"`
		}
		calls := make([]call, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			r.names[c.ID] = c.Name
			calls = append(calls, call{ID: c.ID, Type: "function", Function: fn{Name: c.Name, Arguments: string(c.Arguments)}})
		}
		data, _ := json.Marshal(calls)
		row.ToolCalls = string(data)
	}
	if m.Role == llm.RoleTool {
		row.ToolName = r.names[m.ToolCallID]
	}
	if _, err := r.store.AppendMessage(ctx, r.id, row); err != nil {
		r.logger.Warn("save message", "err", err)
	}
}

func (r *recorder) usage(ctx context.Context, u llm.Usage) {
	if r == nil {
		return
	}
	err := r.store.AddUsage(ctx, r.id, state.Usage{Input: int64(u.InputTokens), Output: int64(u.OutputTokens),
		CacheRead: int64(u.CacheReadTokens), CacheWrite: int64(u.CacheWriteTokens)})
	if err != nil {
		r.logger.Warn("save usage", "err", err)
	}
}

// close ends the session with upstream's CLI reason and closes the store.
// It uses a fresh context so a cancelled run still stamps ended_at.
func (r *recorder) close() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.EndSession(ctx, r.id, "cli_close"); err != nil {
		r.logger.Warn("end session", "err", err)
	}
	if err := r.store.Close(); err != nil {
		r.logger.Warn("close session store", "err", err)
	}
}
