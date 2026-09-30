package airwayim

// Sequence-based synchronization engine.
//
// The backend guarantees per-conversation monotonic sequence values and
// at-least-once event delivery, so clients deduplicate by message_id /
// event_id, order by (conversation_id, sequence), and heal gaps by
// fetching after_sequence (see deps/im/docs/api/openapi.md, "Client
// requirements").
//
// This engine owns the per-conversation last-seen sequence. All fetches
// for one conversation run serialized — FetchFrom and realtime-driven
// drains can never interleave, so each message is emitted exactly once,
// in order. Conversations become tracked through Track/FetchFrom; events
// for untracked conversations are left to the application (surfaced via
// the raw event handler).

import (
	"context"
	"encoding/json"
	"sync"
)

const (
	syncPageLimit = 200
	// syncMaxSyncPages is the upper bound of pages fetched per drain
	// cycle (10 × 200 messages).
	syncMaxSyncPages = 10

	syncStorageKey = "airway-im.sequences"
)

// HistoryOptions carries the optional FetchFrom inputs. The zero value
// fetches from the last synced sequence (0 for fresh conversations) with
// the default page size (200).
type HistoryOptions struct {
	// FromSequence overrides the starting cursor (fetches messages with
	// sequence > FromSequence).
	FromSequence int64
	// Limit caps each fetched page (1-200; the backend falls back to 100
	// outside that range).
	Limit int
}

type syncHandlers struct {
	onMessages       func([]ChatMessage)
	onMessageUpdated func(ChatMessage)
}

type syncEngine struct {
	rest     *RESTClient
	handlers syncHandlers
	store    SequenceStore

	mu        sync.Mutex
	lastSeq   map[string]int64
	convLocks map[string]*sync.Mutex
}

func newSyncEngine(rest *RESTClient, handlers syncHandlers, store SequenceStore) *syncEngine {
	if handlers.onMessages == nil {
		handlers.onMessages = func([]ChatMessage) {}
	}
	if handlers.onMessageUpdated == nil {
		handlers.onMessageUpdated = func(ChatMessage) {}
	}
	engine := &syncEngine{
		rest:      rest,
		handlers:  handlers,
		store:     store,
		lastSeq:   make(map[string]int64),
		convLocks: make(map[string]*sync.Mutex),
	}
	engine.lastSeq = engine.loadSequences()
	return engine
}

// ---- Sequence bookkeeping ----

// Track marks a conversation as tracked, raising its last-seen sequence.
func (e *syncEngine) Track(conversationID string, sequence int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	current, tracked := e.lastSeq[conversationID]
	switch {
	case !tracked:
		e.lastSeq[conversationID] = max(0, sequence)
		e.persistLocked()
	case sequence > current:
		e.lastSeq[conversationID] = sequence
		e.persistLocked()
	}
}

func (e *syncEngine) IsTracked(conversationID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, tracked := e.lastSeq[conversationID]
	return tracked
}

func (e *syncEngine) LastSequence(conversationID string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastSeq[conversationID]
}

// Forget drops all sync state for a conversation.
func (e *syncEngine) Forget(conversationID string) {
	e.mu.Lock()
	delete(e.lastSeq, conversationID)
	delete(e.convLocks, conversationID)
	e.persistLocked()
	e.mu.Unlock()
}

// loadSequences restores the persisted cursors; a corrupted cache starts
// clean rather than fail.
func (e *syncEngine) loadSequences() map[string]int64 {
	if e.store == nil {
		return make(map[string]int64)
	}
	raw, ok := e.store.Get(syncStorageKey)
	if !ok {
		return make(map[string]int64)
	}
	var stored map[string]int64
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return make(map[string]int64)
	}
	sequences := make(map[string]int64, len(stored))
	for id, sequence := range stored {
		if sequence > 0 {
			sequences[id] = sequence
		}
	}
	return sequences
}

func (e *syncEngine) persistLocked() {
	if e.store == nil {
		return
	}
	data, err := marshalBody(e.lastSeq)
	if err != nil {
		return
	}
	e.store.Set(syncStorageKey, string(data))
}

// ---- Serialized fetching ----

// convMutex returns the per-conversation mutex serializing all of its
// fetches; it is never held while acquiring e.mu.
func (e *syncEngine) convMutex(conversationID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	mutex, ok := e.convLocks[conversationID]
	if !ok {
		mutex = &sync.Mutex{}
		e.convLocks[conversationID] = mutex
	}
	return mutex
}

// FetchFrom runs a serialized per-conversation fetch and returns the
// messages it emitted.
func (e *syncEngine) FetchFrom(ctx context.Context, conversationID string, opts HistoryOptions) ([]ChatMessage, error) {
	mutex := e.convMutex(conversationID)
	mutex.Lock()
	defer mutex.Unlock()
	return e.runFetch(ctx, conversationID, opts)
}

func (e *syncEngine) runFetch(ctx context.Context, conversationID string, opts HistoryOptions) ([]ChatMessage, error) {
	pageSize := opts.Limit
	if pageSize <= 0 {
		pageSize = syncPageLimit
	}
	if pageSize > syncPageLimit {
		pageSize = syncPageLimit
	}
	from := opts.FromSequence
	if from == 0 {
		from = e.LastSequence(conversationID)
	}
	var all []ChatMessage
	for range syncMaxSyncPages {
		messages, err := e.rest.ListMessages(ctx, conversationID, ListMessagesOptions{
			AfterSequence: from,
			Limit:         pageSize,
		})
		if err != nil {
			return all, err
		}
		if len(messages) == 0 {
			break
		}
		all = append(all, messages...)
		e.handlers.onMessages(messages)
		from = messages[len(messages)-1].Sequence
		if len(messages) < pageSize {
			break
		}
	}
	// Track even when the page was empty: the caller has seen everything
	// up to from (0 for fresh conversations), so later events can sync.
	e.Track(conversationID, from)
	return all, nil
}

// ReloadMessage serializes a reload of one masked (moderated) message.
func (e *syncEngine) ReloadMessage(ctx context.Context, conversationID string, event GatewayEvent) (*ChatMessage, error) {
	mutex := e.convMutex(conversationID)
	mutex.Lock()
	defer mutex.Unlock()
	if event.Sequence <= 0 {
		return nil, nil
	}
	// The sequence may already be cached; reload starting at sequence - 1
	// and replace the local message with the masked API response.
	page, err := e.rest.ListMessages(ctx, conversationID, ListMessagesOptions{
		AfterSequence: event.Sequence - 1,
		Limit:         10,
	})
	if err != nil {
		return nil, err
	}
	for i := range page {
		if page[i].ID == event.MessageID {
			return &page[i], nil
		}
	}
	return nil, nil
}

// ResyncAll resynchronizes every tracked conversation after a
// (re)connect.
func (e *syncEngine) ResyncAll(ctx context.Context) {
	e.mu.Lock()
	ids := make([]string, 0, len(e.lastSeq))
	for id := range e.lastSeq {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.FetchFrom(ctx, id, HistoryOptions{})
		}()
	}
	wg.Wait()
}

// ---- Event-driven sync ----

// HandleMessageEvent handles a gateway message.created event (serialized
// per conversation).
func (e *syncEngine) HandleMessageEvent(ctx context.Context, event GatewayEvent) {
	conversationID := event.ConversationID
	if !e.IsTracked(conversationID) {
		return // untracked: app decides
	}
	if event.Sequence > 0 && event.Sequence <= e.LastSequence(conversationID) {
		return // duplicate or already-applied (e.g. our own send)
	}
	// The next event for this conversation retries; nothing is lost
	// because the sequence tracker still points at the last applied
	// message.
	_, _ = e.FetchFrom(ctx, conversationID, HistoryOptions{})
}

// HandleMessageModeratedEvent handles a gateway message.moderated event
// by reloading the masked message.
func (e *syncEngine) HandleMessageModeratedEvent(ctx context.Context, event GatewayEvent) {
	if !e.IsTracked(event.ConversationID) {
		return
	}
	if updated, err := e.ReloadMessage(ctx, event.ConversationID, event); err == nil && updated != nil {
		e.handlers.onMessageUpdated(*updated)
	}
}
