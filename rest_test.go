package airwayim

import (
	"context"
	"strings"
	"testing"
)

func TestMeSendsBearerAndUnwrapsEnvelope(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if req.Path != "/api/v1/me" {
			t.Errorf("path = %q", req.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-credential" {
			t.Errorf("authorization = %q", got)
		}
		return 200, respond(map[string]any{
			"uuid": "user-42", "username": "alice", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
		})
	})
	rest := testREST(t, server.URL, nil)
	user, err := rest.Me(context.Background())
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if user.UUID != "user-42" || user.Username != "alice" {
		t.Fatalf("unexpected user: %+v", user)
	}
}

func TestErrorEnvelopeBecomesIMError(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 403, respondError(CodePermissionDenied, "not a member")
	})
	rest := testREST(t, server.URL, nil)
	_, err := rest.GetConversation(context.Background(), "conv1")
	imError, ok := err.(*IMError)
	if !ok {
		t.Fatalf("expected *IMError, got %T: %v", err, err)
	}
	if imError.Code != CodePermissionDenied || imError.Status != 403 || imError.Message != "not a member" {
		t.Fatalf("unexpected error: %+v", imError)
	}
	if imError.IsAuthError() {
		t.Fatal("403/10005 must not be an auth error")
	}
}

func TestNonEnvelopeBodyBecomesCodeMinusOne(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 200, map[string]any{"surprise": true}
	})
	rest := testREST(t, server.URL, nil)
	_, err := rest.Me(context.Background())
	imError, ok := err.(*IMError)
	if !ok {
		t.Fatalf("expected *IMError, got %T", err)
	}
	if imError.Code != -1 || imError.Status != 200 {
		t.Fatalf("unexpected error: %+v", imError)
	}
}

func TestCreateDirectRequestBodyShape(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if req.Path != "/api/v1/conversations" {
			t.Errorf("path = %q", req.Path)
		}
		body := req.JSONBody()
		if body["kind"] != "direct" {
			t.Errorf("kind = %v", body["kind"])
		}
		members, _ := body["member_uuids"].([]any)
		if len(members) != 1 || members[0] != "user-2" {
			t.Errorf("member_uuids = %v", body["member_uuids"])
		}
		return 200, respond(map[string]any{"id": "conv1", "kind": "direct", "created_by": "user-1", "created_at": "t", "updated_at": "t"})
	})
	rest := testREST(t, server.URL, nil)
	conversation, err := rest.CreateDirect(context.Background(), "user-2")
	if err != nil {
		t.Fatalf("createDirect: %v", err)
	}
	if conversation.ID != "conv1" || conversation.Kind != ConversationKindDirect {
		t.Fatalf("unexpected conversation: %+v", conversation)
	}
}

func TestCreateGroupBodyAllowsNullTitle(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		body := req.JSONBody()
		title, present := body["title"]
		if !present || title != nil {
			t.Errorf("title = %v (present=%v), want explicit null", title, present)
		}
		return 200, respond(map[string]any{"id": "g1", "kind": "group", "created_by": "user-1", "created_at": "t", "updated_at": "t"})
	})
	rest := testREST(t, server.URL, nil)
	if _, err := rest.CreateGroup(context.Background(), nil, []string{"user-2"}); err != nil {
		t.Fatalf("createGroup: %v", err)
	}
}

func TestGetDirectReturnsNilWhenNotFound(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 404, respondError(CodeConversationNotFound, "not found")
	})
	rest := testREST(t, server.URL, nil)
	conversation, err := rest.GetDirect(context.Background(), "user-2")
	if err != nil {
		t.Fatalf("getDirect: %v", err)
	}
	if conversation != nil {
		t.Fatalf("expected nil conversation, got %+v", conversation)
	}
}

func TestListMessagesQueryParameters(t *testing.T) {
	var seenQuery map[string][]string
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		seenQuery = req.Query
		return 200, respond([]any{})
	})
	rest := testREST(t, server.URL, nil)
	if _, err := rest.ListMessages(context.Background(), "conv 1", ListMessagesOptions{AfterSequence: 5, Limit: 50}); err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if req := server.capturedRequests()[0]; req.Target != "/api/v1/conversations/conv%201/messages?after_sequence=5&limit=50" {
		t.Fatalf("target = %q", req.Target)
	}
	if got := seenQuery["after_sequence"]; len(got) != 1 || got[0] != "5" {
		t.Fatalf("after_sequence = %v", seenQuery["after_sequence"])
	}
	if got := seenQuery["limit"]; len(got) != 1 || got[0] != "50" {
		t.Fatalf("limit = %v", seenQuery["limit"])
	}
	// Zero options are omitted entirely.
	if _, err := rest.ListMessages(context.Background(), "conv 1", ListMessagesOptions{}); err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(seenQuery) != 0 {
		t.Fatalf("expected no query, got %v", seenQuery)
	}
}

func TestSendMessageRetriesWithSameIdempotencyKey(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 200, respond(map[string]any{
			"id": "m1", "conversation_id": "conv1", "content": "hi", "content_type": "text/markdown",
			"created_at": "t", "sequence": 3,
			"sender": map[string]any{"uuid": "user-1", "username": "alice"},
		})
	})
	server.setDrops(2) // two transport failures, then success
	rest := testREST(t, server.URL, nil)
	message, err := rest.SendMessage(context.Background(), "conv1", "hi", &SendOptions{Retries: 2})
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if message.ID != "m1" || message.Sequence != 3 {
		t.Fatalf("unexpected message: %+v", message)
	}
	requests := server.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(requests))
	}
	first := requests[0].Header.Get("Idempotency-Key")
	if first == "" {
		t.Fatal("idempotency key must be set")
	}
	for i, request := range requests {
		if got := request.Header.Get("Idempotency-Key"); got != first {
			t.Fatalf("attempt %d reused a different key: %q vs %q", i, got, first)
		}
	}
}

func TestSendMessageGivesUpAfterRetries(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 200, respond(nil)
	})
	server.setDrops(5)
	rest := testREST(t, server.URL, nil)
	_, err := rest.SendMessage(context.Background(), "conv1", "hi", &SendOptions{Retries: 2})
	imError, ok := err.(*IMError)
	if !ok {
		t.Fatalf("expected *IMError, got %T: %v", err, err)
	}
	if imError.Status != 0 {
		t.Fatalf("expected transport failure (status 0), got %+v", imError)
	}
	if got := len(server.capturedRequests()); got != 3 { // 1 + 2 retries
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestCredentialRenewalRetriesOnce(t *testing.T) {
	var mintCalls int
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if req.Header.Get("Authorization") == "Bearer fresh-credential" {
			return 200, respond(map[string]any{"uuid": "user-1", "username": "alice", "created_at": "t", "updated_at": "t"})
		}
		return 401, respondError(CodeInvalidCredential, "expired")
	})
	rest := testREST(t, server.URL, func(ctx context.Context) (string, error) {
		mintCalls++
		return "fresh-credential", nil
	})
	user, err := rest.Me(context.Background())
	if err != nil {
		t.Fatalf("me after renewal: %v", err)
	}
	if user.Username != "alice" {
		t.Fatalf("unexpected user: %+v", user)
	}
	if mintCalls != 1 {
		t.Fatalf("getCredential called %d times, want 1", mintCalls)
	}
	// The renewed credential sticks for later requests.
	if _, err := rest.Me(context.Background()); err != nil {
		t.Fatalf("me after renewal: %v", err)
	}
	if got := len(server.capturedRequests()); got != 3 { // 401, retry, straight 200
		t.Fatalf("expected 3 requests total, got %d", got)
	}
}

func TestCredentialRenewalRefusedWhenCallbackReturnsSame(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 401, respondError(CodeInvalidCredential, "expired")
	})
	rest := testREST(t, server.URL, func(ctx context.Context) (string, error) {
		return "test-credential", nil // same as before
	})
	if _, err := rest.Me(context.Background()); err == nil {
		t.Fatal("expected the original auth error to propagate")
	}
}

func TestUploadFileMultipart(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if req.Path != "/api/v1/storage" {
			t.Errorf("path = %q", req.Path)
		}
		contentType := req.Header.Get("Content-Type")
		if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
			t.Errorf("content-type = %q", contentType)
		}
		// A bare (non-envelope) JSON response.
		return 200, map[string]any{"key": "uploads/a.png", "url": "http://x/a.png", "size": 3}
	})
	rest := testREST(t, server.URL, nil)
	result, err := rest.UploadFile(context.Background(), UploadInput{
		Filename: "pic.png",
		Content:  []byte("abc"),
		Dir:      "uploads",
	})
	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if result.Key != "uploads/a.png" || result.Size != 3 {
		t.Fatalf("unexpected result: %+v", result)
	}
	request := server.capturedRequests()[0]
	body := string(request.Body)
	if !strings.Contains(body, `filename="pic.png"`) || !strings.Contains(body, "image/png") {
		t.Fatalf("multipart missing file part: %s", body)
	}
	if !strings.Contains(body, "uploads") {
		t.Fatalf("multipart missing dir field: %s", body)
	}
}

func TestUploadFileFailureSurfacesError(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 413, map[string]any{"error": "file too large"}
	})
	rest := testREST(t, server.URL, nil)
	_, err := rest.UploadFile(context.Background(), UploadInput{Filename: "a.png", Content: []byte("abc")})
	imError, ok := err.(*IMError)
	if !ok {
		t.Fatalf("expected *IMError, got %T: %v", err, err)
	}
	if imError.Status != 413 || imError.Message != "file too large" {
		t.Fatalf("unexpected error: %+v", imError)
	}
}

func TestEachMessageAutoPaging(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		after := req.Query["after_sequence"]
		switch {
		case len(after) == 0 || after[0] == "0":
			return 200, respond([]any{
				map[string]any{"id": "m1", "conversation_id": "c", "content": "a", "content_type": "text/plain", "created_at": "t", "sequence": 1, "sender": map[string]any{"uuid": "u", "username": "n"}},
				map[string]any{"id": "m2", "conversation_id": "c", "content": "b", "content_type": "text/plain", "created_at": "t", "sequence": 2, "sender": map[string]any{"uuid": "u", "username": "n"}},
			})
		case after[0] == "2":
			return 200, respond([]any{
				map[string]any{"id": "m3", "conversation_id": "c", "content": "c", "content_type": "text/plain", "created_at": "t", "sequence": 3, "sender": map[string]any{"uuid": "u", "username": "n"}},
			})
		default:
			t.Errorf("unexpected after_sequence %v", after)
			return 500, nil
		}
	})
	rest := testREST(t, server.URL, nil)
	var sequences []int64
	err := rest.EachMessage(context.Background(), "c", EachMessageOptions{PageSize: 2}, func(message ChatMessage) {
		sequences = append(sequences, message.Sequence)
	})
	if err != nil {
		t.Fatalf("eachMessage: %v", err)
	}
	if len(sequences) != 3 || sequences[0] != 1 || sequences[2] != 3 {
		t.Fatalf("unexpected order: %v", sequences)
	}
}

func TestStorageURL(t *testing.T) {
	rest := testREST(t, "http://stub:1905/", nil)
	got := rest.StorageURL("a b/c+d.png")
	want := "http://stub:1905/api/v1/storage/a%20b/c%2Bd.png"
	if got != want {
		t.Fatalf("storageURL = %q, want %q", got, want)
	}
}

func TestRemoveMembersSequencedRequests(t *testing.T) {
	details := map[string]any{
		"conversation_uuid": "g1", "type": "group", "title": nil,
		"members": []any{map[string]any{"uuid": "user-1", "username": "alice", "role": "owner"}},
	}
	var removed []string
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if req.Method == "DELETE" && strings.HasPrefix(req.Path, "/api/v1/conversations/g1/members/") {
			removed = append(removed, strings.TrimPrefix(req.Path, "/api/v1/conversations/g1/members/"))
			return 200, respond(details)
		}
		t.Errorf("unexpected request %s %s", req.Method, req.Path)
		return 500, nil
	})
	rest := testREST(t, server.URL, nil)
	result, err := rest.RemoveMembers(context.Background(), "g1", []string{"user-2", "user-3"})
	if err != nil {
		t.Fatalf("removeMembers: %v", err)
	}
	if len(result.Members) != 1 || result.Members[0].Role != MemberRoleOwner {
		t.Fatalf("unexpected details: %+v", result)
	}
	if len(removed) != 2 || removed[0] != "user-2" || removed[1] != "user-3" {
		t.Fatalf("unexpected removals: %v", removed)
	}
}
