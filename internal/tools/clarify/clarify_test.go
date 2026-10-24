package clarify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/llm"
	"github.com/umi008/hermes-agent-go/internal/tools"
)

type fakeAsker struct {
	reply Reply
	err   error
	got   []Question
}

func (f *fakeAsker) Ask(_ context.Context, qs []Question) (Reply, error) {
	f.got = qs
	return f.reply, f.err
}

func run(t *testing.T, a Asker, args string) string {
	t.Helper()
	tool := Tool(a)
	if a == nil {
		tool = Tool(nil)
	}
	r, err := tools.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Execute(context.Background(), llm.ToolCall{Name: "clarify", Arguments: json.RawMessage(args)})
	if err != nil {
		return err.Error()
	}
	return out
}

func TestValidation(t *testing.T) {
	tests := []struct{ args, want string }{
		{`{"questions":[]}`, "questions must be a non-empty array."},
		{`{"questions":[{},{},{},{},{},{}]}`, "at most 5 items"},
		{`{"questions":["x"]}`, "questions[0] must be an object."},
		{`{"questions":[{"question":" "}]}`, "questions[0].question must be non-empty text."},
		{`{"questions":[{"question":"q","choices":"a"}]}`, "choices must be a list of strings."},
		{`{"questions":[{"question":"q","choices":[" ",""]}]}`, "has 2 entries but all are blank"},
		{`{"questions":[{"question":"q"}]}`, "Clarify tool is not available in this execution context."},
	}
	for _, tt := range tests {
		if got := run(t, nil, tt.args); !strings.Contains(got, tt.want) || !strings.HasPrefix(got, `{"error":`) {
			t.Errorf("%s: got %s", tt.args, got)
		}
	}
}

func TestAsk(t *testing.T) {
	a := &fakeAsker{reply: Reply{Outcome: "submitted", Answers: map[string]any{
		"q0": "Go (Recommended)",
		"q1": `["x (recommended)"," ","y"]`,
		"q2": nil,
	}}}
	got := run(t, a, `{"questions":[{"question":"Lang?","choices":["Go","Rust","","C","D","E"]},{"question":"Pick","choices":["x","y"],"multi_select":true},{"question":"Name?"},{"question":"Open?","multi_select":true}]}`)
	want := `{"responses":[{"question":"Lang?","choices_offered":["Go","Rust","C","D"],"status":"answered","user_response":"Go"},` +
		`{"question":"Pick","choices_offered":["x","y"],"status":"answered","user_response":["x","y"]},` +
		`{"question":"Name?","choices_offered":null,"status":"skipped","user_response":null},` +
		`{"question":"Open?","choices_offered":null,"status":"unanswered","user_response":null}],"outcome":"submitted"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if a.got[0].Choices[0] != "Go (Recommended)" || a.got[3].MultiSelect || a.got[1].ID != "q1" {
		t.Fatalf("questions: %+v", a.got)
	}
	if got := run(t, &fakeAsker{reply: Reply{Outcome: "timed_out", Notice: "no answer"}}, `{"questions":[{"question":"q"}]}`); !strings.HasSuffix(got, `"outcome":"timed_out","notice":"no answer"}`) {
		t.Fatalf("notice: %s", got)
	}
	if got := run(t, &fakeAsker{err: errors.New("tty closed")}, `{"questions":[{"question":"q"}]}`); got != `{"error":"Failed to get user input: tty closed"}` {
		t.Fatalf("err: %s", got)
	}
}
