// Package clarify ports upstream tools/clarify_tool.py: structured
// multiple-choice or open-ended questions to the user. The UI is an Asker
// supplied by the interface (CLI today).
package clarify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/umi008/hermes-agent-go/internal/tools"
)

// Upstream limits.
const (
	MaxChoices       = 4
	MaxQuestions     = 5
	MaxChoiceChars   = 8000
	RecommendedLabel = "(Recommended)"
)

const shapeHint = "Pass questions=[{question, choices?, multi_select?}]; a single question is a one-entry array."

// Question is a validated question. Choices carry the recommendation label
// on the first entry; Offered are the bare choices.
type Question struct {
	ID          string
	Text        string
	Choices     []string
	Offered     []string
	MultiSelect bool
}

// Reply is the UI's answer. Answers map Question.ID to a string, a
// []string (multi-select) or nil (explicitly skipped); a missing ID is
// unanswered. Outcome is submitted, cancelled, timed_out or undelivered.
type Reply struct {
	Answers map[string]any
	Outcome string
	Notice  string
}

// Asker shows the questions to a human.
type Asker interface {
	Ask(ctx context.Context, qs []Question) (Reply, error)
}

// Tool returns clarify. asker nil means no human is reachable; the tool
// then returns an error result like upstream.
func Tool(asker Asker) tools.Tool {
	return tools.Tool{
		Name:    "clarify",
		Toolset: "clarify",
		Description: "Ask the user one or more questions when you need a decision or feedback. " +
			"Pass every question " +
			fmt.Sprintf("in `questions` (1-%d entries) — a single question is a ", MaxQuestions) +
			"one-entry array, and several INDEPENDENT questions belong in ONE " +
			"call (one form beats a chain of clarify calls; if one answer would " +
			"change another question, ask separately). Per question: " +
			fmt.Sprintf("single-select (up to %d choices — put your recommended ", MaxChoices) +
			"option FIRST, the UI marks it '(Recommended)' and auto-appends an " +
			"'Other' free-text row), multi-select (multi_select=true), or " +
			"open-ended (omit choices). Options go ONLY in `choices`, never " +
			"enumerated inside the question text (choices render as pickable " +
			"rows; options written into the question are dead prose the user " +
			"can't click). Result: {responses: [...], outcome} in question order. " +
			"Each response has status answered, skipped or unanswered " +
			"(user_response is null unless answered); outcome is submitted, " +
			"cancelled, timed_out or undelivered, with a notice saying why " +
			"when the wait ended without a submit. Prefer deciding " +
			"low-stakes questions yourself; don't use this for dangerous-command " +
			"confirmation (the terminal tool handles that).",
		Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":%d,`+
			`"description":"The question(s). Each: question text (options excluded), optional choices (recommended first; omit for free-text), optional multi_select. Responses come back in question order with the question text echoed.",`+
			`"items":{"type":"object","properties":{"question":{"type":"string"},"choices":{"type":"array","items":{"type":"string"},"maxItems":%d},"multi_select":{"type":"boolean"}},"required":["question"]}}},"required":["questions"]}`,
			MaxQuestions, MaxChoices)),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			return handle(ctx, asker, raw)
		},
	}
}

func handle(ctx context.Context, asker Asker, raw json.RawMessage) (string, error) {
	var a struct {
		Questions json.RawMessage `json:"questions"`
	}
	if err := tools.Decode(raw, &a); err != nil {
		return "", err
	}
	qs, msg := normalize(a.Questions)
	if msg != "" {
		return "", tools.Error(msg)
	}
	if asker == nil {
		return "", tools.Error("Clarify tool is not available in this execution context.")
	}
	reply, err := asker.Ask(ctx, qs)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", tools.Error("Failed to get user input: " + err.Error())
	}
	return result(qs, reply)
}

func normalize(raw json.RawMessage) ([]Question, string) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, "questions must be a non-empty array. " + shapeHint
	}
	if len(items) > MaxQuestions {
		return nil, fmt.Sprintf("questions supports at most %d items.", MaxQuestions)
	}
	out := make([]Question, 0, len(items))
	for i, item := range items {
		var q map[string]json.RawMessage
		if err := json.Unmarshal(item, &q); err != nil || q == nil {
			return nil, fmt.Sprintf("questions[%d] must be an object. %s", i, shapeHint)
		}
		var text string
		_ = json.Unmarshal(q["question"], &text)
		if text = strings.TrimSpace(text); text == "" {
			return nil, fmt.Sprintf("questions[%d].question must be non-empty text.", i)
		}
		var choices []string
		if c, ok := q["choices"]; ok && string(c) != "null" {
			if err := json.Unmarshal(c, &choices); err != nil {
				return nil, fmt.Sprintf("questions[%d].choices must be a list of strings.", i)
			}
			var cleaned []string
			for pos, ch := range choices {
				ch = strings.TrimSpace(ch)
				if n := utf8.RuneCountInString(ch); n > MaxChoiceChars {
					return nil, fmt.Sprintf("questions[%d].choices[%d] is %d characters; the limit is %d. Move detail into the question text and resend.", i, pos, n, MaxChoiceChars)
				}
				if ch != "" && len(cleaned) < MaxChoices {
					cleaned = append(cleaned, ch)
				}
			}
			if len(choices) > 0 && len(cleaned) == 0 {
				return nil, fmt.Sprintf("questions[%d].choices has %d entries but all are blank. Send real labels, or omit choices to ask open-ended.", i, len(choices))
			}
			choices = cleaned
		}
		var multi bool
		_ = json.Unmarshal(q["multi_select"], &multi)
		out = append(out, Question{
			ID: fmt.Sprintf("q%d", i), Text: text,
			Choices: markRecommended(choices), Offered: choices,
			MultiSelect: multi && len(choices) > 0,
		})
	}
	return out, ""
}

func markRecommended(choices []string) []string {
	if len(choices) < 2 || choices[0] != StripRecommended(choices[0]) {
		return choices
	}
	return append([]string{choices[0] + " " + RecommendedLabel}, choices[1:]...)
}

// StripRecommended removes the recommendation label from an answer.
func StripRecommended(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= len(RecommendedLabel) && strings.EqualFold(s[len(s)-len(RecommendedLabel):], RecommendedLabel) {
		return strings.TrimSpace(s[:len(s)-len(RecommendedLabel)])
	}
	return s
}

func clean(raw any, multi bool) any {
	if !multi {
		s, _ := raw.(string)
		if s = StripRecommended(s); s == "" {
			return nil
		}
		return s
	}
	var list []string
	switch v := raw.(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			list = []string{v}
		}
	case []string:
		list = v
	}
	var out []string
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			out = append(out, StripRecommended(s))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type response struct {
	Question string   `json:"question"`
	Offered  []string `json:"choices_offered"`
	Status   string   `json:"status"`
	Answer   any      `json:"user_response"`
}

func result(qs []Question, r Reply) (string, error) {
	responses := make([]response, 0, len(qs))
	for _, q := range qs {
		raw, present := r.Answers[q.ID]
		resp := response{Question: q.Text, Offered: q.Offered, Status: "unanswered"}
		if present {
			resp.Status = "skipped"
		}
		if s, isStr := raw.(string); raw != nil && (!isStr || s != "") {
			if v := clean(raw, q.MultiSelect); v != nil {
				resp.Status, resp.Answer = "answered", v
			}
		}
		responses = append(responses, resp)
	}
	return tools.Result(struct {
		Responses []response `json:"responses"`
		Outcome   string     `json:"outcome"`
		Notice    string     `json:"notice,omitempty"`
	}{responses, r.Outcome, r.Notice})
}
