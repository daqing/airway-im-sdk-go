package airwayim

// Conversation handles: one object per conversation with its own event
// subscriptions, so a chat window subscribes directly instead of
// filtering the global stream by conversation id. The kind lives on the
// concrete type (*DirectConversation / *GroupConversation), and the first
// OnMessage listener starts tracking the conversation automatically.
// Handle objects are cached per conversation id inside the session:
// opening the same conversation twice yields the same object.

import "context"

// Conversation is the per-conversation handle interface; *DirectConversation
// and *GroupConversation implement it. Handles are session-owned: only
// the session's open/create methods hand them out.
type Conversation interface {
	// ID is the conversation id.
	ID() string
	// Kind is ConversationKindDirect or ConversationKindGroup.
	Kind() ConversationKind

	// OnMessage subscribes to this conversation's messages: ordered,
	// deduplicated, gap-filled. The first listener starts tracking
	// automatically — it fetches everything since the last persisted
	// cursor (History semantics) and keeps the conversation in sync from
	// then on.
	OnMessage(handler func(ChatMessage)) *Subscription
	// OnMessageUpdated fires when a previously seen message in this
	// conversation was masked by moderation; content is already "***" —
	// re-render by replacing.
	OnMessageUpdated(handler func(ChatMessage)) *Subscription
	// OnMembersAdded fires on group conversations when members were added
	// (including the added users themselves).
	OnMembersAdded(handler func(MembersAddedInfo)) *Subscription
	// OnMembersRemoved fires on group conversations when a member was
	// removed (a kicked user is notified too).
	OnMembersRemoved(handler func(MembersRemovedInfo)) *Subscription

	// History loads the backlog: fetch messages since the last persisted
	// cursor (or opts.FromSequence) and emit them here and on the global
	// stream. Mostly redundant after OnMessage — that already auto-tracks
	// — but useful to await the backlog before rendering.
	History(ctx context.Context, opts HistoryOptions) ([]ChatMessage, error)
	// ListMessages returns a raw ordered message page after a sequence
	// (no state changes, no event emission).
	ListMessages(ctx context.Context, opts ListMessagesOptions) ([]ChatMessage, error)
	// Send sends a message to this conversation (same
	// idempotency/retry semantics as SendGroupMessage).
	Send(ctx context.Context, content string, opts *SendOptions) (ChatMessage, error)
	// Details returns the conversation kind plus members with roles,
	// fresh from the API.
	Details(ctx context.Context) (ConversationDetails, error)
	// LastSequence is the last synced sequence in this conversation.
	LastSequence() int64
	// Forget drops all sync state (e.g. after being kicked from the
	// group).
	Forget()

	// Internal dispatch from the hub; unexported so handles stay
	// session-owned.
	emitLocalMessage(message ChatMessage)
	emitLocalMessageUpdated(message ChatMessage)
	emitLocalMembersAdded(info MembersAddedInfo)
	emitLocalMembersRemoved(info MembersRemovedInfo)
}

type conversationCore struct {
	im   *Session
	id   string
	kind ConversationKind

	messageListeners        *listenerList[ChatMessage]
	messageUpdatedListeners *listenerList[ChatMessage]
	membersAddedListeners   *listenerList[MembersAddedInfo]
	membersRemovedListeners *listenerList[MembersRemovedInfo]
}

func newConversationCore(id string, kind ConversationKind, im *Session) *conversationCore {
	return &conversationCore{
		im:                      im,
		id:                      id,
		kind:                    kind,
		messageListeners:        newListenerList[ChatMessage](im.hub.onPanic),
		messageUpdatedListeners: newListenerList[ChatMessage](im.hub.onPanic),
		membersAddedListeners:   newListenerList[MembersAddedInfo](im.hub.onPanic),
		membersRemovedListeners: newListenerList[MembersRemovedInfo](im.hub.onPanic),
	}
}

func (c *conversationCore) ID() string             { return c.id }
func (c *conversationCore) Kind() ConversationKind { return c.kind }

func (c *conversationCore) OnMessage(handler func(ChatMessage)) *Subscription {
	subscription := c.messageListeners.add(handler)
	go c.im.ensureTracked(c.id)
	return subscription
}

func (c *conversationCore) OnMessageUpdated(handler func(ChatMessage)) *Subscription {
	return c.messageUpdatedListeners.add(handler)
}

func (c *conversationCore) OnMembersAdded(handler func(MembersAddedInfo)) *Subscription {
	return c.membersAddedListeners.add(handler)
}

func (c *conversationCore) OnMembersRemoved(handler func(MembersRemovedInfo)) *Subscription {
	return c.membersRemovedListeners.add(handler)
}

func (c *conversationCore) History(ctx context.Context, opts HistoryOptions) ([]ChatMessage, error) {
	return c.im.sync.FetchFrom(ctx, c.id, opts)
}

func (c *conversationCore) ListMessages(ctx context.Context, opts ListMessagesOptions) ([]ChatMessage, error) {
	return c.im.REST.ListMessages(ctx, c.id, opts)
}

func (c *conversationCore) Send(ctx context.Context, content string, opts *SendOptions) (ChatMessage, error) {
	return c.im.REST.SendMessage(ctx, c.id, content, opts)
}

func (c *conversationCore) Details(ctx context.Context) (ConversationDetails, error) {
	return c.im.REST.GetConversation(ctx, c.id)
}

func (c *conversationCore) LastSequence() int64 {
	return c.im.sync.LastSequence(c.id)
}

func (c *conversationCore) Forget() {
	c.im.ForgetConversation(c.id)
}

func (c *conversationCore) emitLocalMessage(message ChatMessage) {
	c.messageListeners.emit(message)
}

func (c *conversationCore) emitLocalMessageUpdated(message ChatMessage) {
	c.messageUpdatedListeners.emit(message)
}

func (c *conversationCore) emitLocalMembersAdded(info MembersAddedInfo) {
	c.membersAddedListeners.emit(info)
}

func (c *conversationCore) emitLocalMembersRemoved(info MembersRemovedInfo) {
	c.membersRemovedListeners.emit(info)
}

// DirectConversation is a direct (1:1) conversation; the peer of an
// incoming message is message.Sender.
type DirectConversation struct {
	*conversationCore
}

func newDirectConversation(id string, im *Session) *DirectConversation {
	return &DirectConversation{conversationCore: newConversationCore(id, ConversationKindDirect, im)}
}

// GroupConversation is a group conversation; carries the group-only
// operations.
type GroupConversation struct {
	*conversationCore

	// Title from creation time (nil when created without one or when
	// opened by id); Details returns the live value.
	Title *string
}

func newGroupConversation(id string, title *string, im *Session) *GroupConversation {
	return &GroupConversation{
		conversationCore: newConversationCore(id, ConversationKindGroup, im),
		Title:            title,
	}
}

// AddMembers adds members by uuid (owner/admin; idempotent for active
// members).
func (g *GroupConversation) AddMembers(ctx context.Context, memberUUIDs []string) (ConversationDetails, error) {
	return g.im.AddMembers(ctx, g.id, memberUUIDs)
}

// RemoveMembers removes members by uuid (owner/admin; cannot remove self
// or the owner).
func (g *GroupConversation) RemoveMembers(ctx context.Context, userUUIDs []string) (ConversationDetails, error) {
	return g.im.RemoveMembers(ctx, g.id, userUUIDs)
}
