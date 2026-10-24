package main

import (
	"context"
	"strings"
	"testing"
)

func TestGatewayPlatformSelection(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "telegram needs a token", args: nil,
			wantErr: "telegram token is required: set gateway.telegram.token in config.yaml or TELEGRAM_BOT_TOKEN"},
		{name: "discord needs a token", args: []string{"--platform", "discord"},
			wantErr: "discord: no bot token; set discord.token in config.yaml or DISCORD_BOT_TOKEN"},
		{name: "unknown platform", args: []string{"--platform", "slack"},
			wantErr: `unknown platform "slack" (telegram, discord)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := deltasModel(t, "unused")
			f := newHome(t, modelConfig(model.URL))

			code := gatewayCmd(context.Background(), tc.args, f.env())
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (stderr %q)", code, f.stderr.String())
			}
			if got := f.stderr.String(); !strings.Contains(got, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", got, tc.wantErr)
			}
		})
	}
}
