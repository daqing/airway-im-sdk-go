package airwayim

// Thread-safe hub shared by the session, the sync engine, and the
// gateway: owns the listener lists, the connection status, and the
// conversation registry. Gateway events are pumped through a channel so
// frames are handled one at a time, in arrival order.

import "sync"

// Subscription is the cancellation handle returned by the various On…
// registrations; call Cancel to unsubscribe early.
type Subscription struct {
	once   sync.Once
	cancel func()
}

// Cancel unsubscribes the listener; safe to call more than once.
func (s *Subscription) Cancel() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// listenerList is a multicast listener list: thread-safe
// add/remove/emit, so events can be delivered from any goroutine. A
// panicking listener is recovered and routed to panicHandler (the
// session's error stream) instead of crashing the emitting goroutine —
// the same guarantee the TypeScript SDK gives its event loop.
type listenerList[T any] struct {
	mu           sync.RWMutex
	nextID       int64
	listeners    map[int64]func(T)
	panicHandler func(recovered any)
}

func newListenerList[T any](panicHandler func(recovered any)) *listenerList[T] {
	return &listenerList[T]{listeners: make(map[int64]func(T)), panicHandler: panicHandler}
}

func (l *listenerList[T]) add(handler func(T)) *Subscription {
	l.mu.Lock()
	id := l.nextID
	l.nextID++
	l.listeners[id] = handler
	l.mu.Unlock()
	return &Subscription{cancel: func() {
		l.mu.Lock()
		delete(l.listeners, id)
		l.mu.Unlock()
	}}
}

func (l *listenerList[T]) emit(value T) {
	l.mu.RLock()
	snapshot := make([]func(T), 0, len(l.listeners))
	for _, listener := range l.listeners {
		snapshot = append(snapshot, listener)
	}
	l.mu.RUnlock()
	for _, listener := range snapshot {
		l.invoke(listener, value)
	}
}

func (l *listenerList[T]) invoke(listener func(T), value T) {
	defer func() {
		if recovered := recover(); recovered != nil && l.panicHandler != nil {
			l.panicHandler(recovered)
		}
	}()
	listener(value)
}

// conversationRegistry maps conversation ids → handle objects and
// remembered kinds. Member management is group-only server-side, so
// member events also mark their conversation as a group in the kind
// registry.
type conversationRegistry struct {
	mu            sync.Mutex
	conversations map[string]Conversation
	kinds         map[string]ConversationKind
}

func newConversationRegistry() *conversationRegistry {
	return &conversationRegistry{
		conversations: make(map[string]Conversation),
		kinds:         make(map[string]ConversationKind),
	}
}

func (r *conversationRegistry) conversation(id string) Conversation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conversations[id]
}

// directHandle returns the same object (with its listeners) for every
// open/create call on that conversation.
func (r *conversationRegistry) directHandle(id string, im *Session) *DirectConversation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.conversations[id].(*DirectConversation); ok {
		return existing
	}
	conversation := newDirectConversation(id, im)
	r.conversations[id] = conversation
	return conversation
}

func (r *conversationRegistry) groupHandle(id string, title *string, im *Session) *GroupConversation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.conversations[id].(*GroupConversation); ok {
		return existing
	}
	conversation := newGroupConversation(id, title, im)
	r.conversations[id] = conversation
	return conversation
}

// handle returns the handle for any conversation by id; the kind decides
// the concrete type when it must be created.
func (r *conversationRegistry) handle(id string, kind ConversationKind, im *Session) Conversation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.conversations[id]; ok {
		return existing
	}
	var conversation Conversation
	if kind == ConversationKindDirect {
		conversation = newDirectConversation(id, im)
	} else {
		conversation = newGroupConversation(id, nil, im)
	}
	r.conversations[id] = conversation
	return conversation
}

func (r *conversationRegistry) rememberKind(id string, kind ConversationKind) {
	r.mu.Lock()
	r.kinds[id] = kind
	r.mu.Unlock()
}

func (r *conversationRegistry) kind(id string) (ConversationKind, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kind, ok := r.kinds[id]
	return kind, ok
}

// removeKind drops the remembered kind only (ForgetConversation
// semantics — the handle object stays for whoever still holds it).
func (r *conversationRegistry) removeKind(id string) {
	r.mu.Lock()
	delete(r.kinds, id)
	r.mu.Unlock()
}

const eventChannelBuffer = 1024

type eventHub struct {
	registry *conversationRegistry
	events   chan GatewayEvent
	closed   chan struct{}

	closeOnce sync.Once
	statusMu  sync.RWMutex
	status    ConnectionStatus

	statusListeners         *listenerList[ConnectionStatus]
	errorListeners          *listenerList[*IMError]
	rawEventListeners       *listenerList[GatewayEvent]
	messageListeners        *listenerList[ChatMessage]
	messageUpdatedListeners *listenerList[ChatMessage]
	membersAddedListeners   *listenerList[MembersAddedInfo]
	membersRemovedListeners *listenerList[MembersRemovedInfo]
}

func newEventHub() *eventHub {
	hub := &eventHub{
		registry: newConversationRegistry(),
		events:   make(chan GatewayEvent, eventChannelBuffer),
		closed:   make(chan struct{}),
		status:   StatusClosed,
	}
	hub.statusListeners = newListenerList[ConnectionStatus](hub.onPanic)
	hub.errorListeners = newListenerList[*IMError](hub.onPanic)
	hub.rawEventListeners = newListenerList[GatewayEvent](hub.onPanic)
	hub.messageListeners = newListenerList[ChatMessage](hub.onPanic)
	hub.messageUpdatedListeners = newListenerList[ChatMessage](hub.onPanic)
	hub.membersAddedListeners = newListenerList[MembersAddedInfo](hub.onPanic)
	hub.membersRemovedListeners = newListenerList[MembersRemovedInfo](hub.onPanic)
	return hub
}

func (h *eventHub) onPanic(recovered any) {
	h.emitError(&IMError{Code: -1, Status: 0, Message: "event listener panicked"})
	_ = recovered
}

// pushEvent feeds a gateway event into the serialized pump; it drops
// events once the owning session is closed.
func (h *eventHub) pushEvent(event GatewayEvent) {
	select {
	case h.events <- event:
	case <-h.closed:
	}
}

// close stops event delivery; the pump drains and exits.
func (h *eventHub) close() {
	h.closeOnce.Do(func() { close(h.closed) })
}

func (h *eventHub) setStatus(status ConnectionStatus) {
	h.statusMu.Lock()
	h.status = status
	h.statusMu.Unlock()
	h.statusListeners.emit(status)
}

func (h *eventHub) currentStatus() ConnectionStatus {
	h.statusMu.RLock()
	defer h.statusMu.RUnlock()
	return h.status
}

func (h *eventHub) emitError(err *IMError) {
	h.errorListeners.emit(err)
}

func (h *eventHub) emitMessages(messages []ChatMessage) {
	for _, message := range messages {
		h.messageListeners.emit(message)
		if conversation := h.registry.conversation(message.ConversationID); conversation != nil {
			conversation.emitLocalMessage(message)
		}
	}
}

func (h *eventHub) emitMessageUpdated(message ChatMessage) {
	h.messageUpdatedListeners.emit(message)
	if conversation := h.registry.conversation(message.ConversationID); conversation != nil {
		conversation.emitLocalMessageUpdated(message)
	}
}

func (h *eventHub) emitMembersAdded(info MembersAddedInfo) {
	h.membersAddedListeners.emit(info)
	if conversation := h.registry.conversation(info.ConversationID); conversation != nil {
		conversation.emitLocalMembersAdded(info)
	}
}

func (h *eventHub) emitMembersRemoved(info MembersRemovedInfo) {
	h.membersRemovedListeners.emit(info)
	if conversation := h.registry.conversation(info.ConversationID); conversation != nil {
		conversation.emitLocalMembersRemoved(info)
	}
}
