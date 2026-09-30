package airwayim

// End-to-end tests for the zero-dependency RFC 6455 client against a
// hand-rolled stdlib WebSocket server: handshake acceptance hash, text
// echo, protocol-level ping/pong, fragmentation, and the closing
// handshake.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

const wsTestGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsTestServer is a minimal blocking WebSocket server for one connection.
type wsTestServer struct {
	listener  net.Listener
	conn      net.Conn
	reader    *bufio.Reader
	acceptKey string
}

func startWSTestServer(t *testing.T) *wsTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &wsTestServer{listener: listener}
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *wsTestServer) url() string {
	return "ws://" + s.listener.Addr().String() + "/ws"
}

// accept performs the opening handshake for one client.
func (s *wsTestServer) accept(t *testing.T) {
	t.Helper()
	conn, err := s.listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	request, err := httpReadRequest(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if request.Header["upgrade"] == "" || request.Header["sec-websocket-version"] == "" {
		t.Fatalf("unexpected upgrade request: %+v", request.Header)
	}
	key := request.Header["sec-websocket-key"]
	sum := sha1.Sum([]byte(key + wsTestGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(response)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	conn.SetDeadline(time.Time{})
	s.conn = conn
	s.reader = bufio.NewReader(conn)
	s.acceptKey = accept
}

// httpReadRequest is a tiny request reader (net/http.ReadRequest closes
// over the body handling we do not need).
func httpReadRequest(reader *bufio.Reader) (*request, error) {
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	request := &request{Header: map[string]string{}}
	parts := strings.Fields(requestLine)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bad request line %q", requestLine)
	}
	request.Target = parts[1]
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return request, nil
		}
		name, value, found := strings.Cut(line, ":")
		if found {
			request.Header[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
}

type request struct {
	Target string
	Header map[string]string
}

// writeServerFrame writes an unmasked server frame.
func (s *wsTestServer) writeServerFrame(opcode byte, payload []byte) error {
	frame := []byte{0x80 | opcode, byte(len(payload))}
	frame = append(frame, payload...)
	_, err := s.conn.Write(frame)
	return err
}

// readClientFrame reads one masked client frame.
func (s *wsTestServer) readClientFrame(t *testing.T) (opcode byte, payload []byte) {
	t.Helper()
	s.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer s.conn.SetReadDeadline(time.Time{})
	var head [2]byte
	if _, err := io.ReadFull(s.reader, head[:]); err != nil {
		t.Fatalf("read frame head: %v", err)
	}
	opcode = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(s.reader, ext[:]); err != nil {
			t.Fatalf("read extended length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(s.reader, ext[:]); err != nil {
			t.Fatalf("read extended length: %v", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if !masked {
		t.Fatal("client frames must be masked")
	}
	var mask [4]byte
	if _, err := io.ReadFull(s.reader, mask[:]); err != nil {
		t.Fatalf("read mask: %v", err)
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(s.reader, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload
}

func TestWebSocketHandshakeAndTextEcho(t *testing.T) {
	server := startWSTestServer(t)
	socket := &socketTransport{dialTimeout: 2 * time.Second}

	connected := make(chan error, 1)
	go func() { connected <- socket.Open(t.Context(), server.url()) }()
	server.accept(t)
	if err := <-connected; err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := socket.Send("hello 世界"); err != nil {
		t.Fatalf("send: %v", err)
	}
	opcode, payload := server.readClientFrame(t)
	if opcode != opcodeText || string(payload) != "hello 世界" {
		t.Fatalf("frame = %d %q", opcode, payload)
	}

	// Server → client text.
	if err := server.writeServerFrame(opcodeText, []byte(`{"cmd":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	text, err := socket.Receive()
	if err != nil || text != `{"cmd":"ping"}` {
		t.Fatalf("receive = %q, %v", text, err)
	}

	// Protocol-level ping is answered with a pong echoing the payload —
	// but only while a Receive drains the socket, so keep one running.
	if err := server.writeServerFrame(opcodePing, []byte("hb")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	go func() { _, _ = socket.Receive() }() // unblocked by Close below
	opcode, payload = server.readClientFrame(t)
	if opcode != opcodePong || string(payload) != "hb" {
		t.Fatalf("pong = %d %q", opcode, payload)
	}

	if err := socket.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A close frame is sent before the TCP teardown.
	opcode, _ = server.readClientFrame(t)
	if opcode != opcodeClose {
		t.Fatalf("close opcode = %d", opcode)
	}
}

// openAndWait dials the test server and waits until the transport is
// ready, mirroring the awaited Open of the production gateway loop.
func openAndWait(t *testing.T, socket *socketTransport, server *wsTestServer) {
	t.Helper()
	connected := make(chan error, 1)
	go func() { connected <- socket.Open(t.Context(), server.url()) }()
	server.accept(t)
	if err := <-connected; err != nil {
		t.Fatalf("open: %v", err)
	}
}

func TestWebSocketFragmentedMessage(t *testing.T) {
	server := startWSTestServer(t)
	socket := &socketTransport{dialTimeout: 2 * time.Second}
	openAndWait(t, socket, server)

	// Two fragments: first with opcode text (FIN=0), continuation (FIN=1).
	first := []byte{0x01, byte(len("frag"))}
	first = append(first, "frag"...)
	second := []byte{0x80, byte(len("ment"))}
	second = append(second, "ment"...)
	if _, err := server.conn.Write(append(first, second...)); err != nil {
		t.Fatalf("write fragments: %v", err)
	}
	text, err := socket.Receive()
	if err != nil || text != "fragment" {
		t.Fatalf("receive = %q, %v", text, err)
	}
}

func TestWebSocketCloseFrameEndsReceive(t *testing.T) {
	server := startWSTestServer(t)
	socket := &socketTransport{dialTimeout: 2 * time.Second}
	openAndWait(t, socket, server)

	if err := server.writeServerFrame(opcodeClose, nil); err != nil {
		t.Fatalf("write close: %v", err)
	}
	if _, err := socket.Receive(); err != io.EOF {
		t.Fatalf("receive after close = %v, want io.EOF", err)
	}
}

func TestWebSocketHandshakeRejectsPlainHTTP(t *testing.T) {
	// A plain HTTP endpoint (no upgrade) must fail the handshake.
	server := startWSTestServer(t)
	socket := &socketTransport{dialTimeout: 2 * time.Second}
	go func() {
		conn, err := server.listener.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"))
		conn.Close()
	}()
	if err := socket.Open(t.Context(), server.url()); err == nil {
		t.Fatal("expected handshake failure against a non-WebSocket endpoint")
	}
}

func TestWebSocketRejectsUnsupportedScheme(t *testing.T) {
	socket := &socketTransport{dialTimeout: time.Second}
	if err := socket.Open(t.Context(), "http://example.invalid/ws"); err == nil {
		t.Fatal("expected scheme error")
	}
}

func TestWebSocketLargePayloadRoundTrip(t *testing.T) {
	server := startWSTestServer(t)
	socket := &socketTransport{dialTimeout: 2 * time.Second}
	openAndWait(t, socket, server)

	// A 70 KiB payload exercises the 16-bit extended length encoding.
	payload := strings.Repeat("x", 70_000)
	if err := socket.Send(payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	opcode, received := server.readClientFrame(t)
	if opcode != opcodeText || len(received) != 70_000 {
		t.Fatalf("frame = %d, %d bytes", opcode, len(received))
	}

	// Server → client with a 64-bit length header.
	big := make([]byte, 70_000)
	frame := []byte{0x81, 127}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(big)))
	frame = append(frame, length[:]...)
	frame = append(frame, big...)
	if _, err := server.conn.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	text, err := socket.Receive()
	if err != nil || len(text) != 70_000 {
		t.Fatalf("receive = %d bytes, %v", len(text), err)
	}
}
