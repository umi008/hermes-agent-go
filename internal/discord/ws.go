package discord

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// RFC 6455 opcodes and constants.
const (
	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA

	wsAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	// wsMaxMessageBytes bounds a single aggregated message so a hostile or
	// broken peer cannot exhaust memory.
	wsMaxMessageBytes = 16 << 20
	// wsHandshakeTimeout bounds dialing plus the HTTP upgrade.
	wsHandshakeTimeout = 15 * time.Second
	// wsWriteTimeout bounds one frame write.
	wsWriteTimeout = 15 * time.Second
)

// wsCloseError reports a close frame from the peer. Code follows RFC 6455
// section 7.4, with Discord's 4xxx application codes on top.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("discord: websocket closed by peer: code %d: %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("discord: websocket closed by peer: code %d", e.Code)
}

// wsHandshakeError reports a rejected upgrade (non-101 response). The status
// tells the reconnect loop whether retrying can help: 4xx is permanent, 5xx
// and 429 are not.
type wsHandshakeError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *wsHandshakeError) Error() string {
	msg := fmt.Sprintf("discord: gateway handshake rejected: %s", e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// wsConn is a client side WebSocket connection: masked frames out, unmasked
// frames in, no extensions (Discord's gateway negotiates none).
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu   sync.Mutex
	closeOnce sync.Once
}

// dialWebSocket performs the RFC 6455 handshake against rawURL and returns a
// frame connection. header is added to the upgrade request; the Discord
// gateway ignores extra headers, and the bot token is also carried by the
// IDENTIFY payload.
func dialWebSocket(ctx context.Context, rawURL string, header http.Header) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("discord: parse gateway url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "ws"
	case "wss":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("discord: unsupported gateway url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("discord: gateway url %q has no host", rawURL)
	}
	defaultPort := "80"
	if u.Scheme == "wss" {
		defaultPort = "443"
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), defaultPort)
	}

	dialer := &net.Dialer{Timeout: wsHandshakeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("discord: dial gateway: %w", err)
	}
	deadline := time.Now().Add(wsHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	if u.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("discord: gateway tls handshake: %w", err)
		}
		conn = tlsConn
	}

	key, err := newWSKey()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req := &http.Request{
		Method:     http.MethodGet,
		URL:        u,
		Host:       u.Host,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("discord: write gateway handshake: %w", err)
	}

	br := bufio.NewReaderSize(conn, 4096)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("discord: read gateway handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, &wsHandshakeError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	// A 101 response has no body; the reader now points at the first frame.
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(resp.Header.Get("Connection"), "upgrade") {
		_ = conn.Close()
		return nil, errors.New("discord: gateway did not upgrade the connection to websocket")
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAccept(key); got != want {
		_ = conn.Close()
		return nil, fmt.Errorf("discord: gateway sent a bad Sec-WebSocket-Accept header")
	}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		_ = conn.Close()
		return nil, fmt.Errorf("discord: gateway negotiated an unsupported websocket extension %q", ext)
	}

	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

// headerHasToken reports whether a comma separated header value contains
// token, case-insensitively.
func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func newWSKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("discord: generate websocket key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw[:]), nil
}

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsAcceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// WriteText sends one unfragmented text frame.
func (c *wsConn) WriteText(payload []byte) error {
	return c.write(wsOpText, payload)
}

// WriteClose sends a close frame without closing the transport.
func (c *wsConn) WriteClose(code int, reason string) error {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	return c.write(wsOpClose, payload)
}

func (c *wsConn) write(op byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return writeFrame(c.conn, op, payload, true, true)
}

// Close performs a best-effort close handshake and closes the transport. It is
// safe to call from any goroutine and more than once.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.WriteClose(1000, "")
		err = c.conn.Close()
	})
	return err
}

// SetReadDeadline bounds the next read; the zero value clears it.
func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// ReadMessage returns the next text message, answering pings and aggregating
// fragmented frames. A close frame from the peer is returned as *wsCloseError.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var buf []byte
	inMessage := false
	for {
		frame, err := readFrame(c.br, false)
		if err != nil {
			return nil, err
		}
		switch frame.opcode {
		case wsOpPing:
			if err := c.write(wsOpPong, frame.payload); err != nil {
				return nil, err
			}
		case wsOpPong:
			// Heartbeat acknowledgement is the OP 11 payload, not a pong frame;
			// unsolicited pongs are simply ignored.
		case wsOpClose:
			code, reason := parseClosePayload(frame.payload)
			_ = c.write(wsOpClose, frame.payload)
			return nil, &wsCloseError{Code: code, Reason: reason}
		case wsOpText, wsOpBinary:
			if inMessage {
				return nil, errors.New("discord: websocket data frame inside a fragmented message")
			}
			if frame.opcode == wsOpBinary {
				return nil, errors.New("discord: gateway sent an unexpected binary websocket frame")
			}
			if frame.fin {
				return frame.payload, nil
			}
			buf = append(buf, frame.payload...)
			inMessage = true
		case wsOpContinuation:
			if !inMessage {
				return nil, errors.New("discord: websocket continuation frame without a message")
			}
			buf = append(buf, frame.payload...)
			if len(buf) > wsMaxMessageBytes {
				return nil, fmt.Errorf("discord: websocket message exceeds %d bytes", wsMaxMessageBytes)
			}
			if frame.fin {
				return buf, nil
			}
		default:
			return nil, fmt.Errorf("discord: unknown websocket opcode 0x%x", frame.opcode)
		}
	}
}

func parseClosePayload(payload []byte) (int, string) {
	if len(payload) < 2 {
		return 1005, ""
	}
	return int(binary.BigEndian.Uint16(payload[:2])), string(payload[2:])
}

// wsFrame is one decoded RFC 6455 frame.
type wsFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// writeFrame encodes one frame. Clients mask, servers do not.
func writeFrame(w io.Writer, op byte, payload []byte, fin, mask bool) error {
	frame := make([]byte, 0, len(payload)+14)
	b0 := op
	if fin {
		b0 |= 0x80
	}
	frame = append(frame, b0)

	n := len(payload)
	var maskBit byte
	if mask {
		maskBit = 0x80
	}
	switch {
	case n <= 125:
		frame = append(frame, maskBit|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, maskBit|126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		frame = append(frame, maskBit|127)
		frame = append(frame, ext[:]...)
	}
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		frame = append(frame, key[:]...)
		for i, b := range payload {
			frame = append(frame, b^key[i&3])
		}
	} else {
		frame = append(frame, payload...)
	}
	_, err := w.Write(frame)
	return err
}

// readFrame decodes one frame. expectMasked is true for server side readers
// (client frames must be masked) and false for client side readers (server
// frames must not be).
func readFrame(br *bufio.Reader, expectMasked bool) (wsFrame, error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return wsFrame{}, err
	}
	fin := head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		return wsFrame{}, errors.New("discord: websocket frame uses reserved bits")
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	if masked != expectMasked {
		return wsFrame{}, fmt.Errorf("discord: websocket frame masking mismatch (masked=%t)", masked)
	}

	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		raw := binary.BigEndian.Uint64(ext[:])
		if raw > 1<<62 {
			return wsFrame{}, errors.New("discord: websocket frame length out of range")
		}
		length = int64(raw)
	}
	if length > wsMaxMessageBytes {
		return wsFrame{}, fmt.Errorf("discord: websocket frame of %d bytes exceeds the limit", length)
	}

	var key [4]byte
	if masked {
		if _, err := io.ReadFull(br, key[:]); err != nil {
			return wsFrame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return wsFrame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i&3]
		}
	}
	if opcode >= wsOpClose && (!fin || length > 125) {
		return wsFrame{}, errors.New("discord: invalid websocket control frame")
	}
	return wsFrame{fin: fin, opcode: opcode, payload: payload}, nil
}
