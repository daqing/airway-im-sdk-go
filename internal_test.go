package airwayim

import (
	"context"
	"testing"
	"time"
)

func TestMintCredentialSendsInternalSecretAndBody(t *testing.T) {
	var (
		secret  string
		body    map[string]any
		authSet bool
	)
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if req.Path != "/internal/v1/credentials" {
			t.Errorf("path = %q", req.Path)
		}
		secret = req.Header.Get("X-IM-Internal-Secret")
		authSet = req.Header.Get("Authorization") != ""
		body = req.JSONBody()
		return 200, respond(map[string]any{
			"credential": "im1.abc.sig",
			"expires_at": "2026-10-01T00:00:00Z",
		})
	})
	client := NewInternalClient(server.URL, "internal-secret")
	minted, err := client.MintCredential(context.Background(), MintOptions{
		UUID:       "user-42",
		Name:       "alice",
		Nickname:   "Alice",
		TTLSeconds: intPtr(3600),
	})
	if err != nil {
		t.Fatalf("mintCredential: %v", err)
	}
	if secret != "internal-secret" {
		t.Fatalf("internal secret header = %q", secret)
	}
	if authSet {
		t.Error("internal requests must not carry Authorization")
	}
	if body["uuid"] != "user-42" || body["name"] != "alice" || body["nickname"] != "Alice" {
		t.Fatalf("unexpected body: %v", body)
	}
	if body["avatar_url"] != nil {
		t.Errorf("avatar_url must be omitted when empty, got %v", body["avatar_url"])
	}
	if body["ttl_seconds"] != float64(3600) {
		t.Errorf("ttl_seconds = %v", body["ttl_seconds"])
	}
	if minted.Credential != "im1.abc.sig" || minted.ExpiresAt != "2026-10-01T00:00:00Z" {
		t.Fatalf("unexpected minted: %+v", minted)
	}
}

func TestMintCredentialOmitsTTLWhenNil(t *testing.T) {
	var body map[string]any
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		body = req.JSONBody()
		return 200, respond(map[string]any{"credential": "im1.abc.sig"})
	})
	client := NewInternalClient(server.URL, "s")
	minted, err := client.MintCredential(context.Background(), MintOptions{UUID: "u", Name: "n"})
	if err != nil {
		t.Fatalf("mintCredential: %v", err)
	}
	if _, present := body["ttl_seconds"]; present {
		t.Errorf("ttl_seconds must be omitted, body = %v", body)
	}
	if minted.ExpiresAt != "" {
		t.Errorf("expiresAt = %q, want empty", minted.ExpiresAt)
	}
}

func TestMintCredentialRejectsWrongSecret(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 403, respondError(10005, "invalid internal secret")
	})
	client := NewInternalClient(server.URL, "wrong")
	_, err := client.MintCredential(context.Background(), MintOptions{UUID: "u", Name: "n"})
	imError, ok := err.(*IMError)
	if !ok {
		t.Fatalf("expected *IMError, got %T: %v", err, err)
	}
	if imError.Code != 10005 || imError.Status != 403 {
		t.Fatalf("unexpected error: %+v", imError)
	}
}

func TestInternalClientCustomTimeout(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		time.Sleep(50 * time.Millisecond)
		return 200, respond(map[string]any{"credential": "c"})
	})
	client := NewInternalClient(server.URL, "s", WithInternalTransport(NewHTTPTransport(10*time.Millisecond)))
	if _, err := client.MintCredential(context.Background(), MintOptions{UUID: "u", Name: "n"}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func intPtr(v int) *int { return &v }
