package discord

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/umi008/hermes-agent-go/internal/gateway"
)

func helloPayload(intervalMS int) map[string]any {
	return map[string]any{
		"op": gwOpHello,
		"d":  map[string]any{"heartbeat_interval": intervalMS},
	}
}

func readyPayload(botID string) map[string]any {
	return map[string]any{"user": map[string]any{"id": botID}}
}

func dmMessage(id, channelID, content string) map[string]any {
	return messagePayload(messageOptions{
		id:        id,
		channelID: channelID,
		content:   content,
		authorID:  "user-1",
		username:  "alice",
	})
}

func TestHandshakeAndIdentify(t *testing.T) {
	checked := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		if idx != 0 {
			t.Errorf("unexpected extra gateway connection %d", idx)
			return
		}
		if got := c.req.URL.Path; got != "/" {
			t.Errorf("upgrade path = %q, want /", got)
		}
		query := c.req.URL.Query()
		if got := query.Get("v"); got != "10" {
			t.Errorf("gateway v = %q, want 10", got)
		}
		if got := query.Get("encoding"); got != "json" {
			t.Errorf("gateway encoding = %q, want json", got)
		}
		if got := c.req.Header.Get("Authorization"); got != "Bot test-token" {
			t.Errorf("upgrade Authorization = %q, want %q", got, "Bot test-token")
		}
		if got := c.req.Header.Get("Sec-WebSocket-Key"); got == "" {
			t.Error("upgrade is missing Sec-WebSocket-Key")
		}

		c.sendJSON(helloPayload(45000))
		identify, ok := c.expectOp(gwOpIdentify, testTimeout)
		if !ok {
			return
		}
		data := objectOf(identify, "d")
		if got := data["token"]; got != "test-token" {
			t.Errorf("identify token = %v, want test-token", got)
		}
		intents, _ := data["intents"].(float64)
		for _, want := range []int{intentGuilds, intentGuildMessages, intentDirectMessages, intentMessageContent} {
			if int(intents)&want == 0 {
				t.Errorf("identify intents %d is missing bit %d", int(intents), want)
			}
		}
		properties := objectOf(data, "properties")
		for _, key := range []string{"os", "browser", "device"} {
			if properties[key] == "" {
				t.Errorf("identify properties missing %q", key)
			}
		}

		c.sendFrame(wsOpPing, []byte("probe"), true)
		frame, ok := c.next(testTimeout)
		if !ok {
			return
		}
		if frame.opcode != wsOpPong {
			t.Errorf("reply to ping opcode = 0x%x, want pong", frame.opcode)
		}
		if string(frame.payload) != "probe" {
			t.Errorf("pong payload = %q, want probe", frame.payload)
		}
		close(checked)
		c.holdConnection()
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url})
	startAdapter(t, adapter, func(gateway.Inbound) error { return nil })

	select {
	case <-checked:
	case <-time.After(testTimeout):
		t.Fatal("gateway handler did not observe the handshake and identify")
	}
}

func TestHeartbeatSequence(t *testing.T) {
	checked := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		if idx != 0 {
			return
		}
		c.sendJSON(helloPayload(30))
		if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
			return
		}

		first, ok := c.expectOp(gwOpHeartbeat, testTimeout)
		if !ok {
			return
		}
		if first["d"] != nil {
			t.Errorf("first heartbeat d = %v, want null before any event", first["d"])
		}
		c.sendJSON(map[string]any{"op": gwOpHeartbeatAck})

		c.sendDispatch("READY", 1, readyPayload("bot-1"))

		for {
			heartbeat, ok := c.expectOp(gwOpHeartbeat, testTimeout)
			if !ok {
				return
			}
			c.sendJSON(map[string]any{"op": gwOpHeartbeatAck})
			if value, _ := heartbeat["d"].(float64); value == 1 {
				close(checked)
				break
			}
		}
		c.holdConnection()
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url})
	startAdapter(t, adapter, func(gateway.Inbound) error { return nil })

	select {
	case <-checked:
	case <-time.After(testTimeout):
		t.Fatal("heartbeat sequence was not observed")
	}
}

func TestMessageCreateBecomesInbound(t *testing.T) {
	rest := newRESTServer(t)
	inbox := channelInbox()
	sent := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		if idx != 0 {
			return
		}
		c.sendJSON(helloPayload(45000))
		if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
			return
		}
		c.sendDispatch("READY", 1, readyPayload("bot-1"))
		c.sendDispatch("MESSAGE_CREATE", 2, messagePayload(messageOptions{
			id:         "m1",
			channelID:  "chan-1",
			guildID:    "guild-1",
			content:    "<@bot-1> hello there",
			authorID:   "user-1",
			username:   "alice",
			globalName: "Alice",
			nick:       "AliceNick",
			mentions:   []map[string]any{{"id": "bot-1", "username": "hermes", "bot": true}},
		}))
		close(sent)
		c.holdConnection()
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url, APIBase: rest.srv.URL})
	startAdapter(t, adapter, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	select {
	case <-sent:
	case <-time.After(testTimeout):
		t.Fatal("gateway did not deliver MESSAGE_CREATE")
	}

	in := receiveInbound(t, inbox)
	if in.Platform != "discord" {
		t.Errorf("Platform = %q, want discord", in.Platform)
	}
	if in.ChatID != "chan-1" {
		t.Errorf("ChatID = %q, want chan-1", in.ChatID)
	}
	if in.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", in.UserID)
	}
	if in.UserName != "AliceNick" {
		t.Errorf("UserName = %q, want the member nickname AliceNick", in.UserName)
	}
	if in.Text != "hello there" {
		t.Errorf("Text = %q, want %q", in.Text, "hello there")
	}
	if in.Send == nil {
		t.Fatal("Send is nil")
	}
	if err := in.Send(context.Background(), "pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := rest.recorded()
	if len(calls) != 1 {
		t.Fatalf("REST calls = %d, want 1", len(calls))
	}
	if calls[0].Path != "/channels/chan-1/messages" {
		t.Errorf("REST path = %q, want /channels/chan-1/messages", calls[0].Path)
	}
	if calls[0].Content != "pong" {
		t.Errorf("REST content = %q, want pong", calls[0].Content)
	}
	if calls[0].Authorization != "Bot test-token" {
		t.Errorf("REST Authorization = %q, want Bot test-token", calls[0].Authorization)
	}
	if calls[0].ContentType != "application/json" {
		t.Errorf("REST Content-Type = %q, want application/json", calls[0].ContentType)
	}
}

func TestIngressRules(t *testing.T) {
	inbox := channelInbox()
	sent := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		if idx != 0 {
			return
		}
		c.sendJSON(helloPayload(45000))
		if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
			return
		}
		c.sendDispatch("READY", 1, readyPayload("bot-1"))

		// The bot's own message and any other bot's message are ignored.
		c.sendDispatch("MESSAGE_CREATE", 2, messagePayload(messageOptions{
			id: "m-bot", channelID: "chan-ok", guildID: "guild-1",
			content: "<@bot-1> from a bot", authorID: "bot-1", username: "hermes",
			bot: true, mentions: []map[string]any{{"id": "bot-1"}},
		}))
		// Guild message without a mention is ignored.
		c.sendDispatch("MESSAGE_CREATE", 3, messagePayload(messageOptions{
			id: "m-nomention", channelID: "chan-ok", guildID: "guild-1",
			content: "no mention here", authorID: "user-1", username: "alice",
		}))
		// Guild message in a channel outside AllowedChats is ignored even when
		// it mentions the bot.
		c.sendDispatch("MESSAGE_CREATE", 4, messagePayload(messageOptions{
			id: "m-other-channel", channelID: "chan-other", guildID: "guild-1",
			content: "<@bot-1> wrong channel", authorID: "user-1", username: "alice",
			mentions: []map[string]any{{"id": "bot-1"}},
		}))
		// Accepted: mention in an allowed guild channel.
		c.sendDispatch("MESSAGE_CREATE", 5, messagePayload(messageOptions{
			id: "m-ok", channelID: "chan-ok", guildID: "guild-1",
			content: "<@!bot-1> guild ok", authorID: "user-1", username: "alice",
		}))
		// Accepted: a direct message needs no mention.
		c.sendDispatch("MESSAGE_CREATE", 6, dmMessage("m-dm", "dm-1", "dm ok"))
		// System message types are ignored.
		c.sendDispatch("MESSAGE_CREATE", 7, messagePayload(messageOptions{
			id: "m-system", channelID: "dm-1", content: "pins added", authorID: "user-2",
			username: "bob", messageType: 6,
		}))
		close(sent)
		c.holdConnection()
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url, AllowedChats: []string{"chan-ok"}})
	startAdapter(t, adapter, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	select {
	case <-sent:
	case <-time.After(testTimeout):
		t.Fatal("gateway did not deliver the test messages")
	}

	first := receiveInbound(t, inbox)
	if first.ChatID != "chan-ok" || first.Text != "guild ok" {
		t.Errorf("first inbound = %+v, want the mentioned guild message", first)
	}
	second := receiveInbound(t, inbox)
	if second.ChatID != "dm-1" || second.Text != "dm ok" {
		t.Errorf("second inbound = %+v, want the direct message", second)
	}
	expectNoInbound(t, inbox)
}

func TestReconnectAfterCloseFrame(t *testing.T) {
	inbox := channelInbox()
	secondSession := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		switch idx {
		case 0:
			c.sendJSON(helloPayload(45000))
			if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
				return
			}
			c.sendDispatch("READY", 1, readyPayload("bot-1"))
			c.sendClose(1000, "bye")
			c.holdConnection()
		case 1:
			c.sendJSON(helloPayload(45000))
			if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
				return
			}
			c.sendDispatch("READY", 2, readyPayload("bot-1"))
			c.sendDispatch("MESSAGE_CREATE", 3, dmMessage("m2", "dm-1", "second session"))
			close(secondSession)
			c.holdConnection()
		default:
			t.Errorf("unexpected gateway connection %d", idx)
		}
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url})
	adapter.backoffMin = 5 * time.Millisecond
	adapter.backoffMax = 20 * time.Millisecond
	startAdapter(t, adapter, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	select {
	case <-secondSession:
	case <-time.After(testTimeout):
		t.Fatal("the adapter did not reconnect after the close frame")
	}
	in := receiveInbound(t, inbox)
	if in.Text != "second session" {
		t.Errorf("inbound after reconnect = %q, want second session", in.Text)
	}
	if got := gw.acceptedCount(); got < 2 {
		t.Errorf("gateway connections = %d, want at least 2", got)
	}
}

func TestReconnectOnOp7(t *testing.T) {
	inbox := channelInbox()
	secondSession := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		switch idx {
		case 0:
			c.sendJSON(helloPayload(45000))
			if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
				return
			}
			c.sendJSON(map[string]any{"op": gwOpReconnect})
		case 1:
			c.sendJSON(helloPayload(45000))
			if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
				return
			}
			c.sendDispatch("READY", 1, readyPayload("bot-1"))
			c.sendDispatch("MESSAGE_CREATE", 2, dmMessage("m2", "dm-1", "after op7"))
			close(secondSession)
			c.holdConnection()
		default:
			t.Errorf("unexpected gateway connection %d", idx)
		}
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url})
	adapter.backoffMin = 5 * time.Millisecond
	adapter.backoffMax = 20 * time.Millisecond
	startAdapter(t, adapter, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	select {
	case <-secondSession:
	case <-time.After(testTimeout):
		t.Fatal("the adapter did not reconnect after OP 7")
	}
	if in := receiveInbound(t, inbox); in.Text != "after op7" {
		t.Errorf("inbound after OP 7 = %q, want after op7", in.Text)
	}
}

func TestFragmentedMessageCreate(t *testing.T) {
	inbox := channelInbox()
	sent := make(chan struct{})
	gw := startFakeGateway(t, func(idx int, c *fakeGWConn) {
		if idx != 0 {
			return
		}
		c.sendJSON(helloPayload(45000))
		if _, ok := c.expectOp(gwOpIdentify, testTimeout); !ok {
			return
		}
		c.sendDispatch("READY", 1, readyPayload("bot-1"))

		// Fragment the whole dispatch envelope, not just its data field: the
		// client decodes the reassembled message as a Gateway event.
		envelope := map[string]any{"op": gwOpDispatch, "t": "MESSAGE_CREATE", "s": 2, "d": messagePayload(messageOptions{
			id: "m-frag", channelID: "dm-1", content: "fragmented", authorID: "user-1", username: "alice",
		})}
		data, err := json.Marshal(envelope)
		if err != nil {
			t.Errorf("encode message: %v", err)
			return
		}
		half := len(data) / 2
		c.sendFrame(wsOpText, data[:half], false)
		c.sendFrame(wsOpContinuation, data[half:], true)
		close(sent)
		c.holdConnection()
	})

	adapter := newTestAdapter(t, Options{GatewayURL: gw.url})
	startAdapter(t, adapter, func(in gateway.Inbound) error {
		inbox <- in
		return nil
	})

	select {
	case <-sent:
	case <-time.After(testTimeout):
		t.Fatal("gateway did not deliver the fragmented message")
	}
	if in := receiveInbound(t, inbox); in.Text != "fragmented" {
		t.Errorf("inbound text = %q, want fragmented", in.Text)
	}
}
