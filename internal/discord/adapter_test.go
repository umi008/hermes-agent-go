package discord

import (
	"context"
	"testing"

	"github.com/umi008/hermes-agent-go/internal/config"
	"github.com/umi008/hermes-agent-go/internal/gateway"
)

func TestNewDefaults(t *testing.T) {
	adapter, err := New(Options{Token: "  tok  "})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := adapter.Name(); got != "discord" {
		t.Errorf("Name = %q, want discord", got)
	}
	if adapter.token != "tok" {
		t.Errorf("token = %q, want the trimmed value", adapter.token)
	}
	if adapter.apiBase != DefaultAPIBase {
		t.Errorf("apiBase = %q, want %q", adapter.apiBase, DefaultAPIBase)
	}
	wantGateway := "wss://gateway.discord.gg/?encoding=json&v=10"
	if adapter.gatewayURL != wantGateway {
		t.Errorf("gatewayURL = %q, want %q", adapter.gatewayURL, wantGateway)
	}
	if adapter.httpClient == nil || adapter.log == nil {
		t.Error("defaults left httpClient or logger nil")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without a token returned no error")
	}
	if _, err := New(Options{Token: "tok", GatewayURL: "/no/host"}); err == nil {
		t.Error("New with a host-less gateway URL returned no error")
	}
}

func TestAllowedChats(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		chat    string
		want    bool
	}{
		{name: "no list allows everything", chat: "123", want: true},
		{name: "list allows a member", allowed: []string{" 100 ", "200"}, chat: "200", want: true},
		{name: "list rejects a non-member", allowed: []string{"100"}, chat: "200", want: false},
		{name: "wildcard allows everything", allowed: []string{"*"}, chat: "999", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newTestAdapter(t, Options{AllowedChats: tc.allowed})
			if got := adapter.chatAllowed(tc.chat); got != tc.want {
				t.Errorf("chatAllowed(%q) = %t, want %t", tc.chat, got, tc.want)
			}
		})
	}
}

func TestFromConfig(t *testing.T) {
	t.Run("config token and allowed channels", func(t *testing.T) {
		cfg := config.FromMap(map[string]any{
			"discord": map[string]any{
				"token":            "cfg-token",
				"allowed_channels": []any{"100", 200, "300"},
			},
		})
		opts, err := FromConfig(cfg, nil, nil)
		if err != nil {
			t.Fatalf("FromConfig: %v", err)
		}
		if opts.Token != "cfg-token" {
			t.Errorf("Token = %q, want cfg-token", opts.Token)
		}
		want := []string{"100", "200", "300"}
		if len(opts.AllowedChats) != len(want) {
			t.Fatalf("AllowedChats = %v, want %v", opts.AllowedChats, want)
		}
		for i := range want {
			if opts.AllowedChats[i] != want[i] {
				t.Errorf("AllowedChats[%d] = %q, want %q", i, opts.AllowedChats[i], want[i])
			}
		}
	})

	t.Run("environment fallback", func(t *testing.T) {
		env := map[string]string{
			"DISCORD_BOT_TOKEN":        "env-token",
			"DISCORD_ALLOWED_CHANNELS": "111, 222",
		}
		lookup := func(name string) (string, bool) {
			value, ok := env[name]
			return value, ok
		}
		opts, err := FromConfig(config.FromMap(nil), lookup, nil)
		if err != nil {
			t.Fatalf("FromConfig: %v", err)
		}
		if opts.Token != "env-token" {
			t.Errorf("Token = %q, want env-token", opts.Token)
		}
		if len(opts.AllowedChats) != 2 || opts.AllowedChats[0] != "111" || opts.AllowedChats[1] != "222" {
			t.Errorf("AllowedChats = %v, want [111 222]", opts.AllowedChats)
		}
	})

	t.Run("config wins over environment", func(t *testing.T) {
		cfg := config.FromMap(map[string]any{
			"discord": map[string]any{
				"token":         "cfg-token",
				"allowed_chats": "900",
			},
		})
		lookup := func(name string) (string, bool) {
			if name == "DISCORD_BOT_TOKEN" {
				return "env-token", true
			}
			return "", false
		}
		opts, err := FromConfig(cfg, lookup, nil)
		if err != nil {
			t.Fatalf("FromConfig: %v", err)
		}
		if opts.Token != "cfg-token" {
			t.Errorf("Token = %q, want cfg-token", opts.Token)
		}
		if len(opts.AllowedChats) != 1 || opts.AllowedChats[0] != "900" {
			t.Errorf("AllowedChats = %v, want [900]", opts.AllowedChats)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		if _, err := FromConfig(config.FromMap(nil), nil, nil); err == nil {
			t.Error("FromConfig without a token returned no error")
		}
	})
}

func TestDisplayNameFallbacks(t *testing.T) {
	adapter := newTestAdapter(t, Options{})
	adapter.botID = "bot-1"
	ctx := context.Background()

	cases := []struct {
		name string
		opts messageOptions
		want string
	}{
		{
			name: "member nickname wins",
			opts: messageOptions{id: "1", channelID: "dm", content: "hi", authorID: "u1", username: "alice", globalName: "Alice", nick: "Ali"},
			want: "Ali",
		},
		{
			name: "display name without a nickname",
			opts: messageOptions{id: "2", channelID: "dm", content: "hi", authorID: "u1", username: "alice", globalName: "Alice"},
			want: "Alice",
		},
		{
			name: "username is the last resort",
			opts: messageOptions{id: "3", channelID: "dm", content: "hi", authorID: "u1", username: "alice"},
			want: "alice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got gateway.Inbound
			adapter.handleMessage(ctx, decodeMessage(t, messagePayload(tc.opts)), func(in gateway.Inbound) error {
				got = in
				return nil
			})
			if got.UserName != tc.want {
				t.Errorf("UserName = %q, want %q", got.UserName, tc.want)
			}
		})
	}
}
