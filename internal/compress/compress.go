// Package compress ports the core of upstream agent/context_compressor.py:
// when the prompt nears the model's window, the middle of the conversation
// is replaced by one structured summary written by the model, with a
// deterministic fallback when that call fails. The protected head and a
// token-budgeted tail are kept verbatim; old tool outputs inside the
// summarized window are pruned to one-line digests first.
package compress

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// Upstream defaults (hermes_cli/config_defaults.py, model_metadata.py).
const (
	DefaultThreshold     = 0.50
	DefaultProtectFirstN = 3
	DefaultProtectLastN  = 20
	DefaultContextLength = 256_000
	minimumContextLength = 64_000

	leanTailFloor       = 10_000
	leanTailCap         = 25_000
	tailMaxFraction     = 0.20
	maxTailMessageFloor = 8

	minSummaryTokens     = 2000
	summaryRatio         = 0.20
	summaryTokensCeiling = 10_000
	summaryInputMaxChars = 160_000
	contentMax           = 6000
	contentHead          = 4000
	contentTail          = 1500
	toolArgsMax          = 1500
	toolArgsHead         = 1200
	pruneMinChars        = 200
	fallbackMaxChars     = 8000
)

//go:embed prompts.json
var promptsJSON []byte

// prompts are upstream's own strings, dumped from context_compressor.py
// with @@CONTENT@@, @@PREVIOUS@@, @@DATE@@ and 987654321 (the token budget)
// as placeholders.
var prompts struct {
	FreshUser       string `json:"fresh_user"`
	FreshNoUser     string `json:"fresh_nouser"`
	IterativeUser   string `json:"iterative_user"`
	IterativeNoUser string `json:"iterative_nouser"`
	SummaryPrefix   string `json:"summary_prefix"`
	EndMarker       string `json:"end_marker"`
	Note            string `json:"compression_note"`
	MergedHeader    string `json:"merged_header"`
	MergedDelimiter string `json:"merged_delimiter"`
}

func init() {
	if err := json.Unmarshal(promptsJSON, &prompts); err != nil {
		panic(fmt.Sprintf("compress: embedded prompts: %v", err))
	}
}

// Note is the line upstream appends to the system prompt once a
// conversation has been compacted.
func Note() string { return prompts.Note }

// Options configure a Compressor.
type Options struct {
	Client llm.Client
	Model  string
	// ContextLength is the model window; zero means DefaultContextLength.
	ContextLength int
	// MaxTokens is the output reservation subtracted from the window.
	MaxTokens int
	// Threshold is the fraction of the window that triggers compression.
	Threshold     float64
	ProtectFirstN int
	ProtectLastN  int
	Now           func() time.Time
	Logger        *slog.Logger
}

// Compressor keeps the previous summary so later passes update it.
type Compressor struct {
	o        Options
	previous string
	count    int
}

// New builds a Compressor with upstream defaults filled in.
func New(o Options) *Compressor {
	if o.ContextLength <= 0 {
		o.ContextLength = DefaultContextLength
	}
	if o.Threshold <= 0 {
		o.Threshold = DefaultThreshold
	}
	if o.ProtectFirstN < 0 {
		o.ProtectFirstN = 0
	} else if o.ProtectFirstN == 0 {
		o.ProtectFirstN = DefaultProtectFirstN
	}
	if o.ProtectLastN <= 0 {
		o.ProtectLastN = DefaultProtectLastN
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Compressor{o: o}
}

// Count is the number of compressions done.
func (c *Compressor) Count() int { return c.count }

// ThresholdTokens is upstream _compute_threshold_tokens: a fraction of the
// input window (at least 0.75 below 512K), floored at 64K but never past
// 85% of the window.
func (c *Compressor) ThresholdTokens() int {
	ctxLen := c.o.ContextLength
	percent := c.o.Threshold
	if ctxLen < 512_000 {
		percent = max(percent, 0.75)
	}
	eff := ctxLen - c.o.MaxTokens
	if eff <= 0 {
		eff = ctxLen
	}
	pct := int(float64(eff) * percent)
	floored := max(pct, minimumContextLength)
	cap85 := int(float64(eff) * 0.85)
	if floored > pct && floored > cap85 {
		floored = max(pct, cap85)
	}
	if floored >= eff {
		return max(1, min(cap85, eff-1))
	}
	return floored
}

// ShouldCompress reports whether a prompt of tokens needs compacting.
func (c *Compressor) ShouldCompress(tokens int) bool { return tokens >= c.ThresholdTokens() }

// EstimateTokens is upstream estimate_tokens_rough: one token per CJK or
// Hangul character, four UTF-8 bytes per token for the rest.
func EstimateTokens(s string) int {
	cjk, rest := 0, 0
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana) {
			cjk++
		} else {
			rest += utf8.RuneLen(r)
		}
	}
	if cjk == 0 {
		return (len(s) + 3) / 4
	}
	return cjk + (rest+3)/4
}

func messageTokens(m llm.Message) int {
	n := EstimateTokens(m.Content) + 10
	for _, tc := range m.ToolCalls {
		n += EstimateTokens(tc.Name) + EstimateTokens(string(tc.Arguments))
	}
	return n
}

// EstimateRequest estimates a whole request: system, messages and tools.
func EstimateRequest(system []string, msgs []llm.Message, tools []llm.ToolSpec) int {
	n := 0
	for _, s := range system {
		n += EstimateTokens(s)
	}
	for _, m := range msgs {
		n += messageTokens(m)
	}
	for _, t := range tools {
		n += EstimateTokens(t.Name) + EstimateTokens(t.Description) + EstimateTokens(string(t.Parameters))
	}
	return n
}

func (c *Compressor) tailBudget() (budget, soft int) {
	ctxLen := c.o.ContextLength
	budget = max(leanTailFloor, min(leanTailCap, int(float64(ctxLen)*0.025)))
	budget = max(1, min(budget, int(float64(ctxLen)*tailMaxFraction)))
	soft = max(budget, min(budget*3/2, int(float64(ctxLen)*tailMaxFraction)))
	return budget, soft
}

// ErrNothingToCompress means the conversation is too short to compact.
var ErrNothingToCompress = errors.New("compress: nothing to compress")

// Compress returns head + summary + tail. The input is not modified.
func (c *Compressor) Compress(ctx context.Context, msgs []llm.Message) ([]llm.Message, error) {
	n := len(msgs)
	head := c.o.ProtectFirstN
	if c.count > 0 || c.previous != "" {
		head = 0
	}
	head = alignForward(msgs, min(head, n))
	if n <= head+4 {
		return nil, ErrNothingToCompress
	}
	cut := c.tailCut(msgs, head)
	if cut <= head {
		return nil, ErrNothingToCompress
	}
	raw := msgs[head:cut]
	window := pruneToolResults(raw)
	hasUser := false
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.TrimSpace(m.Content) != "" {
			hasUser = true
			break
		}
	}
	body, err := c.summarize(ctx, window, hasUser)
	if err != nil {
		c.o.Logger.Warn("context summary failed; using deterministic fallback", "err", err)
		body = c.fallback(raw, err.Error())
	}
	c.previous = body
	c.count++
	summary := prompts.SummaryPrefix + "\n" + body

	out := append([]llm.Message(nil), msgs[:head]...)
	tail := append([]llm.Message(nil), msgs[cut:]...)
	out = append(out, place(summary, out, tail, hasUser)...)
	return sanitizeToolPairs(out), nil
}

// tailCut walks newest to oldest within the token budget, keeps tool
// groups whole and keeps the newest user turn in the tail.
func (c *Compressor) tailCut(msgs []llm.Message, head int) int {
	n := len(msgs)
	budget, soft := c.tailBudget()
	available := max(0, n-head-1)
	minTail := 0
	if available > 1 {
		floor := max(3, min(c.o.ProtectLastN, maxTailMessageFloor))
		minTail = min(floor, max(3, available-2), available)
	}
	walk := func(ceiling int) int {
		acc, cut := 0, n
		for i := n - 1; i >= head; i-- {
			t := messageTokens(msgs[i])
			if acc+t > ceiling && n-i > minTail {
				break
			}
			acc += t
			cut = i
		}
		return cut
	}
	cut := walk(soft)
	if cut <= head {
		cut = walk(budget)
	}
	cut = min(cut, n-1)
	if cut <= head {
		cut = max(n-minTail, head+1)
	}
	// Never split an assistant from its tool results.
	j := cut - 1
	for j >= head && msgs[j].Role == llm.RoleTool {
		j--
	}
	if j >= head && msgs[j].Role == llm.RoleAssistant && len(msgs[j].ToolCalls) > 0 && j < cut {
		cut = j
	}
	for i := n - 1; i > head; i-- {
		if msgs[i].Role == llm.RoleUser {
			cut = min(cut, i)
			break
		}
	}
	return min(n, alignForward(msgs, max(cut, head+1)))
}

func alignForward(msgs []llm.Message, i int) int {
	for i < len(msgs) && msgs[i].Role == llm.RoleTool {
		i++
	}
	return i
}

// pruneToolResults replaces older duplicate outputs with a marker and long
// outputs with upstream's generic one-line digest.
func pruneToolResults(window []llm.Message) []llm.Message {
	out := append([]llm.Message(nil), window...)
	calls := map[string]llm.ToolCall{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = tc
		}
	}
	seen := map[string]bool{}
	for i := len(out) - 1; i >= 0; i-- {
		m := &out[i]
		if m.Role != llm.RoleTool || utf8.RuneCountInString(m.Content) < pruneMinChars {
			continue
		}
		if seen[m.Content] {
			m.Content = "[Duplicate tool output — same content as a more recent call]"
			continue
		}
		seen[m.Content] = true
		tc := calls[m.ToolCallID]
		m.Content = digest(tc, m.Content)
	}
	return out
}

func digest(tc llm.ToolCall, content string) string {
	name := tc.Name
	if name == "" {
		name = "tool"
	}
	var b strings.Builder
	var args []json.RawMessage
	var keys []string
	dec := json.NewDecoder(strings.NewReader(string(tc.Arguments)))
	if tok, err := dec.Token(); err == nil && tok == json.Delim('{') {
		for dec.More() && len(keys) < 2 {
			k, err := dec.Token()
			if err != nil {
				break
			}
			var v json.RawMessage
			if dec.Decode(&v) != nil {
				break
			}
			keys, args = append(keys, fmt.Sprint(k)), append(args, v)
		}
	}
	for i, k := range keys {
		v := string(args[i])
		var s string
		if json.Unmarshal(args[i], &s) == nil {
			v = s
		}
		fmt.Fprintf(&b, " %s=%s", k, truncRunes(v, 40))
	}
	return fmt.Sprintf("[%s]%s (%s chars result)", name, b.String(), commas(utf8.RuneCountInString(content)))
}

var thinkRE = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

func elideMiddle(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	marker := fmt.Sprintf("⟪HERMES-CONTEXT-COMPRESSION: %s of %s chars omitted here by Hermes's context compressor.⟫",
		commas(len(r)-head-tail), commas(len(r)))
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

func serialize(window []llm.Message) string {
	parts := make([]string, 0, len(window))
	for _, m := range window {
		content := m.Content
		if m.Role == llm.RoleAssistant {
			content = thinkRE.ReplaceAllString(content, "")
		}
		if utf8.RuneCountInString(content) > contentMax {
			content = elideMiddle(content, contentHead, contentTail)
		}
		if m.Role == llm.RoleTool {
			parts = append(parts, fmt.Sprintf("[TOOL RESULT %s]: %s", m.ToolCallID, content))
			continue
		}
		if len(m.ToolCalls) > 0 {
			lines := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				args := string(tc.Arguments)
				if utf8.RuneCountInString(args) > toolArgsMax {
					args = string([]rune(args)[:toolArgsHead]) + "..."
				}
				lines = append(lines, fmt.Sprintf("  %s(%s)", tc.Name, args))
			}
			content += "\n[Tool calls:\n" + strings.Join(lines, "\n") + "\n]"
		}
		parts = append(parts, fmt.Sprintf("[%s]: %s", strings.ToUpper(string(m.Role)), content))
	}
	return strings.Join(parts, "\n\n")
}

// boundInput is upstream _bound_summary_input: keep 45% head and 55% tail.
func boundInput(content string) string {
	r := []rune(content)
	if len(r) <= summaryInputMaxChars {
		return content
	}
	omitted := len(r)
	var marker string
	var headChars, tailChars int
	for range 2 {
		marker = fmt.Sprintf("\n\n...[summary input truncated: omitted %s chars from the middle to keep compression prompt bounded]...\n\n", commas(omitted))
		remaining := max(summaryInputMaxChars-utf8.RuneCountInString(marker), 0)
		headChars = remaining * 45 / 100
		tailChars = remaining - headChars
		omitted = max(len(r)-headChars-tailChars, 0)
	}
	tail := ""
	if tailChars > 0 {
		tail = strings.TrimLeftFunc(string(r[len(r)-tailChars:]), unicode.IsSpace)
	}
	return strings.TrimRightFunc(string(r[:headChars]), unicode.IsSpace) + marker + tail
}

func (c *Compressor) summaryBudget(window []llm.Message) int {
	est := 0
	for _, m := range window {
		est += messageTokens(m)
	}
	ceiling := min(int(float64(c.o.ContextLength)*0.05), summaryTokensCeiling)
	return max(minSummaryTokens, min(int(float64(est)*summaryRatio), ceiling))
}

// Prompt builds the summarizer prompt for window.
func (c *Compressor) Prompt(window []llm.Message, hasUser bool) string {
	tmpl := prompts.FreshNoUser
	switch {
	case c.previous != "" && hasUser:
		tmpl = prompts.IterativeUser
	case c.previous != "":
		tmpl = prompts.IterativeNoUser
	case hasUser:
		tmpl = prompts.FreshUser
	}
	return strings.NewReplacer(
		"@@CONTENT@@", boundInput(serialize(window)),
		"@@PREVIOUS@@", boundInput(c.previous),
		"@@DATE@@", c.o.Now().Format("2006-01-02"),
		"987654321", strconv.Itoa(c.summaryBudget(window)),
	).Replace(tmpl)
}

var refusalRE = regexp.MustCompile(`(?i)^\s*(i'm sorry|i am sorry|i can(no|')t|i won't|i will not|sorry,)`)

func (c *Compressor) summarize(ctx context.Context, window []llm.Message, hasUser bool) (string, error) {
	if c.o.Client == nil {
		return "", errors.New("no summary client")
	}
	req := llm.Request{Model: c.o.Model, Messages: []llm.Message{{Role: llm.RoleUser, Content: c.Prompt(window, hasUser)}}}
	resp, err := c.o.Client.Stream(ctx, req, func(llm.Event) {})
	if err != nil {
		return "", err
	}
	if resp.StopReason == llm.StopMaxTokens {
		return "", errors.New("summary truncated (finish_reason=length)")
	}
	body := strings.TrimSpace(thinkRE.ReplaceAllString(resp.Message.Content, ""))
	if body == "" {
		return "", errors.New("empty summary")
	}
	if refusalRE.MatchString(body) {
		return "", errors.New("summary refused")
	}
	return body, nil
}

// place inserts the summary between head and tail with a role that keeps
// user/assistant alternation, or merges it into the first tail row when
// neither role fits (upstream _summary_placement).
func place(summary string, head, tail []llm.Message, hasUser bool) []llm.Message {
	row := summary + "\n\n" + prompts.EndMarker
	lastHead := llm.Role("")
	if len(head) > 0 {
		lastHead = head[len(head)-1].Role
	}
	forceUser := len(head) == 0 || !hasUser
	role := llm.RoleAssistant
	if lastHead == "" || lastHead == llm.RoleAssistant || lastHead == llm.RoleTool || forceUser {
		role = llm.RoleUser
	}
	if len(tail) == 0 || tail[0].Role != role {
		return append([]llm.Message{{Role: role, Content: row}}, tail...)
	}
	flip := llm.RoleUser
	if role == llm.RoleUser {
		flip = llm.RoleAssistant
	}
	if !forceUser && flip != lastHead {
		return append([]llm.Message{{Role: flip, Content: row}}, tail...)
	}
	first := tail[0]
	if forceUser && role == llm.RoleUser {
		first.Content = row + "\n\n" + first.Content
	} else {
		first.Content = prompts.MergedHeader + "\n" + first.Content + "\n\n" + prompts.MergedDelimiter + "\n\n" + row
	}
	first.Native = nil // the provider form no longer matches the text
	tail[0] = first
	return tail
}

// sanitizeToolPairs drops tool results without a call and strips calls
// without results, except on the newest assistant (still in flight).
func sanitizeToolPairs(msgs []llm.Message) []llm.Message {
	calls, results := map[string]bool{}, map[string]bool{}
	lastAssistant := -1
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
		if m.Role == llm.RoleTool {
			results[m.ToolCallID] = true
		}
		if m.Role == llm.RoleAssistant {
			lastAssistant = i
		}
	}
	out := make([]llm.Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == llm.RoleTool && !calls[m.ToolCallID] {
			continue
		}
		if len(m.ToolCalls) > 0 && i != lastAssistant {
			var kept []llm.ToolCall
			for _, tc := range m.ToolCalls {
				if results[tc.ID] {
					kept = append(kept, tc)
				}
			}
			if len(kept) != len(m.ToolCalls) {
				m.ToolCalls, m.Native = kept, nil
				if len(kept) == 0 && strings.TrimSpace(m.Content) == "" {
					m.Content = "(tool call removed)"
				}
			}
		}
		out = append(out, m)
	}
	return out
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func commas(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
