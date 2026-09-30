package airwayim

import (
	"context"
	"testing"
)

func TestAdminLoginFlow(t *testing.T) {
	var logins int
	var statusAuth string
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		switch req.Path {
		case "/admin/api/login":
			logins++
			body := req.JSONBody()
			if body["username"] != "admin" || body["password"] != "secret" {
				t.Errorf("login body = %v", body)
			}
			return 200, respond(map[string]any{
				"token": "session-token", "expires_at": "2026-10-01T12:00:00Z", "username": "admin",
			})
		case "/admin/api/status":
			statusAuth = req.Header.Get("Authorization")
			return 200, respond(map[string]any{"users": 2, "gateway": map[string]any{"online": 1}})
		case "/admin/api/logout":
			return 200, respond(nil)
		default:
			t.Errorf("unexpected path %q", req.Path)
			return 500, nil
		}
	})
	client := NewAdminClient(server.URL, "admin", "secret")

	session, err := client.Login(context.Background())
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if session.Token != "session-token" || session.Username == nil || *session.Username != "admin" {
		t.Fatalf("unexpected session: %+v", session)
	}

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if statusAuth != "Bearer session-token" {
		t.Fatalf("status authorization = %q", statusAuth)
	}
	users, _ := status.Get("users").AsInt64()
	if users != 2 {
		t.Fatalf("status.users = %v", status)
	}
	// No second login for an authenticated call.
	if logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}

	if err := client.Logout(context.Background()); err != nil {
		t.Fatalf("logout: %v", err)
	}
}

func TestAdminImplicitLoginAndReLoginOn401(t *testing.T) {
	var logins, statuses int
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		switch req.Path {
		case "/admin/api/login":
			logins++
			return 200, respond(map[string]any{"token": "token-" + itoa(logins)})
		case "/admin/api/users":
			statuses++
			auth := req.Header.Get("Authorization")
			// First authenticated call uses token-1 and is rejected
			// (expired server-side); after re-login, token-2 works.
			if auth == "Bearer token-2" {
				return 200, respond([]any{map[string]any{"uuid": "u1", "token_version": 1}})
			}
			return 401, respondError(10002, "session expired")
		default:
			t.Errorf("unexpected path %q", req.Path)
			return 500, nil
		}
	})
	client := NewAdminClient(server.URL, "admin", "secret")
	users, err := client.Users(context.Background())
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("unexpected users: %v", users)
	}
	if logins != 2 {
		t.Fatalf("logins = %d, want 2 (implicit + re-login)", logins)
	}
	if statuses != 2 {
		t.Fatalf("status calls = %d, want 2 (401 + retry)", statuses)
	}
}

func TestAdminRevokeUserAndMessages(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		switch {
		case req.Path == "/admin/api/login":
			return 200, respond(map[string]any{"token": "t"})
		case req.Target == "/admin/api/users/user%202/revoke":
			return 200, respond(map[string]any{"revoked": "user 2"})
		case req.Target == "/admin/api/conversations/conv%201/messages":
			return 200, respond([]any{map[string]any{"id": "m1", "sequence": 1}})
		case req.Target == "/admin/api/messages/m1/mark-illegal":
			return 200, respond(map[string]any{"id": "m1", "content": "***"})
		default:
			t.Errorf("unexpected target %q", req.Target)
			return 500, nil
		}
	})
	client := NewAdminClient(server.URL, "admin", "secret")

	revoked, err := client.RevokeUser(context.Background(), "user 2")
	if err != nil {
		t.Fatalf("revokeUser: %v", err)
	}
	if mustString(revoked.Get("revoked")) != "user 2" {
		t.Fatalf("unexpected revoked: %v", revoked)
	}
	messages, err := client.ConversationMessages(context.Background(), "conv 1")
	if err != nil {
		t.Fatalf("conversationMessages: %v", err)
	}
	if len(messages) != 1 || mustString(messages[0].Get("id")) != "m1" {
		t.Fatalf("unexpected messages: %v", messages)
	}
	marked, err := client.MarkIllegal(context.Background(), "m1")
	if err != nil {
		t.Fatalf("markIllegal: %v", err)
	}
	if mustString(marked.Get("content")) != "***" {
		t.Fatalf("unexpected marked: %v", marked)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func mustString(value JSONValue) string {
	text, _ := value.AsString()
	return text
}
