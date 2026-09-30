package airwayim

// WebSocket gateway client (deps/im/docs/design/gateway.md):
//   - connects to <wsURL>/ws ("ws://" for local dev);
//   - the first application frame must be {"cmd":"auth","opts":["<credential>"]}
//     within the server's 10-second deadline — success replies
//     {"code":0,"data":"OK"}, failure closes with 1008;
//   - {"cmd":"ping"} answers {"code":0,"data":"PONG"}; the application-level
//     heartbeat runs every pingInterval (25s default, 0 disables);
//     protocol-level ping/pong is answered by the transport;
//   - the server pushes at-least-once event frames; duplicates are dropped
//     by event_id.
//
// Never rely on the socket for missed messages — after every (re)connect,
// resync over HTTP (after_sequence), which the session does on ready.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// CredentialProvider is called once when a credential is rejected (HTTP
// 401 / code 10001, or a gateway auth failure) to fetch a fresh one from
// the host backend.
type CredentialProvider func(ctx context.Context) (string, error)

// WebSocketTransport is the socket seam the gateway client talks to. The
// default implementation speaks RFC 6455 over the standard library;
// tests and alternative runtimes plug in their own implementation.
type WebSocketTransport interface {
	// Open brings the socket up; returns once the transport is ready for
	// Send/Receive. An error means the connection could not be established.
	Open(ctx context.Context, rawURL string) error
	// Send writes one text frame.
	Send(text string) error
	// Receive waits for the next text frame. io.EOF (or any error) means
	// the socket closed.
	Receive() (string, error)
	// Close tears the socket down for good and unblocks a pending Receive.
	Close() error
}

// GatewayCallbacks are the callbacks the gateway client reports to its
// owner (the session facade).
type GatewayCallbacks struct {
	// OnEvent receives every deduplicated event frame.
	OnEvent func(event GatewayEvent)
	// OnStatus observes connection lifecycle changes.
	OnStatus func(status ConnectionStatus)
	// OnReady fires after every successful (re)authentication; resync
	// over HTTP here.
	OnReady func()
	// OnError reports gateway auth failures and transport errors.
	OnError func(err *IMError)
}

const (
	initialReconnectDelay = 500 * time.Millisecond
	maxReconnectDelay     = 10 * time.Second
	maxSeenEvents         = 10_000
)

// GatewaySocket manages the gateway connection: connect → authenticate →
// receive loop with exponential backoff reconnection and credential
// refresh on auth rejection. Create it with newGatewaySocket (via New);
// all methods are safe for concurrent use.
type GatewaySocket struct {
	callbacks     GatewayCallbacks
	socketFactory func(rawURL string) WebSocketTransport
	pingInterval  time.Duration

	mu             sync.Mutex
	url            string
	credential     string
	getCredential  CredentialProvider
	seenEventIDs   map[string]struct{}
	reconnectDelay time.Duration
	closedByUser   bool
	authenticated  bool
	authRejected   bool
	refreshedFor   string
	running        bool
	generation     uint64
	cancel         context.CancelFunc
	transport      WebSocketTransport
	pingStop       chan struct{}
}

func newGatewaySocket(
	wsURL, credential string,
	pingInterval time.Duration,
	getCredential CredentialProvider,
	socketFactory func(rawURL string) WebSocketTransport,
	callbacks GatewayCallbacks,
) *GatewaySocket {
	url := joinURL(trimTrailingSlashes(wsURL), "/ws")
	return &GatewaySocket{
		url:            url,
		credential:     credential,
		pingInterval:   pingInterval,
		getCredential:  getCredential,
		socketFactory:  socketFactory,
		callbacks:      callbacks,
		seenEventIDs:   make(map[string]struct{}),
		reconnectDelay: initialReconnectDelay,
	}
}

// SetCredential replaces the credential used by the next (re)connect.
func (s *GatewaySocket) SetCredential(credential string) {
	s.mu.Lock()
	s.credential = credential
	s.mu.Unlock()
}

// SetURL points the next (re)connect at a new gateway base URL.
func (s *GatewaySocket) SetURL(wsURL string) {
	s.mu.Lock()
	s.url = joinURL(trimTrailingSlashes(wsURL), "/ws")
	s.mu.Unlock()
}

// IsOnline reports whether the current socket is authenticated.
func (s *GatewaySocket) IsOnline() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authenticated
}

// Connect opens the connection; idempotent. Failures surface through
// callbacks.OnError and drive the reconnect loop.
func (s *GatewaySocket) Connect() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.closedByUser = false
	s.refreshedFor = ""
	s.generation++
	generation := s.generation
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	go s.runLoop(ctx, generation, cancel)
}

// Disconnect closes the connection for good; no further reconnection
// until Connect again.
func (s *GatewaySocket) Disconnect() {
	s.mu.Lock()
	s.closedByUser = true
	s.running = false
	s.authenticated = false
	s.generation++
	cancel := s.cancel
	transport := s.transport
	s.transport = nil
	pingStop := s.pingStop
	s.pingStop = nil
	s.mu.Unlock()

	if pingStop != nil {
		close(pingStop)
	}
	if cancel != nil {
		cancel()
	}
	if transport != nil {
		transport.Close()
	}
	s.setStatus(StatusClosed)
}

// ---- Run loop ----

func (s *GatewaySocket) runLoop(ctx context.Context, generation uint64, cancel context.CancelFunc) {
	defer func() {
		cancel()
		s.mu.Lock()
		if s.generation == generation {
			s.running = false
		}
		s.mu.Unlock()
	}()
	for {
		if !s.current(generation) || s.isClosedByUser() {
			return
		}
		s.openOnce(ctx, generation)

		s.mu.Lock()
		live := s.generation == generation
		closedByUser := s.closedByUser
		rejected := s.authRejected
		s.authRejected = false
		s.authenticated = false
		s.mu.Unlock()
		if !live {
			return
		}
		if closedByUser {
			return
		}

		if rejected {
			if s.refresh(ctx) {
				s.setStatus(StatusReconnecting)
				continue // reconnect either way
			}
			s.setStatus(StatusOffline)
			return
		}

		s.setStatus(StatusReconnecting)
		s.mu.Lock()
		delay := s.reconnectDelay
		s.reconnectDelay = min(delay*2, maxReconnectDelay)
		s.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
}

// refresh attempts one credential renewal after an auth rejection; it
// returns true when the loop should reconnect either way. Refreshing is
// tried once per credential value: expired credentials recover
// automatically, permanently invalid ones stop the loop.
func (s *GatewaySocket) refresh(ctx context.Context) bool {
	s.mu.Lock()
	getCredential := s.getCredential
	credential := s.credential
	refreshedFor := s.refreshedFor
	s.mu.Unlock()
	if getCredential == nil || refreshedFor == credential {
		return false
	}
	s.mu.Lock()
	s.refreshedFor = credential
	s.mu.Unlock()
	fresh, err := getCredential(ctx)
	if err != nil {
		s.callbacks.OnError(&IMError{
			Code: -1, Status: 0,
			Message: fmt.Sprintf("credential refresh failed: %v", err),
		})
	} else if fresh != "" && fresh != credential {
		s.mu.Lock()
		s.credential = fresh
		s.refreshedFor = ""
		s.mu.Unlock()
	}
	return true
}

// openOnce runs one connect → authenticate → receive cycle; it returns
// when the socket closes, errors, or the credential is rejected.
func (s *GatewaySocket) openOnce(ctx context.Context, generation uint64) {
	s.mu.Lock()
	s.authenticated = false
	s.authRejected = false
	delay := s.reconnectDelay
	url := s.url
	s.mu.Unlock()
	if delay > initialReconnectDelay {
		s.setStatus(StatusReconnecting)
	} else {
		s.setStatus(StatusConnecting)
	}

	transport := s.socketFactory(url)
	s.mu.Lock()
	s.transport = transport
	s.mu.Unlock()
	defer func() {
		s.stopPing()
		s.mu.Lock()
		if s.transport == transport {
			s.transport = nil
		}
		s.mu.Unlock()
		transport.Close()
	}()

	if err := transport.Open(ctx, url); err != nil {
		s.fail(ctx, generation, err)
		return
	}
	s.setStatus(StatusAuthenticating)

	s.mu.Lock()
	credential := s.credential
	s.mu.Unlock()
	authFrame, err := marshalBody(gatewayAuthFrame{Cmd: "auth", Opts: []string{credential}})
	if err != nil {
		s.fail(ctx, generation, err)
		return
	}
	if err := transport.Send(string(authFrame)); err != nil {
		s.fail(ctx, generation, err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		text, err := transport.Receive()
		if err != nil {
			if !s.isClosedByUser() && !s.isAuthRejected() && s.current(generation) {
				s.callbacks.OnError(imErr(err))
			}
			return
		}
		frame := parseGatewayFrame(text)
		if frame == nil {
			continue // ignore non-JSON frames
		}
		if !s.IsOnline() {
			if frame.Code != nil && *frame.Code == 0 {
				s.mu.Lock()
				s.authenticated = true
				s.reconnectDelay = initialReconnectDelay
				s.mu.Unlock()
				s.setStatus(StatusOnline)
				s.startPing()
				s.callbacks.OnReady()
			} else {
				// Auth rejected; the gateway closes with 1008 right after
				// and the receive loop unwinds.
				s.mu.Lock()
				s.authRejected = true
				s.mu.Unlock()
				message := frame.Message
				if message == "" {
					message = text
				}
				s.callbacks.OnError(&IMError{
					Code: -1, Status: 0,
					Message: fmt.Sprintf("gateway auth failed: %s", message),
				})
				return
			}
			continue
		}

		if frame.Cmd != "" && frame.Event == "" {
			continue // command replies (e.g. PONG) carry no event payload
		}
		if frame.Event == "" || frame.EventID == "" {
			continue
		}
		if s.markSeen(frame.EventID) {
			continue // at-least-once dedupe
		}
		s.callbacks.OnEvent(frame.GatewayEvent)
	}
}

func (s *GatewaySocket) fail(ctx context.Context, generation uint64, err error) {
	if !s.isClosedByUser() && !s.isAuthRejected() && s.current(generation) {
		s.callbacks.OnError(imErr(err))
	}
}

// markSeen records an event ID and reports whether it was already seen.
func (s *GatewaySocket) markSeen(eventID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.seenEventIDs[eventID]; seen {
		return true
	}
	if len(s.seenEventIDs) >= maxSeenEvents {
		s.seenEventIDs = make(map[string]struct{})
	}
	s.seenEventIDs[eventID] = struct{}{}
	return false
}

func (s *GatewaySocket) startPing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pingStop != nil {
		close(s.pingStop)
		s.pingStop = nil
	}
	if s.pingInterval <= 0 {
		return
	}
	interval := s.pingInterval
	stop := make(chan struct{})
	s.pingStop = stop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				online := s.authenticated
				transport := s.transport
				s.mu.Unlock()
				if online && transport != nil {
					_ = transport.Send(`{"cmd":"ping"}`)
				}
			}
		}
	}()
}

func (s *GatewaySocket) stopPing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pingStop != nil {
		close(s.pingStop)
		s.pingStop = nil
	}
}

func (s *GatewaySocket) current(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation == generation
}

func (s *GatewaySocket) isClosedByUser() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedByUser
}

func (s *GatewaySocket) isAuthRejected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authRejected
}

func (s *GatewaySocket) setStatus(status ConnectionStatus) {
	s.callbacks.OnStatus(status)
}

type gatewayAuthFrame struct {
	Cmd  string   `json:"cmd"`
	Opts []string `json:"opts"`
}

// gatewayFrame is a loosely-parsed gateway frame; the embedded GatewayEvent
// flattens its fields into the same JSON object.
type gatewayFrame struct {
	Cmd     string `json:"cmd"`
	Code    *int   `json:"code"`
	Message string `json:"message"`
	GatewayEvent
}

func parseGatewayFrame(text string) *gatewayFrame {
	var frame gatewayFrame
	if err := json.Unmarshal([]byte(text), &frame); err != nil {
		return nil
	}
	return &frame
}
