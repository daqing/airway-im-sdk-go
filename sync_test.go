package airwayim

import (
	"context"
	"testing"
)

func messageJSON(id, conversationID string, sequence int64, content string) map[string]any {
	return map[string]any{
		"id": id, "conversation_id": conversationID, "content": content,
		"content_type": "text/plain", "created_at": "2026-01-01T00:00:00Z",
		"sequence": sequence,
		"sender":   map[string]any{"uuid": "user-1", "username": "alice"},
	}
}

func TestFetchFromTracksAndEmits(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 200, respond([]any{
			messageJSON("m1", "conv1", 1, "hello"),
			messageJSON("m2", "conv1", 2, "world"),
		})
	})
	store := NewInMemorySequenceStore()
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{
		onMessages:       func(messages []ChatMessage) {},
		onMessageUpdated: func(ChatMessage) {},
	}, store)

	var emitted []ChatMessage
	engine.handlers.onMessages = func(messages []ChatMessage) {
		emitted = append(emitted, messages...)
	}
	messages, err := engine.FetchFrom(context.Background(), "conv1", HistoryOptions{})
	if err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	if len(messages) != 2 || messages[1].Sequence != 2 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if len(emitted) != 2 {
		t.Fatalf("handlers saw %d messages, want 2", len(emitted))
	}
	if engine.LastSequence("conv1") != 2 {
		t.Fatalf("lastSequence = %d, want 2", engine.LastSequence("conv1"))
	}
	stored, ok := store.Get("airway-im.sequences")
	if !ok || stored != `{"conv1":2}` {
		t.Fatalf("persisted store = %q", stored)
	}
}

func TestFetchFromPagesUntilShort(t *testing.T) {
	var fetches int
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		fetches++
		limit := req.Query["limit"][0]
		if limit != "1" {
			t.Errorf("limit = %q, want 1", limit)
		}
		after := "0"
		if values := req.Query["after_sequence"]; len(values) > 0 {
			after = values[0]
		}
		switch after {
		case "0":
			return 200, respond([]any{messageJSON("m1", "c", 1, "a")})
		case "1":
			return 200, respond([]any{messageJSON("m2", "c", 2, "b")})
		case "2":
			return 200, respond([]any{})
		default:
			t.Errorf("unexpected after_sequence %q", after)
			return 500, nil
		}
	})
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{}, nil)
	messages, err := engine.FetchFrom(context.Background(), "c", HistoryOptions{Limit: 1})
	if err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	if len(messages) != 2 || fetches != 3 {
		t.Fatalf("messages=%d fetches=%d, want 2/3", len(messages), fetches)
	}
}

func TestFetchFromStartsFromPersistedCursor(t *testing.T) {
	var seenAfter []string
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		seenAfter = append(seenAfter, req.Query["after_sequence"][0])
		return 200, respond([]any{})
	})
	store := NewInMemorySequenceStore()
	store.Set("airway-im.sequences", `{"conv1":7}`)
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{}, store)
	if engine.LastSequence("conv1") != 7 {
		t.Fatalf("cursor not restored: %d", engine.LastSequence("conv1"))
	}
	if _, err := engine.FetchFrom(context.Background(), "conv1", HistoryOptions{}); err != nil {
		t.Fatalf("fetchFrom: %v", err)
	}
	if len(seenAfter) != 1 || seenAfter[0] != "7" {
		t.Fatalf("after_sequence values = %v, want [7]", seenAfter)
	}
	// A corrupted cache starts clean instead of failing.
	broken := NewInMemorySequenceStore()
	broken.Set("airway-im.sequences", "{not json")
	engine = newSyncEngine(testREST(t, server.URL, nil), syncHandlers{}, broken)
	if engine.LastSequence("conv1") != 0 {
		t.Fatalf("corrupted cache should start clean, got %d", engine.LastSequence("conv1"))
	}
}

func TestHandleMessageEventGapHealAndDedupe(t *testing.T) {
	var fetches int
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		fetches++
		return 200, respond([]any{messageJSON("m8", "conv1", 8, "gap fill")})
	})
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{}, nil)
	engine.Track("conv1", 5)

	// Untracked conversations are left to the application.
	engine.HandleMessageEvent(context.Background(), GatewayEvent{
		EventID: "e1", Event: EventMessageCreated, ConversationID: "conv2", Sequence: 9,
	})
	if fetches != 0 {
		t.Fatalf("untracked conversation fetched %d times", fetches)
	}

	// Duplicate or already-applied sequence: no fetch.
	engine.HandleMessageEvent(context.Background(), GatewayEvent{
		EventID: "e2", Event: EventMessageCreated, ConversationID: "conv1", Sequence: 5,
	})
	if fetches != 0 {
		t.Fatalf("stale event fetched %d times", fetches)
	}

	// A gap triggers exactly one serialized fetch and raises the cursor.
	engine.HandleMessageEvent(context.Background(), GatewayEvent{
		EventID: "e3", Event: EventMessageCreated, ConversationID: "conv1", Sequence: 8,
	})
	if fetches != 1 {
		t.Fatalf("gap event fetched %d times, want 1", fetches)
	}
	if engine.LastSequence("conv1") != 8 {
		t.Fatalf("cursor = %d, want 8", engine.LastSequence("conv1"))
	}
}

func TestHandleMessageModeratedEventReloadsMaskedMessage(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		if after := req.Query["after_sequence"][0]; after != "4" {
			t.Errorf("after_sequence = %q, want 4 (sequence-1)", after)
		}
		return 200, respond([]any{
			messageJSON("other", "conv1", 5, "not it"),
			messageJSON("m5", "conv1", 5, "***"),
		})
	})
	var updated ChatMessage
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{
		onMessages:       func([]ChatMessage) {},
		onMessageUpdated: func(message ChatMessage) { updated = message },
	}, nil)
	engine.Track("conv1", 5)
	engine.HandleMessageModeratedEvent(context.Background(), GatewayEvent{
		EventID: "e1", Event: EventMessageModerated, ConversationID: "conv1",
		Sequence: 5, MessageID: "m5",
	})
	if updated.ID != "m5" || updated.Content != "***" {
		t.Fatalf("unexpected updated message: %+v", updated)
	}
}

func TestForgetDropsState(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 200, respond([]any{})
	})
	store := NewInMemorySequenceStore()
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{}, store)
	engine.Track("conv1", 3)
	engine.Forget("conv1")
	if engine.IsTracked("conv1") {
		t.Fatal("conversation should be untracked after Forget")
	}
	stored, _ := store.Get("airway-im.sequences")
	if stored != `{}` {
		t.Fatalf("persisted store = %q, want empty object", stored)
	}
}

func TestTrackNeverLowersCursor(t *testing.T) {
	server := newStubServer(t, func(req capturedRequest) (int, any) {
		return 200, respond([]any{})
	})
	engine := newSyncEngine(testREST(t, server.URL, nil), syncHandlers{}, nil)
	engine.Track("conv1", 10)
	engine.Track("conv1", 4)
	if engine.LastSequence("conv1") != 10 {
		t.Fatalf("cursor = %d, want 10", engine.LastSequence("conv1"))
	}
}
