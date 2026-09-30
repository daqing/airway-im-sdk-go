package airwayim

// A minimal RFC 6455 WebSocket client built on the standard library only,
// mirroring the zero-dependency approach of the Ruby and PHP SDKs: it
// covers the opening handshake, masked client frames, fragmentation,
// protocol-level ping/pong, and the closing handshake — everything the
// gateway protocol needs and nothing it forbids (no compression
// extension is ever negotiated).

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
	"sync"
	"time"
)

const (
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	opcodeContinuation = 0x0
	opcodeText         = 0x1
	opcodeBinary       = 0x2
	opcodeClose        = 0x8
	opcodePing         = 0x9
	opcodePong         = 0xA

	// wsMaxMessage caps one reassembled message; the gateway itself
	// limits JSON messages to 64 KiB, so this leaves generous headroom.
	wsMaxMessage = 1 << 20

	wsHandshakeTimeout = 15 * time.Second
	wsWriteTimeout     = 10 * time.Second
)

// wsConn is one established WebSocket connection. Send is safe for
// concurrent use; Receive must run on a single goroutine.
type wsConn struct {
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
}

// dialWebSocket performs the HTTP upgrade handshake against rawURL
// (ws:// or wss://), bounding every step by timeout.
func dialWebSocket(ctx context.Context, rawURL string, timeout time.Duration) (*wsConn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid websocket URL: %w", err)
	}
	var secure bool
	switch parsed.Scheme {
	case "ws":
		secure = false
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("unsupported websocket scheme %q", parsed.Scheme)
	}
	host := parsed.Host
	if parsed.Port() == "" {
		if secure {
			host = parsed.Hostname() + ":443"
		} else {
			host = parsed.Hostname() + ":80"
		}
	}

	dialer := net.Dialer{Timeout: timeout}
	var conn net.Conn
	if secure {
		conn, err = tls.DialWithDialer(&dialer, "tcp", host, &tls.Config{ServerName: parsed.Hostname()})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}

	var keySeed [16]byte
	if _, err := rand.Read(keySeed[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keySeed[:])
	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}
	// A deadline bounds the whole handshake; it is cleared once the
	// connection is established. Closing the conn (e.g. from Close())
	// unblocks any stuck step.
	conn.SetDeadline(time.Now().Add(timeout))
	request := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake write: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake read: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: HTTP %d", response.StatusCode)
	}
	accept := response.Header.Get("Sec-WebSocket-Accept")
	sum := sha1.Sum([]byte(key + wsGUID))
	if accept != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, errors.New("websocket handshake: invalid Sec-WebSocket-Accept")
	}
	conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, reader: bufio.NewReader(conn)}, nil
}

// writeFrame sends one masked client frame in a single write.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(payload) > wsMaxMessage {
		return fmt.Errorf("websocket frame too large: %d bytes", len(payload))
	}
	var header [14]byte
	header[0] = 0x80 | opcode
	index := 2
	switch {
	case len(payload) < 126:
		header[1] = 0x80 | byte(len(payload))
	case len(payload) <= 0xFFFF:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(len(payload)))
		index = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(len(payload)))
		index = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(header[index:index+4], mask[:])
	index += 4
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	_, err := c.conn.Write(append(header[:index], masked...))
	c.conn.SetWriteDeadline(time.Time{})
	return err
}

// readText waits for the next complete text message. Protocol-level pings
// are answered with pongs, binary messages are skipped (they are not part
// of the gateway protocol), and a close frame ends the connection with
// io.EOF.
func (c *wsConn) readText() (string, error) {
	var (
		message     []byte
		messageKind = byte(opcodeText)
		open        bool
	)
	for {
		opcode, fin, payload, err := c.readFrame()
		if err != nil {
			return "", err
		}
		switch opcode {
		case opcodePing:
			_ = c.writeFrame(opcodePong, payload)
			continue
		case opcodePong:
			continue
		case opcodeClose:
			_ = c.writeFrame(opcodeClose, nil)
			return "", io.EOF
		case opcodeText, opcodeBinary, opcodeContinuation:
			if opcode != opcodeContinuation {
				message = append(message[:0], payload...)
				messageKind = opcode
				open = true
			} else if open {
				message = append(message, payload...)
			} else {
				continue // stray continuation: ignore defensively
			}
			if len(message) > wsMaxMessage {
				return "", errors.New("websocket message too large")
			}
			if fin {
				if messageKind == opcodeBinary {
					open = false
					continue
				}
				return string(message), nil
			}
		default:
			return "", fmt.Errorf("websocket: unexpected opcode %d", opcode)
		}
	}
}

// readFrame reads one frame's header and payload, unmasking server
// payloads when (incorrectly) masked.
func (c *wsConn) readFrame() (opcode byte, fin bool, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(c.reader, head[:]); err != nil {
		return 0, false, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
			return 0, false, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
			return 0, false, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > wsMaxMessage {
		return 0, false, nil, errors.New("websocket frame too large")
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, maskKey[:]); err != nil {
			return 0, false, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, fin, payload, nil
}

// Close performs a best-effort closing handshake and always tears down the
// underlying connection, unblocking any pending Receive.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.writeFrame(opcodeClose, nil)
		err = c.conn.Close()
	})
	return err
}

// socketTransport is the default WebSocketTransport built on wsConn.
type socketTransport struct {
	dialTimeout time.Duration
	conn        *wsConn
}

func (t *socketTransport) Open(ctx context.Context, rawURL string) error {
	conn, err := dialWebSocket(ctx, rawURL, t.dialTimeout)
	if err != nil {
		return err
	}
	t.conn = conn
	return nil
}

func (t *socketTransport) Send(text string) error {
	if t.conn == nil {
		return errors.New("websocket: socket is not open")
	}
	return t.conn.writeFrame(opcodeText, []byte(text))
}

func (t *socketTransport) Receive() (string, error) {
	if t.conn == nil {
		return "", errors.New("websocket: socket is not open")
	}
	return t.conn.readText()
}

func (t *socketTransport) Close() error {
	if t.conn == nil {
		return nil
	}
	err := t.conn.Close()
	t.conn = nil
	return err
}
