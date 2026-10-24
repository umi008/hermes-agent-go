package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/umi008/hermes-agent-go/internal/tools/approval"
)

// The approval conversation, reusing the CLI's wording so the two menus read
// the same (hermes_cli/repl.go, upstream locales/en.yaml `approval.*`).
const (
	approvalReprompt = "Reply [o]nce, [s]ession or [d]eny."
	approvalDenied   = "✗ Denied"
	approvalOnce     = "✓ Allowed once"
	approvalSession  = "✓ Allowed for this session"
)

// ApprovalPrompt renders the upstream dangerous-command menu for a chat. The
// port never writes config.yaml, so the menu is the short one (no [a]lways);
// "always" answers count as session, as the CLI does.
func ApprovalPrompt(r approval.Request) string {
	return fmt.Sprintf("⚠️  DANGEROUS COMMAND: %s\n%s\n\n[o]nce  |  [s]ession  |  [d]eny\n\nChoice [o/s/D]:",
		r.Description, r.Command)
}

// ask sends the menu, then takes the next message from this chat as the
// answer. An unrecognised answer is re-prompted once; a second unrecognised
// answer, a closed chat or a cancelled turn all count as a denial, so an
// approval is never left pending.
func (c *chat) ask(ctx context.Context, r approval.Request) (approval.Choice, error) {
	if err := c.say(ctx, ApprovalPrompt(r)); err != nil {
		return approval.Deny, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := c.say(ctx, approvalReprompt); err != nil {
				return approval.Deny, err
			}
		}
		text, ok := c.next(ctx)
		if !ok {
			if err := ctx.Err(); err != nil {
				// The turn ended while the chat was asked: deny.
				return approval.Deny, err
			}
			return approval.Deny, errors.New("gateway: chat closed while waiting for an approval")
		}
		choice, recognised := parseApproval(text)
		if !recognised {
			continue
		}
		_ = c.say(ctx, approvalConfirmation(choice))
		return choice, nil
	}
	_ = c.say(ctx, approvalDenied)
	return approval.Deny, nil
}

// parseApproval maps a typed answer to a choice, as the CLI's linePrompter:
// o/once, s/session (and a/always, since permanent approval is unavailable in
// this port) and d/deny.
func parseApproval(text string) (approval.Choice, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "o", "once":
		return approval.Once, true
	case "s", "session", "a", "always":
		return approval.Session, true
	case "d", "deny":
		return approval.Deny, true
	}
	return approval.Deny, false
}

func approvalConfirmation(c approval.Choice) string {
	switch c {
	case approval.Once:
		return approvalOnce
	case approval.Session, approval.Always:
		return approvalSession
	}
	return approvalDenied
}
