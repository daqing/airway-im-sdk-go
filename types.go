package airwayim

// ConversationKind is how a conversation id routes in the UI.
type ConversationKind string

const (
	ConversationKindDirect ConversationKind = "direct"
	ConversationKindGroup  ConversationKind = "group"
)

// MemberRole is an open string enum: the two known roles ship as
// constants, unknown values still decode.
type MemberRole string

const (
	MemberRoleOwner  MemberRole = "owner"
	MemberRoleAdmin  MemberRole = "admin"
	MemberRoleMember MemberRole = "member"
)

// ContentType is how to render message content. ContentTypeMarkdown is
// the send default.
type ContentType string

const (
	ContentTypeMarkdown ContentType = "text/markdown"
	ContentTypePlain    ContentType = "text/plain"
)

// ConnectionStatus is the gateway connection lifecycle.
type ConnectionStatus string

const (
	StatusConnecting     ConnectionStatus = "connecting"
	StatusAuthenticating ConnectionStatus = "authenticating"
	StatusOnline         ConnectionStatus = "online"
	StatusReconnecting   ConnectionStatus = "reconnecting"
	StatusOffline        ConnectionStatus = "offline"
	StatusClosed         ConnectionStatus = "closed"
)

// Well-known gateway event names (the wire format is open; unknown
// strings may appear and are surfaced via OnEvent).
const (
	EventMessageCreated   = "message.created"
	EventMessageModerated = "message.moderated"
	EventMemberAdded      = "conversation.member_added"
	EventMemberRemoved    = "conversation.member_removed"
)

// User is the authenticated (or listed) user profile.
type User struct {
	UUID       string  `json:"uuid"`
	Username   string  `json:"username"`
	Nickname   *string `json:"nickname"`
	AvatarURL  *string `json:"avatar_url"`
	Email      *string `json:"email"`
	LastSeenAt *string `json:"last_seen_at"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

// Sender is a message author profile (current, not a send-time snapshot).
type Sender struct {
	UUID      string  `json:"uuid"`
	Username  string  `json:"username"`
	Nickname  *string `json:"nickname"`
	AvatarURL *string `json:"avatar_url"`
}

// ConversationSummary is a conversation as returned by list/create calls.
type ConversationSummary struct {
	ID        string           `json:"id"`
	Kind      ConversationKind `json:"kind"`
	Title     *string          `json:"title"`
	AvatarURL *string          `json:"avatar_url"`
	CreatedBy string           `json:"created_by"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
}

// ConversationMember is an active member with its role.
type ConversationMember struct {
	UUID      string     `json:"uuid"`
	Username  string     `json:"username"`
	Nickname  *string    `json:"nickname"`
	AvatarURL *string    `json:"avatar_url"`
	Role      MemberRole `json:"role"`
}

// ConversationDetails is the conversation kind plus active members with
// roles, as returned by the details endpoint. ConversationUUID echoes the
// requested conversation id.
type ConversationDetails struct {
	ConversationUUID string               `json:"conversation_uuid"`
	Type             ConversationKind     `json:"type"`
	Title            *string              `json:"title"`
	Members          []ConversationMember `json:"members"`
}

// ChatMessage is a message as returned by sends, history, and realtime
// events. Sort by Sequence, never by CreatedAt; dedupe by ID.
type ChatMessage struct {
	// ID is the server-generated, globally unique ULID; the stable dedupe
	// key.
	ID string `json:"id"`
	// ConversationID routes the message to its chat window.
	ConversationID string `json:"conversation_id"`
	// Sender is the author profile.
	Sender Sender `json:"sender"`
	// Content is the body, 1-32768 UTF-8 bytes, CRLF normalized to LF; a
	// moderated message reads back as the literal "***".
	Content string `json:"content"`
	// ContentType is how to render Content.
	ContentType ContentType `json:"content_type"`
	// CreatedAt is an RFC 3339 UTC server commit timestamp.
	CreatedAt string `json:"created_at"`
	// Sequence is the position within the conversation, >= 1;
	// (ConversationID, Sequence) is the total order.
	Sequence int64 `json:"sequence"`
}

// UploadResult is the response of a file upload.
type UploadResult struct {
	Key  string `json:"key"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// UploadInput carries the multipart fields for a file upload. Dir is
// optional (empty omits the form field); Filename defaults to "file" when
// empty.
type UploadInput struct {
	Filename string
	Content  []byte
	Dir      string
}

// GatewayTargets is the fan-out target set of an event frame.
type GatewayTargets struct {
	UserUUIDs []string `json:"user_uuids"`
}

// GatewayEvent is a pushed gateway event frame (deps/im/docs/api/openapi.md,
// "WebSocket Gateway companion contract"). Delivery is at-least-once;
// dedupe by EventID (the SDK's gateway client already does). Zero-value
// optional fields (MessageID, Sequence 0, empty slices) mean absent.
type GatewayEvent struct {
	EventID         string         `json:"event_id"`
	Event           string         `json:"event"`
	MessageID       string         `json:"message_id"`
	ConversationID  string         `json:"conversation_id"`
	Sequence        int64          `json:"sequence"`
	AddedUserUUIDs  []string       `json:"added_user_uuids"`
	RemovedUserUUID string         `json:"removed_user_uuid"`
	Targets         GatewayTargets `json:"targets"`
}

// MembersAddedInfo is the payload of the members-added notification.
type MembersAddedInfo struct {
	ConversationID string       `json:"conversation_id"`
	AddedUserUUIDs []string     `json:"added_user_uuids"`
	Event          GatewayEvent `json:"event"`
}

// MembersRemovedInfo is the payload of the members-removed notification
// (a kicked user is notified too).
type MembersRemovedInfo struct {
	ConversationID  string       `json:"conversation_id"`
	RemovedUserUUID string       `json:"removed_user_uuid"`
	Event           GatewayEvent `json:"event"`
}

// mintData is the internal minting endpoint's data payload.
type mintData struct {
	Credential string  `json:"credential"`
	ExpiresAt  *string `json:"expires_at"`
}

// MintedCredential is a freshly minted credential.
type MintedCredential struct {
	// Credential is the finished im1.<payload>.<sig> token: hand it to the
	// client with your login response and pass it to New / SetCredential.
	Credential string
	// ExpiresAt is an RFC 3339 UTC timestamp; empty when TTLSeconds was 0
	// (no expiry claim).
	ExpiresAt string
}

// AdminSession is the admin login payload.
type AdminSession struct {
	Token     string  `json:"token"`
	ExpiresAt *string `json:"expires_at"`
	Username  *string `json:"username"`
}

// marshalBody JSON-encodes a request body without HTML escaping.
func marshalBody(v any) ([]byte, error) {
	return marshalJSONNoEscape(v)
}
