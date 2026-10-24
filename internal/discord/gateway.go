package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/umi008/hermes-agent-go/internal/gateway"
)

// Discord Gateway v10 opcodes used by this adapter.
const (
	gwOpDispatch       = 0
	gwOpHeartbeat      = 1
	gwOpIdentify       = 2
	gwOpReconnect      = 7
	gwOpInvalidSession = 9
	gwOpHello          = 10
	gwOpHeartbeatAck   = 11
)

// Gateway intents: GUILDS, GUILD_MESSAGES, DIRECT_MESSAGES and the privileged
// MESSAGE_CONTENT (the same set upstream's discord.py client requests). Without
// MESSAGE_CONTENT the guild message content arrives empty, so it must be
// enabled in the Developer Portal for the bot.
const (
	intentGuilds         = 1 << 0
	intentGuildMessages  = 1 << 9
	intentDirectMessages = 1 << 12
	intentMessageContent = 1 << 15

	gatewayIntents = intentGuilds | intentGuildMessages | intentDirectMessages | intentMessageContent
)

// defaultHeartbeatInterval is Discord's documented fallback when HELLO omits
// the interval.
const defaultHeartbeatInterval = 41250 * time.Millisecond

// errGatewayReconnect means the session ended in a way a fresh connection can
// fix (peer close, OP 7, OP 9, transport error).
var errGatewayReconnect = errors.New("discord: gateway asked to reconnect")

// errHeartbeatMissing means a heartbeat was not acknowledged before the next
// one was due, which RFC/Discord treat as a zombie connection.
var errHeartbeatMissing = errors.New("discord: gateway did not acknowledge a heartbeat")

// fatalError marks a failure that retrying cannot fix, such as a rejected bot
// token or a disallowed intent.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

// gwPayload is one gateway frame.
type gwPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// runGateway keeps a gateway session alive until ctx is cancelled. A session
// that ends for a recoverable reason is retried with exponential backoff; a
// permanent failure is returned.
func (a *Adapter) runGateway(ctx context.Context, handle func(gateway.Inbound) error) error {
	backoffMin := a.backoffMin
	if backoffMin <= 0 {
		backoffMin = defaultBackoffFloor
	}
	backoffMax := a.backoffMax
	if backoffMax < backoffMin {
		backoffMax = backoffMin
	}
	backoff := backoffMin

	for {
		if ctx.Err() != nil {
			return nil
		}
		err := a.runSession(ctx, handle, func() { backoff = backoffMin })
		if ctx.Err() != nil {
			return nil
		}
		var fatal *fatalError
		if errors.As(err, &fatal) {
			return fatal.err
		}
		if err == nil {
			err = errGatewayReconnect
		}
		a.log.Warn("discord: gateway session ended; reconnecting", "error", err, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// runSession connects once: HELLO, IDENTIFY, heartbeat, then dispatch until the
// connection ends. onReady is called when Discord confirms the session.
func (a *Adapter) runSession(ctx context.Context, handle func(gateway.Inbound) error, onReady func()) error {
	conn, err := dialWebSocket(ctx, a.gatewayURL, a.authHeader())
	if err != nil {
		return classifyGatewayDialError(err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(wsHandshakeTimeout))
	data, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("discord: waiting for the gateway hello: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	var hello gwPayload
	if err := json.Unmarshal(data, &hello); err != nil {
		return fmt.Errorf("discord: decode the gateway hello: %w", err)
	}
	if hello.Op != gwOpHello {
		return fmt.Errorf("discord: expected gateway op %d (hello), got op %d", gwOpHello, hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if len(hello.D) > 0 {
		if err := json.Unmarshal(hello.D, &helloData); err != nil {
			return fmt.Errorf("discord: decode the gateway hello payload: %w", err)
		}
	}
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var seq atomic.Int64
	var ackPending atomic.Bool
	heartbeatErrs := make(chan error, 1)
	go a.heartbeatLoop(sessionCtx, conn, interval, &seq, &ackPending, heartbeatErrs)

	if err := a.identify(conn); err != nil {
		return err
	}
	a.log.Info("discord: gateway connected", "heartbeat_interval", interval)

	for {
		data, err := conn.ReadMessage()
		if err != nil {
			cancel()
			select {
			case heartbeatErr := <-heartbeatErrs:
				if heartbeatErr != nil {
					return heartbeatErr
				}
			default:
			}
			return err
		}
		var payload gwPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			a.log.Warn("discord: dropping a malformed gateway payload", "error", err)
			continue
		}
		if payload.S != nil {
			seq.Store(*payload.S)
		}
		switch payload.Op {
		case gwOpDispatch:
			a.dispatch(ctx, handle, payload, onReady)
		case gwOpHeartbeat:
			// The gateway may ask for an immediate heartbeat.
			if err := a.sendHeartbeat(conn, &seq, &ackPending); err != nil {
				return fmt.Errorf("discord: send heartbeat: %w", err)
			}
		case gwOpReconnect:
			a.log.Info("discord: the gateway requested a reconnect")
			return errGatewayReconnect
		case gwOpInvalidSession:
			var resumable bool
			if len(payload.D) > 0 {
				_ = json.Unmarshal(payload.D, &resumable)
			}
			// RESUME is out of scope, so the next session identifies again.
			a.log.Info("discord: the gateway invalidated the session", "resumable", resumable)
			return errGatewayReconnect
		case gwOpHeartbeatAck:
			ackPending.Store(false)
		}
	}
}

// classifyGatewayDialError turns a permanent upgrade rejection into a fatal
// error. Gateway authentication happens in IDENTIFY, so a 4xx here means the
// URL is wrong; 429 and 5xx are worth retrying.
func classifyGatewayDialError(err error) error {
	var handshake *wsHandshakeError
	if errors.As(err, &handshake) &&
		handshake.StatusCode >= 400 && handshake.StatusCode < 500 &&
		handshake.StatusCode != http.StatusTooManyRequests {
		return &fatalError{err: err}
	}
	return err
}

// identify sends the OP 2 payload carrying the bot token and intents.
func (a *Adapter) identify(conn *wsConn) error {
	payload := map[string]any{
		"op": gwOpIdentify,
		"d": map[string]any{
			"token":   a.token,
			"intents": gatewayIntents,
			"properties": map[string]string{
				"os":      runtime.GOOS,
				"browser": "hermes-go",
				"device":  "hermes-go",
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("discord: encode identify: %w", err)
	}
	if err := conn.WriteText(data); err != nil {
		return fmt.Errorf("discord: send identify: %w", err)
	}
	return nil
}

// dispatch handles one DISPATCH event.
func (a *Adapter) dispatch(ctx context.Context, handle func(gateway.Inbound) error, payload gwPayload, onReady func()) {
	switch payload.T {
	case "READY":
		var ready struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if err := json.Unmarshal(payload.D, &ready); err != nil {
			a.log.Warn("discord: decode ready payload", "error", err)
			return
		}
		if ready.User.ID == "" {
			a.log.Warn("discord: ready payload has no user id")
			return
		}
		a.botID = ready.User.ID
		a.log.Info("discord: identified", "bot_user_id", ready.User.ID)
		onReady()
	case "MESSAGE_CREATE":
		var msg discordMessage
		if err := json.Unmarshal(payload.D, &msg); err != nil {
			a.log.Warn("discord: decode message_create payload", "error", err)
			return
		}
		a.handleMessage(ctx, msg, handle)
	}
}

// heartbeatLoop sends OP 1 every interval, jittering the first one as Discord
// asks, and treats a missing ACK as a zombie connection.
func (a *Adapter) heartbeatLoop(ctx context.Context, conn *wsConn, interval time.Duration, seq *atomic.Int64, ackPending *atomic.Bool, errs chan<- error) {
	jitter := time.Duration(0)
	if interval > 0 {
		jitter = rand.N(interval)
	}
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ackPending.Load() {
			errs <- errHeartbeatMissing
			_ = conn.Close()
			return
		}
		if err := a.sendHeartbeat(conn, seq, ackPending); err != nil {
			errs <- fmt.Errorf("discord: send heartbeat: %w", err)
			_ = conn.Close()
			return
		}
		timer.Reset(interval)
	}
}

// sendHeartbeat sends one OP 1 frame. d carries the last sequence number, or
// null when no event has been received yet.
func (a *Adapter) sendHeartbeat(conn *wsConn, seq *atomic.Int64, ackPending *atomic.Bool) error {
	var last any
	if value := seq.Load(); value != 0 {
		last = value
	}
	data, err := json.Marshal(map[string]any{"op": gwOpHeartbeat, "d": last})
	if err != nil {
		return err
	}
	ackPending.Store(true)
	return conn.WriteText(data)
}

// sleepCtx waits for d or until ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
