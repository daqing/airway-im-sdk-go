package airwayim

// Client for the admin console API (/admin/api on the public listener).
// Authenticates with IM_ADMIN_USERNAME / IM_ADMIN_PASSWORD session tokens
// (12-hour in-memory sessions): the first authenticated call logs in
// transparently, and a 401 mid-session triggers one re-login and retry.
//
// This is an operational API, not a client API — never expose it publicly
// without network-level access control and TLS. Payloads are returned as
// JSONValue (the admin shapes are open-ended; see
// deps/im/docs/api/admin.md).

import (
	"context"
	"sync"
	"time"
)

// AdminOption customizes an AdminClient.
type AdminOption func(*AdminClient)

// WithAdminTimeout sets the per-request timeout (default 15s).
func WithAdminTimeout(timeout time.Duration) AdminOption {
	return func(c *AdminClient) { c.transport = NewHTTPTransport(timeout) }
}

// WithAdminTransport replaces the HTTP transport (tests, proxies).
func WithAdminTransport(transport HTTPTransport) AdminOption {
	return func(c *AdminClient) { c.transport = transport }
}

// AdminClient covers the admin console API with transparent session
// management.
type AdminClient struct {
	baseURL   string
	username  string
	password  string
	transport HTTPTransport

	mu    sync.Mutex
	token string
}

// NewAdminClient creates an admin client for the IM backend's public
// listener.
func NewAdminClient(apiURL, username, password string, opts ...AdminOption) *AdminClient {
	client := &AdminClient{
		baseURL:   trimTrailingSlashes(apiURL),
		username:  username,
		password:  password,
		transport: NewHTTPTransport(15 * time.Second),
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// Login exchanges admin credentials for a session token (also happens
// implicitly before the first authenticated call).
func (c *AdminClient) Login(ctx context.Context) (AdminSession, error) {
	body, err := marshalBody(struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{Username: c.username, Password: c.password})
	if err != nil {
		return AdminSession{}, imErr(err)
	}
	var data AdminSession
	if err := c.call(ctx, "POST", "/admin/api/login", body, false, &data); err != nil {
		return AdminSession{}, err
	}
	c.mu.Lock()
	c.token = data.Token
	c.mu.Unlock()
	return data, nil
}

// Logout revokes the current session.
func (c *AdminClient) Logout(ctx context.Context) error {
	var ignored JSONValue
	if err := c.call(ctx, "POST", "/admin/api/logout", nil, true, &ignored); err != nil {
		return err
	}
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	return nil
}

// Status returns aggregated status: user, outbox, gateway, and delivery
// metrics.
func (c *AdminClient) Status(ctx context.Context) (JSONValue, error) {
	var value JSONValue
	err := c.call(ctx, "GET", "/admin/api/status", nil, true, &value)
	return value, err
}

// Users lists registered identities, newest first, with token_version.
func (c *AdminClient) Users(ctx context.Context) ([]JSONValue, error) {
	var users []JSONValue
	err := c.call(ctx, "GET", "/admin/api/users", nil, true, &users)
	return users, err
}

// RevokeUser revokes a user's outstanding credentials (bumps
// token_version) and best-effort kicks their live gateway connections. An
// unknown uuid returns 404; revocation is not a ban.
func (c *AdminClient) RevokeUser(ctx context.Context, uuid string) (JSONValue, error) {
	var value JSONValue
	err := c.call(ctx, "POST", "/admin/api/users/"+escapeSegment(uuid)+"/revoke", nil, true, &value)
	return value, err
}

// GroupConversations lists all group conversations with member/message
// counts.
func (c *AdminClient) GroupConversations(ctx context.Context) ([]JSONValue, error) {
	var conversations []JSONValue
	err := c.call(ctx, "GET", "/admin/api/conversations", nil, true, &conversations)
	return conversations, err
}

// ConversationMessages lists all messages in a conversation, ascending
// sequence order.
func (c *AdminClient) ConversationMessages(ctx context.Context, conversationID string) ([]JSONValue, error) {
	var messages []JSONValue
	err := c.call(ctx, "GET",
		"/admin/api/conversations/"+escapeSegment(conversationID)+"/messages", nil, true, &messages)
	return messages, err
}

// MarkIllegal marks a message illegal (idempotent): masked to *** for
// clients, and a message.moderated event fans out to online members.
func (c *AdminClient) MarkIllegal(ctx context.Context, messageID string) (JSONValue, error) {
	var value JSONValue
	err := c.call(ctx, "POST", "/admin/api/messages/"+escapeSegment(messageID)+"/mark-illegal", nil, true, &value)
	return value, err
}

// ---- Plumbing ----

func (c *AdminClient) call(ctx context.Context, method, path string, body []byte, auth bool, out any) error {
	if auth {
		if err := c.ensureLoggedIn(ctx); err != nil {
			return err
		}
	}
	err := c.perform(ctx, method, path, body, auth, out)
	if imError, ok := err.(*IMError); ok && auth && imError.Status == 401 {
		// The 12-hour session token expired (or was lost server-side):
		// re-login once and retry a single time.
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		if err := c.ensureLoggedIn(ctx); err != nil {
			return err
		}
		return c.perform(ctx, method, path, body, auth, out)
	}
	return err
}

func (c *AdminClient) perform(ctx context.Context, method, path string, body []byte, auth bool, out any) error {
	headers := make(map[string]string, 2)
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	if auth {
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()
		headers["Authorization"] = "Bearer " + token
	}
	response, err := c.transport.Send(ctx, &HTTPRequest{
		URL:     buildURL(c.baseURL, path, nil),
		Method:  method,
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return imErr(err)
	}
	return unwrapEnvelope(response, out)
}

func (c *AdminClient) ensureLoggedIn(ctx context.Context) error {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		return nil
	}
	_, err := c.Login(ctx)
	return err
}
