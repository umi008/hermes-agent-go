package llm

import (
	"strings"
	"testing"
)

func scrubAll(deltas []string) (visible, hidden string) {
	s := NewThinkScrubber()
	var v, h strings.Builder
	for _, d := range deltas {
		vis, hid := s.Feed(d)
		v.WriteString(vis)
		h.WriteString(hid)
	}
	v.WriteString(s.Flush())
	return v.String(), h.String()
}

func TestThinkScrubber(t *testing.T) {
	tests := []struct {
		name        string
		deltas      []string
		wantVisible string
		wantHidden  string
	}{
		{"no tags", []string{"hello ", "world"}, "hello world", ""},
		{"whole block", []string{"<think>plan</think>answer"}, "answer", "plan"},
		{"open tag alone then text", []string{"<think>", "Let me check", "</think>", "done"}, "done", "Let me check"},
		{"tag split across deltas", []string{"<thi", "nk>secret</th", "ink>ok"}, "ok", "secret"},
		{"case insensitive", []string{"<THINK>x</Think>y"}, "y", "x"},
		{"inline pair removed", []string{"a <thinking>x</thinking> b"}, "a  b", "x"},
		{"mention mid-line kept", []string{"use the <think> tag"}, "use the <think> tag", ""},
		{"open after newline starts block", []string{"intro\n<reasoning>", "hidden"}, "intro\n", "hidden"},
		{"unterminated block dropped at flush", []string{"<think>never closed"}, "", "never closed"},
		{"orphan close stripped", []string{"text</think>\nmore"}, "textmore", ""},
		{"cjk tags", []string{"<思考>想</思考>答"}, "答", "想"},
		{"partial non-tag released at flush", []string{"a <th"}, "a <th", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, h := scrubAll(tt.deltas)
			if v != tt.wantVisible || h != tt.wantHidden {
				t.Fatalf("visible=%q hidden=%q; want %q / %q", v, h, tt.wantVisible, tt.wantHidden)
			}
		})
	}
}
