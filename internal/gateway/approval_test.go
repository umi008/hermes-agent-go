package gateway

import (
	"strings"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// TestApprovalPromptShapes pins the dangerous-command menu: the normal shape
// and upstream's reduced smart-deny shape.
func TestApprovalPromptShapes(t *testing.T) {
	normal := ApprovalPrompt(approval.Request{Command: "git push --force", Description: "force push"})
	for _, want := range []string{"DANGEROUS COMMAND: force push", "git push --force",
		"[o]nce  |  [s]ession  |  [d]eny", "Choice [o/s/D]:"} {
		if !strings.Contains(normal, want) {
			t.Errorf("normal prompt lacks %q: %s", want, normal)
		}
	}
	smart := ApprovalPrompt(approval.Request{Command: "git push --force", Description: "force push", SmartDenied: true})
	for _, want := range []string{"DANGEROUS COMMAND: force push", "git push --force",
		"[o]nce  |  [d]eny", "Choice [o/D]:"} {
		if !strings.Contains(smart, want) {
			t.Errorf("smart-deny prompt lacks %q: %s", want, smart)
		}
	}
	if strings.Contains(smart, "session") {
		t.Errorf("a smart deny must not offer a session scope: %s", smart)
	}
}

// TestParseApprovalShapes pins the answers each menu accepts: a smart deny
// grants one operation at most, so its scope answers are not recognised.
func TestParseApprovalShapes(t *testing.T) {
	for _, tc := range []struct {
		text       string
		smart      bool
		wantChoice approval.Choice
		wantOK     bool
	}{
		{"o", false, approval.Once, true},
		{"session", false, approval.Session, true},
		{"a", false, approval.Session, true},
		{"d", false, approval.Deny, true},
		{"d", true, approval.Deny, true},
		{"o", true, approval.Once, true},
		{"once", true, approval.Once, true},
		{"s", true, approval.Deny, false},
		{"session", true, approval.Deny, false},
		{"a", true, approval.Deny, false},
		{"always", true, approval.Deny, false},
		{"x", true, approval.Deny, false},
	} {
		choice, ok := parseApproval(tc.text, tc.smart)
		if choice != tc.wantChoice || ok != tc.wantOK {
			t.Errorf("parseApproval(%q, %v) = %v, %v; want %v, %v",
				tc.text, tc.smart, choice, ok, tc.wantChoice, tc.wantOK)
		}
	}
}
