package airwayim

// AirwayIM: the high-level entry point combining the REST client, the
// WebSocket gateway, and the sequence-based sync engine behind one typed
// event-emitting facade. This is what client applications should use.
//
//	im := airwayim.New(
//	    "https://im.example.com", // IM backend (:1905)
//	    "wss://im.example.com",   // gateway (:1910); "" for REST-only use
//	    credential,               // host-issued credential from your backend
//	    airwayim.WithGetCredential(fetchFreshCredentialFromYourBackend),
//	)
//	im.OnMessage(func(message airwayim.ChatMessage) { ... })
//	im.Connect()
//
// Every method is safe for concurrent use; share one Session across your
// whole application. Event handlers run on their own goroutines — hop to
// your UI thread yourself before touching UI.

import (
	"context"
	"sync"
	"time"
)

// Option customizes a Session.
type Option func(*options)

type options struct {
	wsURL            string
	getCredential    CredentialProvider
	timeout          time.Duration
	pingInterval     time.Duration
	persistSequences bool
	sequenceStore    SequenceStore
	httpTransport    HTTPTransport
	socketFactory    func(rawURL string) WebSocketTransport
	autoConnect      bool
}

func defaultOptions() *options {
	return &options{
		timeout:          15 * time.Second,
		pingInterval:     25 * time.Second,
		persistSequences: true,
	}
}

// WithGetCredential registers the callback invoked once when the
// credential is rejected (HTTP 401/10001 or gateway auth failure) to
// fetch a fresh one from your backend — e.g. via your login session. Keep
// it fast; realtime resumes automatically with the returned credential.
func WithGetCredential(getCredential CredentialProvider) Option {
	return func(o *options) { o.getCredential = getCredential }
}

// WithTimeout sets the REST request timeout (default 15s).
func WithTimeout(timeout time.Duration) Option {
	return func(o *options) { o.timeout = timeout }
}

// WithPingInterval sets the gateway application-level heartbeat interval
// (default 25s; 0 disables).
func WithPingInterval(interval time.Duration) Option {
	return func(o *options) { o.pingInterval = interval }
}

// WithPersistSequences disables persisting per-conversation last-seen
// sequences (cursors then live in memory only), unless a store is given
// via WithSequenceStore.
func WithPersistSequences(enabled bool) Option {
	return func(o *options) { o.persistSequences = enabled }
}

// WithSequenceStore sets where sequence cursors persist; without it,
// enabled sessions persist into an in-process store. Pass
// WithPersistSequences(false) to keep cursors in memory only.
func WithSequenceStore(store SequenceStore) Option {
	return func(o *options) { o.sequenceStore = store }
}

// WithHTTPTransport replaces the REST transport (tests, proxies).
func WithHTTPTransport(transport HTTPTransport) Option {
	return func(o *options) { o.httpTransport = transport }
}

// WithSocketFactory replaces the WebSocket transport factory (tests,
// alternative dialers).
func WithSocketFactory(factory func(rawURL string) WebSocketTransport) Option {
	return func(o *options) { o.socketFactory = factory }
}

// WithAutoConnect connects the gateway immediately (default false; call
// Connect yourself).
func WithAutoConnect(enabled bool) Option {
	return func(o *options) { o.autoConnect = enabled }
}

// Session is the owner of the REST client, the gateway connection, the
// sync engine, and the conversation handles.
type Session struct {
	REST *RESTClient

	hub     *eventHub
	sync    *syncEngine
	gateway *GatewaySocket

	ctx      context.Context
	cancel   context.CancelFunc
	closeMu  sync.Mutex
	closed   bool
	pumpOnce sync.Once
}

// New creates a session.
//
// apiURL is the IM backend base URL, e.g. https://im.example.com (the
// :1905 service). wsURL is the gateway WebSocket base URL, e.g.
// wss://im.example.com (the :1910 service); the SDK connects to
// <wsURL>/ws — pass "" for REST-only use. credential is the host-signed
// credential issued by your own backend.
func New(apiURL, wsURL, credential string, opts ...Option) *Session {
	options := defaultOptions()
	for _, opt := range opts {
		opt(options)
	}

	ctx, cancel := context.WithCancel(context.Background())
	hub := newEventHub()
	transport := options.httpTransport
	if transport == nil {
		transport = NewHTTPTransport(options.timeout)
	}

	session := &Session{
		hub:    hub,
		ctx:    ctx,
		cancel: cancel,
	}
	session.REST = newRESTClient(apiURL, credential, options.getCredential, transport)

	store := options.sequenceStore
	if options.persistSequences && store == nil {
		store = NewInMemorySequenceStore()
	}
	if !options.persistSequences {
		store = nil
	}
	session.sync = newSyncEngine(session.REST, syncHandlers{
		onMessages:       hub.emitMessages,
		onMessageUpdated: hub.emitMessageUpdated,
	}, store)

	if wsURL != "" {
		factory := options.socketFactory
		if factory == nil {
			factory = func(rawURL string) WebSocketTransport {
				return &socketTransport{dialTimeout: options.timeout}
			}
		}
		session.gateway = newGatewaySocket(
			wsURL,
			credential,
			options.pingInterval,
			options.getCredential,
			factory,
			GatewayCallbacks{
				OnEvent: hub.pushEvent,
				OnStatus: func(status ConnectionStatus) {
					hub.setStatus(status)
				},
				OnReady: func() {
					// Realtime resync after every successful
					// (re)authentication.
					go func() {
						if err := session.resync(session.ctx); err != nil {
							hub.emitError(imErr(err))
						}
					}()
				},
				OnError: hub.emitError,
			})
	}

	session.startPump()
	if options.autoConnect {
		session.Connect()
	}
	return session
}

// startPump runs the serialized event pump: gateway frames are handled
// one at a time, in arrival order.
func (s *Session) startPump() {
	s.pumpOnce.Do(func() {
		go func() {
			for {
				select {
				case <-s.ctx.Done():
					return
				case event := <-s.hub.events:
					s.handleEvent(s.ctx, event)
				}
			}
		}()
	})
}

// ---- Events ----

// OnMessage subscribes to new messages: deduplicated and ordered;
// includes your own sends.
func (s *Session) OnMessage(handler func(ChatMessage)) *Subscription {
	return s.hub.messageListeners.add(handler)
}

// OnMessageUpdated fires when a previously seen message was masked by
// moderation (content "***").
func (s *Session) OnMessageUpdated(handler func(ChatMessage)) *Subscription {
	return s.hub.messageUpdatedListeners.add(handler)
}

// OnMembersAdded fires when group members were added (including the added
// users themselves).
func (s *Session) OnMembersAdded(handler func(MembersAddedInfo)) *Subscription {
	return s.hub.membersAddedListeners.add(handler)
}

// OnMembersRemoved fires when a group member was removed (a kicked user
// is notified too).
func (s *Session) OnMembersRemoved(handler func(MembersRemovedInfo)) *Subscription {
	return s.hub.membersRemovedListeners.add(handler)
}

// OnStatus observes connection lifecycle changes.
func (s *Session) OnStatus(handler func(ConnectionStatus)) *Subscription {
	return s.hub.statusListeners.add(handler)
}

// OnError observes gateway auth failures, credential renewal failures,
// and listener wiring problems.
func (s *Session) OnError(handler func(*IMError)) *Subscription {
	return s.hub.errorListeners.add(handler)
}

// OnEvent observes every raw gateway frame, including events for
// untracked conversations (use for unread badges and similar cases).
func (s *Session) OnEvent(handler func(GatewayEvent)) *Subscription {
	return s.hub.rawEventListeners.add(handler)
}

// ConnectionStatus is the latest reported connection status
// (StatusClosed before connecting).
func (s *Session) ConnectionStatus() ConnectionStatus {
	return s.hub.currentStatus()
}

// ---- Realtime ----

// Connect opens the gateway connection (no-op in REST-only mode).
func (s *Session) Connect() {
	if s.gateway == nil {
		s.hub.emitError(&IMError{
			Code: -1, Status: 0,
			Message: "No wsURL configured; realtime is disabled",
		})
		return
	}
	if s.isClosed() {
		s.hub.emitError(&IMError{Code: -1, Status: 0, Message: "session is closed"})
		return
	}
	s.gateway.Connect()
}

// Disconnect closes the gateway connection and stops reconnecting.
func (s *Session) Disconnect() {
	if s.gateway != nil {
		s.gateway.Disconnect()
	}
}

// IsOnline reports whether the gateway socket is authenticated (false in
// REST-only mode).
func (s *Session) IsOnline() bool {
	if s.gateway == nil {
		return false
	}
	return s.gateway.IsOnline()
}

// SetCredential replaces the credential everywhere (e.g. after your own
// re-login flow).
func (s *Session) SetCredential(credential string) {
	s.REST.SetCredential(credential)
	if s.gateway != nil {
		s.gateway.SetCredential(credential)
	}
}

// Close disconnects the gateway and stops the event pump and pending
// syncs. The session cannot be reused afterwards.
func (s *Session) Close() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	s.closeMu.Unlock()
	if s.gateway != nil {
		s.gateway.Disconnect()
	}
	s.hub.close()
	s.cancel()
}

func (s *Session) isClosed() bool {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closed
}

// ---- Event routing ----

func (s *Session) handleEvent(ctx context.Context, event GatewayEvent) {
	s.hub.rawEventListeners.emit(event)
	switch event.Event {
	case EventMessageCreated:
		go s.sync.HandleMessageEvent(ctx, event)
	case EventMessageModerated:
		go s.sync.HandleMessageModeratedEvent(ctx, event)
	case EventMemberAdded:
		// Member management is group-only server-side, so these events
		// mark the conversation as a group in the kind registry.
		s.hub.registry.rememberKind(event.ConversationID, ConversationKindGroup)
		if len(event.AddedUserUUIDs) > 0 {
			s.hub.emitMembersAdded(MembersAddedInfo{
				ConversationID: event.ConversationID,
				AddedUserUUIDs: event.AddedUserUUIDs,
				Event:          event,
			})
		}
	case EventMemberRemoved:
		s.hub.registry.rememberKind(event.ConversationID, ConversationKindGroup)
		if event.RemovedUserUUID != "" {
			s.hub.emitMembersRemoved(MembersRemovedInfo{
				ConversationID:  event.ConversationID,
				RemovedUserUUID: event.RemovedUserUUID,
				Event:           event,
			})
		}
	}
}

// resync reloads every tracked conversation.
func (s *Session) resync(ctx context.Context) error {
	s.sync.ResyncAll(ctx)
	return nil
}

// ---- REST passthrough ----

// Me returns the authenticated user profile.
func (s *Session) Me(ctx context.Context) (User, error) {
	return s.REST.Me(ctx)
}

// ListGroups lists my active group conversations (direct ones are
// excluded by the backend).
func (s *Session) ListGroups(ctx context.Context) ([]ConversationSummary, error) {
	conversations, err := s.REST.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, conversation := range conversations {
		s.hub.registry.rememberKind(conversation.ID, ConversationKindGroup)
	}
	return conversations, nil
}

// CreateDirect get-or-creates the direct (1:1) conversation with one
// user, by uuid.
func (s *Session) CreateDirect(ctx context.Context, otherUserUUID string) (*DirectConversation, error) {
	conversation, err := s.REST.CreateDirect(ctx, otherUserUUID)
	if err != nil {
		return nil, err
	}
	s.hub.registry.rememberKind(conversation.ID, ConversationKindDirect)
	return s.hub.registry.directHandle(conversation.ID, s), nil
}

// GetDirect returns the existing direct conversation with one user, by
// uuid, as a handle — or nil when none exists yet (CreateDirect
// get-or-creates instead).
func (s *Session) GetDirect(ctx context.Context, otherUserUUID string) (*DirectConversation, error) {
	conversation, err := s.REST.GetDirect(ctx, otherUserUUID)
	if err != nil || conversation == nil {
		return nil, err
	}
	s.hub.registry.rememberKind(conversation.ID, ConversationKindDirect)
	return s.hub.registry.directHandle(conversation.ID, s), nil
}

// CreateGroup creates a group conversation; the authenticated user
// becomes its owner. A nil title creates the group without one.
func (s *Session) CreateGroup(ctx context.Context, title *string, memberUUIDs []string) (*GroupConversation, error) {
	conversation, err := s.REST.CreateGroup(ctx, title, memberUUIDs)
	if err != nil {
		return nil, err
	}
	s.hub.registry.rememberKind(conversation.ID, ConversationKindGroup)
	return s.hub.registry.groupHandle(conversation.ID, conversation.Title, s), nil
}

// OpenConversation opens any conversation by id as a conversation object
// (e.g. one learned from a message event or ListGroups). The kind is
// answered from the registry when this session already saw the
// conversation, otherwise fetched via REST once; it fails if the user
// cannot see the conversation.
func (s *Session) OpenConversation(ctx context.Context, conversationID string) (Conversation, error) {
	if existing := s.hub.registry.conversation(conversationID); existing != nil {
		return existing, nil
	}
	kind, known := s.hub.registry.kind(conversationID)
	if !known {
		details, err := s.REST.GetConversation(ctx, conversationID)
		if err != nil {
			return nil, err
		}
		kind = details.Type
		s.hub.registry.rememberKind(details.ConversationUUID, kind)
	}
	return s.hub.registry.handle(conversationID, kind, s), nil
}

// AddMembers adds members (by uuid) to a group (owner/admin; idempotent
// for already-active members).
func (s *Session) AddMembers(ctx context.Context, conversationID string, memberUUIDs []string) (ConversationDetails, error) {
	// Member management is group-only server-side.
	s.hub.registry.rememberKind(conversationID, ConversationKindGroup)
	return s.REST.AddMembers(ctx, conversationID, memberUUIDs)
}

// RemoveMembers removes members (by uuid) from a group (owner/admin;
// cannot remove self or the owner).
func (s *Session) RemoveMembers(ctx context.Context, conversationID string, userUUIDs []string) (ConversationDetails, error) {
	s.hub.registry.rememberKind(conversationID, ConversationKindGroup)
	return s.REST.RemoveMembers(ctx, conversationID, userUUIDs)
}

// History is the initial load for a conversation: fetch messages after
// opts.FromSequence (default: last persisted sequence, else 0), track the
// sequence, and emit each message via OnMessage handlers. After this, the
// conversation is tracked and realtime events auto-heal gaps for it.
func (s *Session) History(ctx context.Context, conversationID string, opts HistoryOptions) ([]ChatMessage, error) {
	return s.sync.FetchFrom(ctx, conversationID, opts)
}

// ListMessages returns a raw ordered message page after a sequence (no
// state changes, no event emission). Most applications should use History
// + realtime events instead.
func (s *Session) ListMessages(ctx context.Context, conversationID string, opts ListMessagesOptions) ([]ChatMessage, error) {
	return s.REST.ListMessages(ctx, conversationID, opts)
}

// SendGroupMessage sends a message to a group conversation by id. It
// returns the stored message; the local sequence tracker is updated so
// the sender's own message.created event does not trigger a redundant
// fetch. The message is emitted via OnMessage only when it arrives back
// through realtime/sync (at-least-once) — use the return value for
// immediate UI feedback.
func (s *Session) SendGroupMessage(ctx context.Context, conversationID, content string, opts *SendOptions) (ChatMessage, error) {
	s.hub.registry.rememberKind(conversationID, ConversationKindGroup)
	message, err := s.REST.SendMessage(ctx, conversationID, content, opts)
	if err != nil {
		return ChatMessage{}, err
	}
	s.sync.Track(conversationID, message.Sequence)
	return message, nil
}

// SendDirectMessage sends a direct message to one other user, identified
// by their uuid: get-or-create the direct conversation, then send. Same
// idempotency semantics as SendGroupMessage.
func (s *Session) SendDirectMessage(ctx context.Context, otherUserUUID, content string, opts *SendOptions) (ChatMessage, error) {
	message, err := s.REST.SendDirectMessage(ctx, otherUserUUID, content, opts)
	if err != nil {
		return ChatMessage{}, err
	}
	s.sync.Track(message.ConversationID, message.Sequence)
	return message, nil
}

// LastSequence is the last synced sequence in a conversation.
func (s *Session) LastSequence(conversationID string) int64 {
	return s.sync.LastSequence(conversationID)
}

// ForgetConversation drops all sync state for a conversation (e.g. after
// being kicked).
func (s *Session) ForgetConversation(conversationID string) {
	s.sync.Forget(conversationID)
	s.hub.registry.removeKind(conversationID)
}

// ensureTracked starts tracking a conversation when its first message
// listener registers.
func (s *Session) ensureTracked(conversationID string) {
	if s.sync.IsTracked(conversationID) {
		return
	}
	if _, err := s.History(s.ctx, conversationID, HistoryOptions{}); err != nil {
		s.hub.emitError(imErr(err))
	}
}

// ---- Storage ----

// UploadFile uploads a file; returns {key, url, size}.
func (s *Session) UploadFile(ctx context.Context, input UploadInput) (UploadResult, error) {
	return s.REST.UploadFile(ctx, input)
}

// UploadFileFromPath reads path from disk and uploads it.
func (s *Session) UploadFileFromPath(ctx context.Context, path string, dir string) (UploadResult, error) {
	return s.REST.UploadFileFromPath(ctx, path, dir)
}

// StorageURL returns the public URL for a storage key (for <img src>,
// downloads, …).
func (s *Session) StorageURL(key string) string {
	return s.REST.StorageURL(key)
}
