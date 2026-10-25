package apiserver

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// obj is a JSON object that keeps its key order, so the wire objects match
// upstream's dict insertion order.
type obj []kv

type kv struct {
	k string
	v any
}

// MarshalJSON encodes the fields in order.
func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(f.k)
		b.Write(key)
		b.WriteByte(':')
		val, err := json.Marshal(f.v)
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// historyEntry is one OpenAI chat-format transcript message: the shape a
// client sends as conversation_history and the store replays.
type historyEntry struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toMessages converts a transcript for the agent. Roles the agent cannot
// replay (system, developer) are dropped; instructions carry system text.
func toMessages(h []historyEntry) []llm.Message {
	out := make([]llm.Message, 0, len(h))
	for _, e := range h {
		m := llm.Message{Role: llm.Role(e.Role), Content: e.Content, ToolCallID: e.ToolCallID}
		switch m.Role {
		case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		default:
			continue
		}
		for _, tc := range e.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: json.RawMessage(tc.Function.Arguments)})
		}
		out = append(out, m)
	}
	return out
}

// fromMessages is toMessages' inverse, for the stored transcript.
func fromMessages(msgs []llm.Message) []historyEntry {
	out := make([]historyEntry, 0, len(msgs))
	for _, m := range msgs {
		e := historyEntry{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			w := wireToolCall{ID: tc.ID, Type: "function"}
			w.Function.Name, w.Function.Arguments = tc.Name, string(tc.Arguments)
			e.ToolCalls = append(e.ToolCalls, w)
		}
		out = append(out, e)
	}
	return out
}

// randomHex is n random bytes as lowercase hex, the uuid4().hex slices
// upstream ids are cut from.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// record is a JSON object updated in place like a Python dict: a new key goes
// last, an existing key keeps its position.
type record struct {
	keys []string
	vals map[string]any
}

func newRecord() *record { return &record{vals: map[string]any{}} }

func (r *record) set(k string, v any) {
	if _, ok := r.vals[k]; !ok {
		r.keys = append(r.keys, k)
	}
	r.vals[k] = v
}

// setDefault sets k only when it is absent, as dict.setdefault.
func (r *record) setDefault(k string, v any) {
	if _, ok := r.vals[k]; !ok {
		r.set(k, v)
	}
}

func (r *record) get(k string) any { return r.vals[k] }

// snapshot is the record's current fields as an ordered object.
func (r *record) snapshot() obj {
	out := make(obj, 0, len(r.keys))
	for _, k := range r.keys {
		out = append(out, kv{k, r.vals[k]})
	}
	return out
}

// unixSeconds is a time as Python time.time(): float seconds.
func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
