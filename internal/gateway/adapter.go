// Package gateway turns chat platforms into Hermes sessions: one session per
// external chat, replies delivered back to that chat, approvals asked inside
// the conversation.
//
// The adapters (Telegram, Discord) know their platform's wire protocol; the
// core owns sessions, prompts and approvals. This file is that boundary and
// nothing else, so an adapter never imports the store and the core never
// speaks HTTP to a platform.
package gateway

import "context"

// Inbound is one message a platform delivered, plus the only way to answer it.
type Inbound struct {
	// Platform is the session source recorded in the store: "telegram" or
	// "discord".
	Platform string
	// ChatID identifies the conversation. A session is bound to it.
	ChatID string
	// UserID and UserName identify who wrote the message, for the prompt and
	// for the approval prompt.
	UserID   string
	UserName string
	Text     string
	// Send posts text to the same chat. The core may call it several times
	// (the reply, then an approval prompt). A nil Send means the platform
	// cannot answer, and the core must skip the message.
	Send func(ctx context.Context, text string) error
}

// Adapter is one chat platform.
type Adapter interface {
	// Name is the platform name, used as the session source.
	Name() string
	// Run blocks until ctx is cancelled, handing every inbound message to
	// handle. handle returns quickly: the core queues per chat, so a slow turn
	// never stalls the adapter's receive loop.
	Run(ctx context.Context, handle func(Inbound) error) error
}
