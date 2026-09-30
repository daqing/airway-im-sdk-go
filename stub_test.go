package airwayim

// Test infrastructure shared by the suite: a rule-based stub HTTP server
// (net/http with hijacked-connection drops, mirroring the Ruby/PHP/Swift
// suites) and a scriptable in-memory WebSocket transport the tests play
// the server with.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- Stub HTTP server ----

type capturedRequest struct {
	Method string
	// Target is the raw request target with escapes intact, e.g.
	// /api/v1/conversations/conv%201/messages?limit=2.
	Target string
	Path   string
	Query  map[string][]string
	Header http.Header
	Body   []byte
}

func (r capturedRequest) JSONBody() map[string]any {
	var body map[string]any
	_ = json.Unmarshal(r.Body, &body)
	return body
}

type stubServer struct {
	*httptest.Server

	mu       sync.Mutex
	drops    int
	requests []capturedRequest
	handler  func(req capturedRequest) (int, any)
}

// respond builds an envelope response body.
func respond(data any) any {
	return map[string]any{"code": 0, "data": data}
}

func respondError(code int, message string) any {
	return map[string]any{"code": code, "data": nil, "message": message}
}

func newStubServer(t *testing.T, handler func(req capturedRequest) (int, any)) *stubServer {
	t.Helper()
	stub := &stubServer{handler: handler}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured := capturedRequest{
			Method: r.Method,
			Target: r.RequestURI,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Header: r.Header.Clone(),
			Body:   body,
		}
		stub.mu.Lock()
		stub.requests = append(stub.requests, captured)
		drops := stub.drops
		stub.mu.Unlock()
		if drops > 0 {
			stub.mu.Lock()
			stub.drops--
			stub.mu.Unlock()
			// Drop the connection without responding: the client sees a
			// transport failure (IMError status 0).
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		status, payload := stub.handler(captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (s *stubServer) setDrops(count int) {
	s.mu.Lock()
	s.drops = count
	s.mu.Unlock()
}

func (s *stubServer) capturedRequests() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRequest(nil), s.requests...)
}

func testREST(t *testing.T, baseURL string, getCredential CredentialProvider) *RESTClient {
	t.Helper()
	return newRESTClient(baseURL, "test-credential", getCredential, NewHTTPTransport(2*time.Second))
}

// ---- Fake WebSocket transport ----

type fakeSocket struct {
	mu   sync.Mutex
	sent []string
	url  string

	frames    chan string
	done      chan struct{}
	closeOnce sync.Once
	onSend    func(fake *fakeSocket, text string)
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{frames: make(chan string, 64), done: make(chan struct{})}
}

func (f *fakeSocket) Open(_ context.Context, rawURL string) error {
	f.mu.Lock()
	f.url = rawURL
	f.mu.Unlock()
	return nil
}

func (f *fakeSocket) Send(text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	onSend := f.onSend
	f.mu.Unlock()
	if onSend != nil {
		onSend(f, text)
	}
	return nil
}

func (f *fakeSocket) Receive() (string, error) {
	select {
	case text := <-f.frames:
		return text, nil
	case <-f.done:
		return "", io.EOF
	}
}

func (f *fakeSocket) Close() error {
	f.serverClose()
	return nil
}

// ---- Server side (test controls) ----

func (f *fakeSocket) serverEnqueue(text string) {
	select {
	case f.frames <- text:
	case <-f.done:
	}
}

func (f *fakeSocket) serverClose() {
	f.closeOnce.Do(func() { close(f.done) })
}

func (f *fakeSocket) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeSocket) openedURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.url
}

// answerAuthOK replies {"code":0,"data":"OK"} to every auth frame.
func (f *fakeSocket) answerAuthOK() {
	f.mu.Lock()
	f.onSend = func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			go fake.serverEnqueue(`{"code":0,"data":"OK"}`)
		}
	}
	f.mu.Unlock()
}

// answerAuthRejected replies a failure to every auth frame.
func (f *fakeSocket) answerAuthRejected(code int, message string) {
	f.mu.Lock()
	f.onSend = func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			go fake.serverEnqueue(fmt.Sprintf(`{"code":%d,"message":%q}`, code, message))
		}
	}
	f.mu.Unlock()
}

type fakeSocketFactory struct {
	mu      sync.Mutex
	sockets []*fakeSocket
	onSend  func(fake *fakeSocket, text string)
}

func (ff *fakeSocketFactory) make(rawURL string) WebSocketTransport {
	socket := newFakeSocket()
	socket.mu.Lock()
	socket.onSend = ff.onSend
	socket.mu.Unlock()
	ff.mu.Lock()
	ff.sockets = append(ff.sockets, socket)
	ff.mu.Unlock()
	return socket
}

func (ff *fakeSocketFactory) all() []*fakeSocket {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return append([]*fakeSocket(nil), ff.sockets...)
}

func (ff *fakeSocketFactory) last() *fakeSocket {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.sockets) == 0 {
		return nil
	}
	return ff.sockets[len(ff.sockets)-1]
}

// answerAuthOnSend installs a per-factory send hook applied to every
// socket it creates.
func (ff *fakeSocketFactory) setOnSend(onSend func(fake *fakeSocket, text string)) {
	ff.mu.Lock()
	ff.onSend = onSend
	ff.mu.Unlock()
	for _, socket := range ff.all() {
		socket.mu.Lock()
		socket.onSend = onSend
		socket.mu.Unlock()
	}
}

// ---- Polling helpers ----

func waitFor(t *testing.T, label string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for: %s", label)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// capture collects values from concurrent goroutines.
type capture[T any] struct {
	mu    sync.Mutex
	items []T
}

func (c *capture[T]) append(item T) {
	c.mu.Lock()
	c.items = append(c.items, item)
	c.mu.Unlock()
}

func (c *capture[T]) all() []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]T(nil), c.items...)
}

func (c *capture[T]) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *capture[T]) last() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) == 0 {
		var zero T
		return zero, false
	}
	return c.items[len(c.items)-1], true
}

// eventFrame builds a gateway event frame as a JSON string.
func eventFrame(eventID, event, conversationID string, sequence int64, messageID string) string {
	object := map[string]any{
		"event_id":        eventID,
		"event":           event,
		"conversation_id": conversationID,
	}
	if sequence > 0 {
		object["sequence"] = sequence
	}
	if messageID != "" {
		object["message_id"] = messageID
	}
	data, _ := json.Marshal(object)
	return string(data)
}
