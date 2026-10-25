package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/umi008/hermes-agent-go/internal/llm"
)

// TurnHooks observe a conversation, as upstream's lifecycle hooks
// on_session_start, pre_llm_call, post_llm_call and on_session_end.
// Every method runs on the goroutine that called Run.
type TurnHooks interface {
	// SessionStart fires before the first turn of a conversation.
	SessionStart(ctx context.Context, model string)
	// PreLLMCall fires once per turn before the first model call. The
	// returned text is appended to the user message sent to the model;
	// the message persisted through OnMessage keeps the original text.
	PreLLMCall(ctx context.Context, t TurnInfo) string
	// PostLLMCall fires once per turn after the tool loop.
	PostLLMCall(ctx context.Context, t TurnInfo, response string)
	// TurnEnd fires last in every turn, upstream's per-turn on_session_end.
	TurnEnd(ctx context.Context, t TurnInfo, end TurnEnd)
}

// TurnInfo describes the running turn.
type TurnInfo struct {
	UserMessage string
	// History is a copy of the conversation, the current user message
	// included.
	History   []llm.Message
	FirstTurn bool
	// Model is the model serving the turn, a fallback's when one is active.
	Model string
}

// TurnEnd is how a turn finished.
type TurnEnd struct {
	Completed   bool
	Failed      bool
	Interrupted bool
	// Reason is upstream's turn_exit_reason.
	Reason string
}

// turnEnd maps a Run outcome to upstream's exit flags and reason.
func turnEnd(ctx context.Context, res Result, err error, maxTurns int) TurnEnd {
	switch {
	case errors.Is(err, ErrMaxTurns):
		return TurnEnd{Reason: fmt.Sprintf("max_iterations_reached(%d/%d)", res.Turns, maxTurns)}
	case err != nil && ctx.Err() != nil:
		return TurnEnd{Interrupted: true, Reason: "interrupted_by_user"}
	case err != nil:
		return TurnEnd{Failed: true, Reason: "all_retries_exhausted_no_response"}
	}
	return TurnEnd{Completed: true, Reason: "text_response(finish_reason=" + finishReason(res.StopReason) + ")"}
}

// finishReason is the OpenAI finish_reason upstream reports for a stop.
func finishReason(stop string) string {
	switch stop {
	case llm.StopEnd:
		return "stop"
	case llm.StopMaxTokens:
		return "length"
	case llm.StopToolUse:
		return "tool_calls"
	}
	return stop
}
