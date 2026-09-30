# airway-im-sdk-go

The Go SDK for [Airway IM](https://github.com/daqing/airway-im-plugin): it
wraps the backend's REST API, the WebSocket gateway protocol, and the
server-to-server credential-minting endpoint into ready-to-use Go
interfaces, so Go backends and Go client applications never have to
implement credentials, the gateway first-frame authentication, heartbeats,
reconnect-with-backoff, or sequence-based catch-up sync themselves.

Built on the **Go standard library only** — zero third-party dependencies,
including the RFC 6455 WebSocket client. Requires Go 1.27+. Versioned in
lockstep with the other Airway IM SDKs (currently 0.7.0); the public API
mirrors `airway-im-sdk-ts` and `airway-im-sdk-swift` one to one.

## Features

- **Full REST coverage** — profile, conversations (direct get-or-create /
  groups / member management), messages (history, send, idempotent retry),
  and file upload, all strongly typed with automatic unwrapping of the
  `{code, data, message}` envelope.
- **Conversation handles** — `CreateDirect` / `GetDirect` / `CreateGroup` /
  `OpenConversation` return per-conversation objects
  (`*DirectConversation` / `*GroupConversation`) with scoped `OnMessage`
  events and `Send` / `History`; the kind lives on the type, and the first
  message listener starts tracking automatically.
- **Realtime gateway** — automatically performs the `{"cmd":"auth"}`
  first-frame authentication, application-level heartbeat (25 s by
  default), exponential backoff reconnection (0.5 s → 10 s), and
  deduplication by `event_id`.
- **Sync engine** — maintains a per-conversation `sequence` cursor: on a
  `message.created` event it fills gaps via `after_sequence`, so messages
  that arrive out of order, duplicated, or while offline are eventually
  delivered through the `OnMessage` event **in order, without duplicates
  or loss**. Cursors can be persisted through a `SequenceStore` of your
  choice so a restart only fetches the delta.
- **Credential renewal** — when a credential expires (HTTP 401/10001 or a
  gateway auth failure), the SDK automatically calls your
  `WithGetCredential` callback for a fresh one and recovers; business code
  is unaffected.
- **Server-side credential minting** — a Go backend obtains credentials
  through `InternalClient` (server-to-server, private network; never link
  it into client code) instead of raw HTTP calls.
- **Admin operations** — `AdminClient` covers the admin console API
  (status, users, revocation, message review, moderation) with transparent
  session management.
- **Credential helpers** — `SignCredential` / `DecodeCredential` /
  `VerifyCredentialSignature` / `IsCredentialExpired` for the Airway
  project's own side.

## Installation

The package lives in this repository under `install/ignore/sdk/go/`
(module `github.com/daqing/airway-im-sdk-go`); releases are tagged in
lockstep with the other Airway IM SDKs (currently 0.7.0). Add it to your
`go.mod` with a `replace` directive during development, or point
`go get` at the release repository once published:

```bash
go get github.com/daqing/airway-im-sdk-go
```

## Quick start

### 1. Obtain and deliver a credential from your own backend

The SDK contains no login logic. The user first logs in on your platform
(password, SMS code, …); once login succeeds, **your platform's own server
backend** reads `(uuid, name)` from its own user table and obtains the
credential **server-to-server** by calling the internal minting endpoint
(protected by `IM_INTERNAL_SECRET`), returning the finished credential
together with your own login response. A Go backend does it with
`InternalClient` (server side only, never link it into client code):

```go
internal := airwayim.NewInternalClient(
    "http://127.0.0.1:1906", // internal listener, private network only
    os.Getenv("IM_INTERNAL_SECRET"))

minted, err := internal.MintCredential(ctx, airwayim.MintOptions{
    UUID:     user.UUID,      // from your own user table
    Name:     user.Username,
    Nickname: user.DisplayName, // optional; written only when non-empty
})
// minted.Credential — "im1.…" — hand to the client with your login response
// minted.ExpiresAt  — RFC 3339 UTC, when to re-mint; empty for service credentials
```

Backends in other languages call the same endpoint over plain HTTP. The
signing secret `IM_AUTH_SECRET` lives only on the IM server side of the
Airway project; your backend only needs `IM_INTERNAL_SECRET`. The client
holds no secret, only the finished credential (REST:
`Authorization: Bearer`; WebSocket: the first `auth` command).

**Prerequisite: your platform must have its own server backend.** A pure
client app without a server cannot integrate securely — the client has no
secure way to obtain a credential. The full trust model is documented in
[`deps/im/docs/design/identity.md`](../../../deps/im/docs/design/identity.md).

### 2. Exchange messages in a Go application

```go
im := airwayim.New(
    "https://im.example.com", // IM backend (:1905), https in production
    "wss://im.example.com",   // WebSocket gateway (:1910); the SDK connects to <wsURL>/ws
    storedCredential,
    airwayim.WithGetCredential(fetchNewCredentialFromYourBackend), // called automatically when invalid
)
defer im.Close()

im.OnStatus(func(status airwayim.ConnectionStatus) { log.Println("connection:", status) })
im.Connect()

// Direct chat: the kind lives on the type
direct, err := im.CreateDirect(ctx, otherUserUUID) // get-or-create
direct.OnMessage(func(message airwayim.ChatMessage) {
    // ordered, deduplicated, gap-filled (including messages missed while
    // offline); the first listener starts tracking automatically
    log.Println(message.Sender.Username, message.Content)
})
backlog, err := direct.History(ctx, airwayim.HistoryOptions{}) // backlog so far, ascending sequence

// Sending (the SDK generates the Idempotency-Key automatically and retries
// network failures with the same key, so a message is never duplicated)
_, err = direct.Send(ctx, "hello", &airwayim.SendOptions{ContentType: airwayim.ContentTypePlain})

// Group chat: the same model
group, err := im.CreateGroup(ctx, ptr("Team"), []string{otherUserUUID})
group.OnMessage(func(message airwayim.ChatMessage) { log.Println(message.Content) })
_, err = group.Send(ctx, "hello", nil)
_, err = group.AddMembers(ctx, []string{anotherUserUUID})
```

### 3. Long-running processes

`Connect` is idempotent — call it again whenever the process regains
network access; the SDK reconnects and catches up on sync automatically.
All methods are safe for concurrent use, so share one `Session` across
your whole application. `Close` disconnects the gateway and stops the
event pump when the session is no longer needed.

## API reference

### `airwayim.New` options

| Parameter | Required | Default | Description |
| --- | --- | --- | --- |
| `apiURL` | ✓ | — | IM backend URL (:1905); https in production |
| `wsURL` | | — | Gateway base URL (`:1910`) — pass it **without** a path; the SDK appends `/ws` itself. `""` for REST-only use |
| `credential` | ✓ | — | User credential issued by your backend |
| `WithGetCredential` | | — | Fetches a fresh credential when the current one is rejected (HTTP 401/10001 or gateway auth failure); one renewal + one retry per request |
| `WithTimeout` | | `15s` | REST request timeout |
| `WithPingInterval` | | `25s` | Application-level heartbeat interval, `0` disables |
| `WithPersistSequences` | | `true` | Persist per-conversation sequence cursors |
| `WithSequenceStore` | | in-process | Where cursors persist (`SequenceStore` interface for file/database/redis backends) |
| `WithHTTPTransport` / `WithSocketFactory` | | net/http / RFC 6455 | Custom transport seams for tests and proxies |
| `WithAutoConnect` | | `false` | Connect to the gateway immediately after creation |

### REST methods (`im.*`)

| Method | Endpoint |
| --- | --- |
| `im.Me(ctx)` | `GET /api/v1/me` |
| `im.ListGroups(ctx)` | `GET /api/v1/conversations?type=group` |
| `im.CreateDirect(ctx, otherUserUUID)` | Direct get-or-create; returns the `*DirectConversation` handle |
| `im.GetDirect(ctx, otherUserUUID)` | The existing direct handle (nil when none) |
| `im.CreateGroup(ctx, title, memberUUIDs)` | `POST /api/v1/group`; returns the `*GroupConversation` handle |
| `im.OpenConversation(ctx, id)` | Open any conversation by id as a handle (kind from the registry, else one REST fetch) |
| `im.AddMembers(ctx, conversationID, memberUUIDs)` | `POST .../members` |
| `im.RemoveMembers(ctx, conversationID, userUUIDs)` | `DELETE .../members/:user_uuid` |
| `im.History(ctx, conversationID, opts)` | Pull history + start tracking sync |
| `im.ListMessages(ctx, conversationID, opts)` | `GET .../messages` (raw paging) |
| `im.SendGroupMessage(ctx, conversationID, content, opts)` | `POST /api/v1/messages` |
| `im.SendDirectMessage(ctx, otherUserUUID, content, opts)` | Get-or-create the direct conversation, then `POST /api/v1/messages` |
| `im.UploadFile(ctx, input)` / `im.UploadFileFromPath(ctx, path, dir)` | `POST /api/v1/storage` (multipart) |
| `im.StorageURL(key)` | File download URL |

Send options: `ContentType` (`ContentTypeMarkdown` default /
`ContentTypePlain`), `IdempotencyKey` (auto-generated by default),
`Retries` (network-failure retries, default 1).

The same methods are available directly on `im.REST` (a `*RESTClient`)
for REST-only integrations without a session.

### Conversation handles (`*DirectConversation` / `*GroupConversation`)

Handles are per-conversation objects — the kind lives on the type, and
the same conversation always yields the same handle. Anything emitted on
a handle also appears on the facade's global stream (below), and vice
versa.

| Member | Description |
| --- | --- |
| `ID()` / `Kind()` | Conversation id; `direct` or `group` |
| `OnMessage(…)` / `OnMessageUpdated(…)` | Ordered message events; groups also fire `OnMembersAdded` / `OnMembersRemoved`. The first `OnMessage` listener starts tracking automatically (history since the last persisted cursor, then realtime). Every registration returns a `*Subscription` with `Cancel()` |
| `History(ctx, opts)` | Await the backlog; emits each message via `OnMessage` |
| `Send(ctx, content, opts)` | Send into this conversation (same idempotency/retry semantics as `SendGroupMessage`) |
| `ListMessages(ctx, opts)` | Raw ordered page, no state changes |
| `LastSequence()` / `Forget()` | Sync cursor; drop all state for this conversation |
| `Details(ctx)` | Conversation kind + members with roles, fresh from the API |
| group only: `Title` | Title from creation time (nil when opened by id) |
| group only: `AddMembers(…)` / `RemoveMembers(…)` | Member management; returns members with roles |

### Global stream (`im.On…`)

Conversation handles are the per-window API; the facade also exposes a
global stream, handy for unread badges or a unified inbox:

| Registration | Payload | Description |
| --- | --- | --- |
| `OnMessage` | `ChatMessage` | New message, ordered, deduplicated, gap-filled |
| `OnMessageUpdated` | `ChatMessage` | Message masked by moderation; content is already `***`; re-render by replacing |
| `OnMembersAdded` | `MembersAddedInfo` | Group members added (including the added users themselves) |
| `OnMembersRemoved` | `MembersRemovedInfo` | Group member removed (a kicked user is notified too) |
| `OnStatus` | `ConnectionStatus` | `connecting / authenticating / online / reconnecting / offline / closed` |
| `OnError` | `*IMError` | Gateway auth failure, credential renewal failure, etc. |
| `OnEvent` | Raw `GatewayEvent` | Every gateway frame; messages of conversations not yet tracked also arrive here |

Connection control: `im.Connect()` (idempotent) / `im.Disconnect()` /
`im.IsOnline()` / `im.ConnectionStatus()` / `im.SetCredential(_)` /
`im.LastSequence(conversationID)` / `im.ForgetConversation(conversationID)`
(e.g. drop the sync cursor after being kicked from a group).

### Message model (`ChatMessage`)

`Send`, `SendDirectMessage`, `ListMessages`, `History`, and realtime
`OnMessage` events all carry the same `ChatMessage`:

| Field | Type | Description |
| --- | --- | --- |
| `ID` | string, 26-char ULID | Server-generated, globally unique message identifier; the stable key for deduplication. |
| `ConversationID` | string, 26-char ULID | Conversation the message belongs to; route messages to their chat window by it. |
| `Sender.UUID` | string | Author's stable identity uuid (identity is uuid-only across the API and events). |
| `Sender.Username` | string | Host-assigned account handle, stable for the account's lifetime. |
| `Sender.Nickname` | string? | Preferred display name; fall back to `Username` when empty. |
| `Sender.AvatarURL` | string? | Avatar image URL; render a placeholder when empty. |
| `Content` | string | Message body, 1–32768 UTF-8 encoded bytes, CRLF normalized to LF by the server. A moderated message reads back as the literal `***`. |
| `ContentType` | string | `text/markdown` (default) or `text/plain` — how to render `Content`. |
| `CreatedAt` | string, RFC 3339 UTC | Server commit timestamp; convert to the viewer's local time zone for display. |
| `Sequence` | integer ≥ 1 | Position within the conversation, allocated at commit. `(ConversationID, Sequence)` is the total order to sort by. |

Sort by `Sequence`, never by `CreatedAt`. A retried send with the same
idempotency key returns the original message (same `ID` and `Sequence`);
the SDK already deduplicates and gap-fills realtime events for you.
`Sender` reflects the author's current profile, not a send-time snapshot.

### `InternalClient` (internal minting endpoint, default `127.0.0.1:1906`)

| Method | Description |
| --- | --- |
| `MintCredential(ctx, MintOptions)` | Server-to-server credential minting; `TTLSeconds` nil applies the backend default (86400, capped at 2592000), `0` omits expiry; returns `MintedCredential` (`Credential`, `ExpiresAt`) |

This type is for your backend only — a client app must never call it:
whoever can mint credentials can impersonate any user. Error code `10005`
here means the `X-IM-Internal-Secret` header is missing or wrong (not the
public API's permission denied); `10006` means the deployment has no
`IM_AUTH_SECRET` configured and cannot sign.

### `AdminClient` (admin console, `/admin/api`)

Logs in automatically on the first call (12-hour session); a mid-session
401 triggers one re-login and retry. Responses are `JSONValue` (the admin
shapes are open-ended; see
[`deps/im/docs/api/admin.md`](../../../deps/im/docs/api/admin.md)).

| Method | Description |
| --- | --- |
| `Login` / `Logout` | Explicit login / logout |
| `Status` | Aggregated status: users, outbox, gateway/delivery metrics |
| `Users` | Registered users (with `token_version`) |
| `RevokeUser(uuid)` | Revoke all of a user's credentials and kick their connections |
| `GroupConversations` | All group conversations (with member/message counts) |
| `ConversationMessages(id)` | Message review (ascending) |
| `MarkIllegal(messageID)` | Mark illegal (idempotent): content masked to `***` for clients |

### `SignCredential` (signing and verification helpers)

Local signing is for trusted holders of `IM_AUTH_SECRET` on the Airway
project's own side; third-party platform backends never hold that secret
and must use `InternalClient` instead.

| Function | Description |
| --- | --- |
| `SignCredential(secret, uuid, name, opts)` | Issues an `im1.<payload>.<sig>` HMAC-SHA256 credential; optional fields are written only when present, never clobbering an existing profile. `TTLSeconds`: 0 applies the 24 h default, negative omits `exp` (service credentials only) |
| `DecodeCredential(credential)` | Returns the `CredentialClaims` (error on malformed input) |
| `VerifyCredentialSignature(credential, secret)` | Constant-time verification, returns `false` on malformed input |
| `IsCredentialExpired(credential, now)` | Whether `exp` has passed; credentials without `exp` never expire |

### Error handling

All REST errors are `*IMError` (`Code` — the envelope business code,
`Status` — the HTTP status code, `Status == 0` on transport failure).
Common checks:

```go
message, err := im.SendGroupMessage(ctx, id, "hi", nil)
var imError *airwayim.IMError
if errors.As(err, &imError) {
    if imError.IsAuthError() { /* 10001: credential invalid/expired — wait for auto-renewal or re-login */ }
    else if imError.Code == airwayim.CodePermissionDenied { /* 10005: not a member */ }
    else if imError.Code == airwayim.CodeConversationNotFound { /* 11001 */ }
}
```

Full error codes: `10000` internal error, `10001` invalid credential,
`10003` invalid request, `10005` permission denied, `11001` conversation
not found, `11002` idempotency key reused with a different request.

## Message reliability model

The backend guarantees a monotonically increasing per-conversation
`sequence`; delivery is **at-least-once**. The SDK's sync engine takes
care of deduplication by `event_id` / `message_id`, ordering by
`sequence`, and gap-filling via `after_sequence`. Business code only needs
to:

1. open the conversation — a handle's first `OnMessage` (or `History`)
   starts tracking;
2. append-render in the `OnMessage` event (handle repeated messages
   idempotently by `message.ID`);
3. render sends optimistically from `Send`'s return value and dedupe the
   realtime echo by `ID`.

Conversations never tracked do not auto-fetch messages (avoids flushing
the whole history); handle them via `OnEvent` for unread badges and
similar cases.

## Concurrency

Every `Session`, `RESTClient`, `InternalClient`, and `AdminClient` method
is safe for concurrent use; share one instance across your whole
application. Event handlers run on their own goroutines — hop to your UI
thread yourself before touching UI. `Subscription.Cancel()` unsubscribes;
keep the returned subscription for as long as the listener should fire.

## Local development and verification

```bash
go test -race ./...
# 58 tests: signing vectors (byte-for-byte against the Node reference),
# envelope parsing, idempotent retry, credential renewal, sync engine,
# gateway reconnect/auth flows, and a hand-rolled RFC 6455 server —
# standard library only, no external services.
```

For end-to-end verification against a real stack (backend :1905 / gateway
:1910 / internal :1906, see the repository root README): start the stack
and follow "Quick start" step by step.

## Protocol reference

- API contract: [`deps/im/docs/api/openapi.md`](../../../deps/im/docs/api/openapi.md)
- Gateway protocol: [`deps/im/docs/design/gateway.md`](../../../deps/im/docs/design/gateway.md)
- Credential issuance: [`deps/im/docs/design/identity.md`](../../../deps/im/docs/design/identity.md)
- Admin console: [`deps/im/docs/api/admin.md`](../../../deps/im/docs/api/admin.md)

---

中文版本：[README.zh-CN.md](README.zh-CN.md)
