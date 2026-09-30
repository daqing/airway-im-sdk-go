package airwayim

// REST client for the Airway IM plugin public API (default port :1905).
// Every request carries Authorization: Bearer <host-signed credential>;
// responses are unwrapped from the {code, data, message} envelope and
// decoded into typed values. Errors are *IMError.
//
// A RESTClient is safe for concurrent use; a successful credential
// renewal is a one-shot overwrite.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ListMessagesOptions carries the optional ListMessages inputs. Zero
// values mean "not set": the backend then starts from the newest and
// applies its default page size (100).
type ListMessagesOptions struct {
	// AfterSequence returns messages with sequence > AfterSequence.
	AfterSequence int64
	// Limit is the page size (1-200; the backend falls back to 100
	// outside that range).
	Limit int
}

// EachMessageOptions carries the optional EachMessage inputs.
type EachMessageOptions struct {
	// AfterSequence starts paging after this sequence (default 0).
	AfterSequence int64
	// PageSize is the page size (capped at 200; default 100).
	PageSize int
}

// SendOptions carries the optional SendMessage inputs.
type SendOptions struct {
	// ContentType is how to render content; empty applies the default
	// ContentTypeMarkdown.
	ContentType ContentType
	// IdempotencyKey controls the dedupe key (1-128 chars, reuse only for
	// the same logical request); empty generates a random key.
	IdempotencyKey string
	// Retries is the number of automatic retries with the same
	// idempotency key on network failures; 0 applies the default 1.
	Retries int
}

type RESTClient struct {
	baseURL   string
	transport HTTPTransport

	mu            sync.Mutex
	credential    string
	getCredential CredentialProvider
}

func newRESTClient(baseURL, credential string, getCredential CredentialProvider, transport HTTPTransport) *RESTClient {
	return &RESTClient{
		baseURL:       trimTrailingSlashes(baseURL),
		credential:    credential,
		getCredential: getCredential,
		transport:     transport,
	}
}

// SetCredential replaces the bearer credential (e.g. after a re-login).
func (r *RESTClient) SetCredential(credential string) {
	r.mu.Lock()
	r.credential = credential
	r.mu.Unlock()
}

// BaseURL returns the IM backend base URL.
func (r *RESTClient) BaseURL() string {
	return r.baseURL
}

// ---- Identity ----

// Me returns the authenticated user profile.
func (r *RESTClient) Me(ctx context.Context) (User, error) {
	var user User
	err := r.request(ctx, "GET", "/api/v1/me", requestOptions{}, &user)
	return user, err
}

// ---- Conversations ----

// ListGroups lists my active group conversations (direct ones are
// excluded by the backend).
func (r *RESTClient) ListGroups(ctx context.Context) ([]ConversationSummary, error) {
	var conversations []ConversationSummary
	err := r.request(ctx, "GET", "/api/v1/conversations", requestOptions{
		query: []queryPair{{name: "type", value: "group"}},
	}, &conversations)
	return conversations, err
}

// CreateDirect get-or-creates the direct conversation with one other
// user, identified by their uuid.
func (r *RESTClient) CreateDirect(ctx context.Context, otherUserUUID string) (ConversationSummary, error) {
	body, err := marshalBody(directBody{
		Kind:        "direct",
		MemberUUIDs: []string{otherUserUUID},
	})
	if err != nil {
		return ConversationSummary{}, imErr(err)
	}
	var conversation ConversationSummary
	err = r.request(ctx, "POST", "/api/v1/conversations", requestOptions{body: body}, &conversation)
	return conversation, err
}

// GetDirect returns the direct conversation with one other user by uuid,
// or nil when none exists yet (read-only; CreateDirect get-or-creates
// instead).
func (r *RESTClient) GetDirect(ctx context.Context, otherUserUUID string) (*ConversationSummary, error) {
	var conversation *ConversationSummary
	err := r.request(ctx, "GET", "/api/v1/conversations/direct/"+escapeSegment(otherUserUUID), requestOptions{}, &conversation)
	if err != nil {
		if imError, ok := err.(*IMError); ok && imError.Code == CodeConversationNotFound {
			return nil, nil
		}
		return nil, err
	}
	return conversation, nil
}

// CreateGroup creates a new group; the authenticated user becomes its
// owner. A nil title creates the group without one.
func (r *RESTClient) CreateGroup(ctx context.Context, title *string, memberUUIDs []string) (ConversationSummary, error) {
	body, err := marshalBody(groupBody{Title: title, MemberUUIDs: memberUUIDs})
	if err != nil {
		return ConversationSummary{}, imErr(err)
	}
	var conversation ConversationSummary
	err = r.request(ctx, "POST", "/api/v1/group", requestOptions{body: body}, &conversation)
	return conversation, err
}

// GetConversation returns the conversation kind plus active members with
// roles.
func (r *RESTClient) GetConversation(ctx context.Context, conversationID string) (ConversationDetails, error) {
	var details ConversationDetails
	err := r.request(ctx, "GET", "/api/v1/conversations/"+escapeSegment(conversationID), requestOptions{}, &details)
	return details, err
}

// AddMembers adds members (by uuid) to a group (owner/admin; idempotent
// for already-active members).
func (r *RESTClient) AddMembers(ctx context.Context, conversationID string, memberUUIDs []string) (ConversationDetails, error) {
	body, err := marshalBody(membersBody{MemberUUIDs: memberUUIDs})
	if err != nil {
		return ConversationDetails{}, imErr(err)
	}
	var details ConversationDetails
	err = r.request(ctx, "POST", "/api/v1/conversations/"+escapeSegment(conversationID)+"/members", requestOptions{body: body}, &details)
	return details, err
}

// RemoveMembers removes members (by uuid) from a group (owner/admin;
// cannot remove self or the owner). Idempotent for members who are not
// active. Returns the details after the last removal (the current
// details for an empty list).
func (r *RESTClient) RemoveMembers(ctx context.Context, conversationID string, userUUIDs []string) (ConversationDetails, error) {
	if len(userUUIDs) == 0 {
		return r.GetConversation(ctx, conversationID)
	}
	var details ConversationDetails
	for _, uuid := range userUUIDs {
		var current ConversationDetails
		err := r.request(ctx, "DELETE",
			"/api/v1/conversations/"+escapeSegment(conversationID)+"/members/"+escapeSegment(uuid),
			requestOptions{}, &current)
		if err != nil {
			return ConversationDetails{}, err
		}
		details = current
	}
	return details, nil
}

// ---- Messages ----

// ListMessages returns an ordered message page after a sequence (limit
// 1-200; the backend falls back to 100 outside that range). Use for
// history display and reconnect synchronization.
func (r *RESTClient) ListMessages(ctx context.Context, conversationID string, opts ListMessagesOptions) ([]ChatMessage, error) {
	query := []queryPair{
		{name: "after_sequence", value: formatInt(opts.AfterSequence)},
		{name: "limit", value: formatInt(int64(opts.Limit))},
	}
	var messages []ChatMessage
	err := r.request(ctx, "GET",
		"/api/v1/conversations/"+escapeSegment(conversationID)+"/messages",
		requestOptions{query: query}, &messages)
	return messages, err
}

// EachMessage auto-pages over the conversation history in ascending
// sequence order, calling handler for every message; it stops when a page
// comes back short or empty.
func (r *RESTClient) EachMessage(ctx context.Context, conversationID string, opts EachMessageOptions, handler func(ChatMessage)) error {
	size := opts.PageSize
	if size <= 0 {
		size = 100
	}
	size = min(size, 200)
	cursor := opts.AfterSequence
	for {
		page, err := r.ListMessages(ctx, conversationID, ListMessagesOptions{AfterSequence: cursor, Limit: size})
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, message := range page {
			handler(message)
		}
		cursor = page[len(page)-1].Sequence
		if len(page) < size {
			return nil
		}
	}
}

// SendMessage sends a message by conversation id. A random Idempotency-Key
// is generated per call and reused across network-failure retries, so
// retry storms can never duplicate a message; pass opts.IdempotencyKey to
// control it.
func (r *RESTClient) SendMessage(ctx context.Context, conversationID, content string, opts *SendOptions) (ChatMessage, error) {
	options := SendOptions{}
	if opts != nil {
		options = *opts
	}
	if options.ContentType == "" {
		options.ContentType = ContentTypeMarkdown
	}
	if options.IdempotencyKey == "" {
		options.IdempotencyKey = randomID()
	}
	if options.Retries <= 0 {
		options.Retries = 1
	}
	body, err := marshalBody(sendBody{
		ConversationID: conversationID,
		Content:        content,
		ContentType:    options.ContentType,
	})
	if err != nil {
		return ChatMessage{}, imErr(err)
	}
	var attempt int
	for {
		var message ChatMessage
		err = r.request(ctx, "POST", "/api/v1/messages", requestOptions{
			body:    body,
			headers: map[string]string{"Idempotency-Key": options.IdempotencyKey},
		}, &message)
		if err == nil {
			return message, nil
		}
		attempt++
		// Retry transport failures (status 0) only; HTTP errors are final
		// and the backend dedupes by the reused key anyway.
		if imError, ok := err.(*IMError); !ok || imError.Status != 0 || attempt > options.Retries {
			return ChatMessage{}, err
		}
	}
}

// SendDirectMessage sends a direct message to one other user, identified
// by their uuid: get-or-create the direct conversation, then send. Same
// idempotency semantics as SendMessage.
func (r *RESTClient) SendDirectMessage(ctx context.Context, otherUserUUID, content string, opts *SendOptions) (ChatMessage, error) {
	conversation, err := r.CreateDirect(ctx, otherUserUUID)
	if err != nil {
		return ChatMessage{}, err
	}
	return r.SendMessage(ctx, conversation.ID, content, opts)
}

// ---- Storage ----

// UploadFile uploads a file (development-stage API: currently no auth
// middleware) and returns its key, URL, and size.
func (r *RESTClient) UploadFile(ctx context.Context, input UploadInput) (UploadResult, error) {
	filename := input.Filename
	if filename == "" {
		filename = "file"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition",
		fmt.Sprintf("form-data; name=\"file\"; filename=\"%s\"", sanitizeFilename(filename)))
	header.Set("Content-Type", mimeTypeFor(filename))
	part, err := writer.CreatePart(header)
	if err != nil {
		return UploadResult{}, imErr(err)
	}
	if _, err := part.Write(input.Content); err != nil {
		return UploadResult{}, imErr(err)
	}
	if input.Dir != "" {
		if err := writer.WriteField("dir", input.Dir); err != nil {
			return UploadResult{}, imErr(err)
		}
	}
	if err := writer.Close(); err != nil {
		return UploadResult{}, imErr(err)
	}

	response, err := r.transport.Send(ctx, &HTTPRequest{
		URL:    joinURL(r.baseURL, "/api/v1/storage"),
		Method: "POST",
		Headers: map[string]string{
			"Content-Type": writer.FormDataContentType(),
		},
		Body: body.Bytes(),
	})
	if err != nil {
		return UploadResult{}, imErr(err)
	}
	if response.Status >= 200 && response.Status < 300 {
		var result UploadResult
		if err := json.Unmarshal(response.Body, &result); err == nil {
			return result, nil
		}
	}
	var failure struct {
		Error string `json:"error"`
	}
	message := fmt.Sprintf("HTTP %d: upload failed", response.Status)
	if err := json.Unmarshal(response.Body, &failure); err == nil && failure.Error != "" {
		message = failure.Error
	}
	return UploadResult{}, &IMError{Code: -1, Status: response.Status, Message: message}
}

// StorageURL returns the public download URL for a storage key.
func (r *RESTClient) StorageURL(key string) string {
	return joinURL(r.baseURL, "/api/v1/storage/"+escapeSegments(key))
}

// ---- Plumbing ----

type directBody struct {
	Kind        string   `json:"kind"`
	MemberUUIDs []string `json:"member_uuids"`
}

type groupBody struct {
	Title       *string  `json:"title"`
	MemberUUIDs []string `json:"member_uuids"`
}

type membersBody struct {
	MemberUUIDs []string `json:"member_uuids"`
}

type sendBody struct {
	ConversationID string      `json:"conversation_id"`
	Content        string      `json:"content"`
	ContentType    ContentType `json:"content_type"`
}

type requestOptions struct {
	query   []queryPair
	body    []byte
	headers map[string]string
	noAuth  bool // opt out of bearer authentication
}

func (r *RESTClient) request(ctx context.Context, method, path string, opts requestOptions, out any) error {
	err := r.requestOnce(ctx, method, path, opts, out)
	if err == nil {
		return nil
	}
	imError, ok := err.(*IMError)
	if !ok || !imError.IsAuthError() || opts.noAuth {
		return err
	}
	r.mu.Lock()
	previous := r.credential
	getCredential := r.getCredential
	r.mu.Unlock()
	// Credential expired/revoked: refresh once through the host callback
	// and retry a single time; never auto-retry an intentionally
	// anonymous request. Other errors propagate unchanged.
	if getCredential == nil || previous == "" {
		return err
	}
	fresh, err := getCredential(ctx)
	if err != nil {
		return err
	}
	if fresh == "" || fresh == previous {
		return imError
	}
	r.SetCredential(fresh)
	return r.requestOnce(ctx, method, path, opts, out)
}

func (r *RESTClient) requestOnce(ctx context.Context, method, path string, opts requestOptions, out any) error {
	url := buildURL(r.baseURL, path, opts.query)
	headers := make(map[string]string, len(opts.headers)+2)
	for name, value := range opts.headers {
		headers[name] = value
	}
	r.mu.Lock()
	credential := r.credential
	r.mu.Unlock()
	if !opts.noAuth && credential != "" {
		headers["Authorization"] = "Bearer " + credential
	}
	if opts.body != nil {
		headers["Content-Type"] = "application/json"
	}
	response, err := r.transport.Send(ctx, &HTTPRequest{
		URL:     url,
		Method:  method,
		Headers: headers,
		Body:    opts.body,
	})
	if err != nil {
		// Transport failure: no HTTP status, safe to retry at a higher level.
		return imErr(err)
	}
	return unwrapEnvelope(response, out)
}

// unwrapEnvelope unwraps the {code, data, message} envelope: decodes data
// into out on success, returns *IMError otherwise.
func unwrapEnvelope(response *HTTPResponse, out any) error {
	var envelope struct {
		Code    *int            `json:"code"`
		Data    json.RawMessage `json:"data"`
		Message *string         `json:"message"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || envelope.Code == nil {
		return &IMError{
			Code:    -1,
			Status:  response.Status,
			Message: fmt.Sprintf("HTTP %d: unexpected response", response.Status),
		}
	}
	if response.Status >= 200 && response.Status < 300 && *envelope.Code == 0 {
		if out == nil {
			return nil
		}
		data := envelope.Data
		if len(data) == 0 {
			data = []byte("null")
		}
		if err := json.Unmarshal(data, out); err != nil {
			return &IMError{
				Code:    -1,
				Status:  response.Status,
				Message: fmt.Sprintf("decode response: %v", err),
			}
		}
		return nil
	}
	message := fmt.Sprintf("HTTP %d", response.Status)
	if envelope.Message != nil {
		message = *envelope.Message
	}
	return &IMError{Code: *envelope.Code, Status: response.Status, Message: message}
}

func sanitizeFilename(filename string) string {
	replacer := strings.NewReplacer("\r", "_", "\n", "_", "\"", "_", "\\", "_")
	return replacer.Replace(filename)
}

var mimeTypes = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"svg":  "image/svg+xml",
	"pdf":  "application/pdf",
	"txt":  "text/plain",
	"md":   "text/markdown",
	"mp4":  "video/mp4",
	"mov":  "video/quicktime",
	"mp3":  "audio/mpeg",
	"zip":  "application/zip",
	"json": "application/json",
}

func mimeTypeFor(filename string) string {
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	if known, ok := mimeTypes[extension]; ok {
		return known
	}
	if detected := mime.TypeByExtension(extension); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

// UploadFileFromPath reads path from disk and uploads it (see UploadFile).
func (r *RESTClient) UploadFileFromPath(ctx context.Context, path string, dir string) (UploadResult, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return UploadResult{}, imErr(err)
	}
	return r.UploadFile(ctx, UploadInput{
		Filename: filepath.Base(path),
		Content:  content,
		Dir:      dir,
	})
}

// formatInt renders an int for query use; zero renders empty (omit).
func formatInt(value int64) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
