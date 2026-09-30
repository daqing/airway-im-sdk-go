package airwayim

import (
	"strings"
	"testing"
	"time"
)

// newTestSession builds a REST-only session against the stub server.
func newTestSession(t *testing.T, handler func(req capturedRequest) (int, any), opts ...Option) (*Session, *stubServer) {
	t.Helper()
	server := newStubServer(t, handler)
	opts = append([]Option{
		WithHTTPTransport(NewHTTPTransport(2 * time.Second)),
	}, opts...)
	session := New(server.URL, "", "test-credential", opts...)
	t.Cleanup(session.Close)
	return session, server
}

func TestSessionConversationHandlesAreCachedPerID(t *testing.T) {
	conversationJSON := map[string]any{"id": "conv-direct", "kind": "direct", "created_by": "u1", "created_at": "t", "updated_at": "t"}
	groupJSON := map[string]any{"id": "conv-group", "kind": "group", "title": "Team", "created_by": "u1", "created_at": "t", "updated_at": "t"}
	detailsJSON := map[string]any{
		"conversation_uuid": "conv-open", "type": "group", "title": nil,
		"members": []any{},
	}
	session, _ := newTestSession(t, func(req capturedRequest) (int, any) {
		switch req.Path {
		case "/api/v1/conversations":
			return 200, respond(conversationJSON)
		case "/api/v1/group":
			return 200, respond(groupJSON)
		case "/api/v1/conversations/conv-open":
			return 200, respond(detailsJSON)
		default:
			t.Errorf("unexpected path %q", req.Path)
			return 500, nil
		}
	})

	direct, err := session.CreateDirect(t.Context(), "user-2")
	if err != nil {
		t.Fatalf("createDirect: %v", err)
	}
	if direct.Kind() != ConversationKindDirect {
		t.Fatalf("kind = %v", direct.Kind())
	}
	again, err := session.CreateDirect(t.Context(), "user-2")
	if err != nil {
		t.Fatalf("createDirect: %v", err)
	}
	if again != direct {
		t.Fatal("the same conversation must yield the same handle object")
	}

	group, err := session.CreateGroup(t.Context(), strPtr("Team"), []string{"user-2"})
	if err != nil {
		t.Fatalf("createGroup: %v", err)
	}
	if group.Kind() != ConversationKindGroup || group.Title == nil || *group.Title != "Team" {
		t.Fatalf("unexpected group handle: %+v", group)
	}

	opened, err := session.OpenConversation(t.Context(), "conv-open")
	if err != nil {
		t.Fatalf("openConversation: %v", err)
	}
	if _, ok := opened.(*GroupConversation); !ok {
		t.Fatalf("openConversation returned %T, want *GroupConversation", opened)
	}
}

func TestSessionGetDirectNilWhenNone(t *testing.T) {
	session, _ := newTestSession(t, func(req capturedRequest) (int, any) {
		return 404, respondError(CodeConversationNotFound, "not found")
	})
	direct, err := session.GetDirect(t.Context(), "user-2")
	if err != nil {
		t.Fatalf("getDirect: %v", err)
	}
	if direct != nil {
		t.Fatalf("expected nil handle, got %+v", direct)
	}
}

func TestSessionFirstMessageListenerTracksConversation(t *testing.T) {
	var messageCount int
	session, _ := newTestSession(t, func(req capturedRequest) (int, any) {
		if strings.HasSuffix(req.Path, "/messages") {
			messageCount++
			return 200, respond([]any{
				messageJSON("m1", "conv-direct", 1, "backlog"),
			})
		}
		if req.Path == "/api/v1/conversations" {
			return 200, respond(map[string]any{"id": "conv-direct", "kind": "direct", "created_by": "u1", "created_at": "t", "updated_at": "t"})
		}
		t.Errorf("unexpected path %q", req.Path)
		return 500, nil
	})

	var received capture[ChatMessage]
	session.OnMessage(func(message ChatMessage) { received.append(message) })

	direct, err := session.CreateDirect(t.Context(), "user-2")
	if err != nil {
		t.Fatalf("createDirect: %v", err)
	}
	direct.OnMessage(func(message ChatMessage) { received.append(message) })

	// The first listener triggers a background history fetch; the message
	// arrives on both the global stream and the handle.
	waitFor(t, "backlog delivered", 2*time.Second, func() bool {
		return received.count() >= 2
	})
	waitFor(t, "history fetched", 2*time.Second, func() bool { return messageCount >= 1 })
	if session.LastSequence("conv-direct") != 1 {
		t.Fatalf("lastSequence = %d, want 1", session.LastSequence("conv-direct"))
	}
}

func TestSessionSendGroupMessageTracksSequence(t *testing.T) {
	session, server := newTestSession(t, func(req capturedRequest) (int, any) {
		if req.Path == "/api/v1/messages" {
			if got := req.Header.Get("Idempotency-Key"); got == "" {
				t.Error("send must carry an Idempotency-Key")
			}
			return 200, respond(messageJSON("m9", "conv-group", 9, "hi"))
		}
		t.Errorf("unexpected path %q", req.Path)
		return 500, nil
	})
	message, err := session.SendGroupMessage(t.Context(), "conv-group", "hi", nil)
	if err != nil {
		t.Fatalf("sendGroupMessage: %v", err)
	}
	if message.Sequence != 9 {
		t.Fatalf("sequence = %d", message.Sequence)
	}
	if session.LastSequence("conv-group") != 9 {
		t.Fatalf("sender's cursor = %d, want 9 (no redundant refetch)",
			session.LastSequence("conv-group"))
	}
	_ = server
}

func TestSessionForgetConversationDropsState(t *testing.T) {
	session, _ := newTestSession(t, func(req capturedRequest) (int, any) {
		return 200, respond([]any{})
	})
	session.History(t.Context(), "conv1", HistoryOptions{})
	if !session.sync.IsTracked("conv1") {
		t.Fatal("conversation should be tracked after History")
	}
	session.ForgetConversation("conv1")
	if session.sync.IsTracked("conv1") {
		t.Fatal("conversation should be untracked after ForgetConversation")
	}
	if _, known := session.hub.registry.kind("conv1"); known {
		t.Fatal("kind should be forgotten")
	}
}

func TestSessionRESTOnlyConnectReportsError(t *testing.T) {
	var errors capture[string]
	session, _ := newTestSession(t, func(req capturedRequest) (int, any) {
		return 200, respond(nil)
	})
	session.OnError(func(err *IMError) { errors.append(err.Message) })
	session.Connect()
	waitFor(t, "realtime-disabled error", time.Second, func() bool {
		return errors.count() > 0
	})
	message, _ := errors.last()
	if !strings.Contains(message, "No wsURL") {
		t.Fatalf("unexpected error: %q", message)
	}
}

func TestSessionGatewayEndToEndWithFakeSocket(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		switch {
		case req.Path == "/api/v1/conversations/conv-group/messages":
			after := "0"
			if values := req.Query["after_sequence"]; len(values) > 0 {
				after = values[0]
			}
			if after == "1" {
				return 200, respond([]any{messageJSON("m2", "conv-group", 2, "gap fill")})
			}
			return 200, respond([]any{messageJSON("m1", "conv-group", 1, "synced")})
		case req.Path == "/api/v1/group":
			return 200, respond(map[string]any{"id": "conv-group", "kind": "group", "created_by": "u1", "created_at": "t", "updated_at": "t"})
		default:
			t.Errorf("unexpected path %q", req.Path)
			return 500, nil
		}
	})
	factory := &fakeSocketFactory{}
	factory.setOnSend(func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			go fake.serverEnqueue(`{"code":0,"data":"OK"}`)
		}
	})
	session := New(server.URL, "ws://stub", "test-credential",
		WithHTTPTransport(NewHTTPTransport(2*time.Second)),
		WithSocketFactory(factory.make),
		WithPingInterval(time.Second),
	)
	t.Cleanup(session.Close)

	var statuses capture[ConnectionStatus]
	session.OnStatus(func(status ConnectionStatus) { statuses.append(status) })

	group, err := session.CreateGroup(t.Context(), nil, []string{"user-2"})
	if err != nil {
		t.Fatalf("createGroup: %v", err)
	}
	var received capture[ChatMessage]
	group.OnMessage(func(message ChatMessage) { received.append(message) })

	session.Connect()
	waitFor(t, "online", 2*time.Second, func() bool { return session.IsOnline() })
	// The auto-track from OnMessage runs in the background; wait until the
	// initial history fetch has landed before pushing a realtime event.
	waitFor(t, "initial history synced", 2*time.Second, func() bool {
		return session.LastSequence("conv-group") == 1
	})

	// A realtime event for the tracked conversation heals the gap via
	// HTTP and delivers the message on the handle.
	factory.last().serverEnqueue(eventFrame("evt-1", EventMessageCreated, "conv-group", 2, "m2"))
	waitFor(t, "gap-healed delivery", 2*time.Second, func() bool {
		return received.count() >= 2
	})
	if session.LastSequence("conv-group") < 2 {
		t.Fatalf("cursor = %d, want >= 2", session.LastSequence("conv-group"))
	}

	// Member events surface on the facade and mark the conversation group.
	var membersAdded capture[MembersAddedInfo]
	session.OnMembersAdded(func(info MembersAddedInfo) { membersAdded.append(info) })
	factory.last().serverEnqueue(`{"event_id":"evt-2","event":"conversation.member_added","conversation_id":"conv-group","added_user_uuids":["user-2"]}`)
	waitFor(t, "members added", 2*time.Second, func() bool {
		return membersAdded.count() > 0
	})
	info, _ := membersAdded.last()
	if info.ConversationID != "conv-group" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if _, known := session.hub.registry.kind("conv-group"); !known {
		t.Fatal("member event should have remembered the conversation kind")
	}

	session.Disconnect()
	waitFor(t, "closed", 2*time.Second, func() bool {
		return session.ConnectionStatus() == StatusClosed
	})
}

func TestSessionCloseStopsEverything(t *testing.T) {
	session, _ := newTestSession(t, func(req capturedRequest) (int, any) {
		return 200, respond(nil)
	}, WithAutoConnect(false))
	session.Close()
	// Double close is safe.
	session.Close()
	if !session.isClosed() {
		t.Fatal("session should report closed")
	}
}

func strPtr(v string) *string { return &v }
