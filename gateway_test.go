package airwayim

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recordedGateway collects everything a gateway reports to its owner.
type recordedGateway struct {
	statuses capture[ConnectionStatus]
	events   capture[GatewayEvent]
	errors   capture[string]
	ready    capture[struct{}]
}

func (r *recordedGateway) callbacks() GatewayCallbacks {
	return GatewayCallbacks{
		OnEvent:  r.events.append,
		OnStatus: r.statuses.append,
		OnReady:  func() { r.ready.append(struct{}{}) },
		OnError:  func(err *IMError) { r.errors.append(err.Message) },
	}
}

func hasStatus(statuses []ConnectionStatus, want ConnectionStatus) bool {
	for _, status := range statuses {
		if status == want {
			return true
		}
	}
	return false
}

func sentContains(socket *fakeSocket, needle string) bool {
	for _, text := range socket.sentTexts() {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func TestGatewayAuthenticatesAndReceivesEvents(t *testing.T) {
	factory := &fakeSocketFactory{}
	recorded := &recordedGateway{}
	gateway := newGatewaySocket("ws://stub:1910", "cred-1", 50*time.Millisecond, nil, factory.make, recorded.callbacks())
	// Auth frames are answered OK; command replies never surface as events.
	factory.setOnSend(func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			if !strings.Contains(text, "cred-1") {
				t.Errorf("auth frame used wrong credential: %s", text)
			}
			go fake.serverEnqueue(`{"code":0,"data":"OK"}`)
		}
	})
	gateway.Connect()

	waitFor(t, "online", 2*time.Second, func() bool { return gateway.IsOnline() })
	socket := factory.last()
	if socket == nil || socket.openedURL() != "ws://stub:1910/ws" {
		t.Fatalf("gateway must dial <wsURL>/ws")
	}
	socket.serverEnqueue(eventFrame("evt-1", EventMessageCreated, "conv1", 3, "m1"))
	socket.serverEnqueue(eventFrame("evt-1", EventMessageCreated, "conv1", 3, "m1")) // duplicate
	socket.serverEnqueue(eventFrame("evt-2", EventMessageCreated, "conv1", 4, "m2"))
	socket.serverEnqueue(`{"code":0,"data":"PONG"}`) // command reply: not an event

	waitFor(t, "two deduplicated events", 2*time.Second, func() bool {
		return recorded.events.count() == 2
	})
	events := recorded.events.all()
	if events[0].EventID != "evt-1" || events[1].EventID != "evt-2" {
		t.Fatalf("unexpected event order: %v", events)
	}
	if events[0].Sequence != 3 || events[0].MessageID != "m1" {
		t.Fatalf("event fields lost: %+v", events[0])
	}
	// Heartbeat pings flow once online.
	waitFor(t, "application ping", 2*time.Second, func() bool {
		return sentContains(socket, `"ping"`)
	})
	gateway.Disconnect()
	waitFor(t, "closed status", 2*time.Second, func() bool {
		status, _ := recorded.statuses.last()
		return status == StatusClosed
	})
	if gateway.IsOnline() {
		t.Fatal("gateway must be offline after Disconnect")
	}
}

func TestGatewayAuthFailureTriggersCredentialRefresh(t *testing.T) {
	factory := &fakeSocketFactory{}
	recorded := &recordedGateway{}
	refreshes := 0
	getCredential := func(ctx context.Context) (string, error) {
		refreshes++
		return "fresh-cred", nil
	}
	gateway := newGatewaySocket("ws://stub", "stale-cred", time.Second, getCredential, factory.make, recorded.callbacks())
	factory.setOnSend(func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			if strings.Contains(text, "fresh-cred") {
				go fake.serverEnqueue(`{"code":0,"data":"OK"}`)
			} else {
				go fake.serverEnqueue(`{"code":10001,"message":"invalid credential"}`)
			}
		}
	})
	gateway.Connect()

	waitFor(t, "online after refresh", 3*time.Second, func() bool { return gateway.IsOnline() })
	if refreshes != 1 {
		t.Fatalf("getCredential called %d times, want 1", refreshes)
	}
	if got := len(factory.all()); got != 2 {
		t.Fatalf("expected a fresh socket after rejection, got %d", got)
	}
	if !hasStatus(recorded.statuses.all(), StatusAuthenticating) {
		t.Fatalf("statuses missing authenticating: %v", recorded.statuses.all())
	}
	waitFor(t, "auth failure reported", time.Second, func() bool {
		return recorded.errors.count() > 0
	})
	message, _ := recorded.errors.last()
	if !strings.Contains(message, "gateway auth failed") {
		t.Fatalf("unexpected error: %q", message)
	}
	gateway.Disconnect()
}

func TestGatewayReconnectsAfterTransportLoss(t *testing.T) {
	factory := &fakeSocketFactory{}
	recorded := &recordedGateway{}
	gateway := newGatewaySocket("ws://stub", "cred", time.Second, nil, factory.make, recorded.callbacks())
	factory.setOnSend(func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			go fake.serverEnqueue(`{"code":0,"data":"OK"}`)
		}
	})
	gateway.Connect()
	waitFor(t, "first connection online", 2*time.Second, func() bool { return gateway.IsOnline() })

	// The server drops the connection; the gateway must reconnect with a
	// fresh socket and re-authenticate.
	factory.all()[0].serverClose()
	waitFor(t, "second socket", 5*time.Second, func() bool {
		return len(factory.all()) == 2
	})
	waitFor(t, "online again", 3*time.Second, func() bool { return gateway.IsOnline() })
	readyCount := recorded.ready.count()
	if readyCount < 2 {
		t.Fatalf("ready fired %d times, want >= 2 (once per reconnect)", readyCount)
	}
	if !hasStatus(recorded.statuses.all(), StatusReconnecting) {
		t.Fatalf("statuses missing reconnecting: %v", recorded.statuses.all())
	}
	gateway.Disconnect()
}

func TestGatewayStopsWhenRefreshCannotHelp(t *testing.T) {
	factory := &fakeSocketFactory{}
	recorded := &recordedGateway{}
	refreshes := 0
	getCredential := func(ctx context.Context) (string, error) {
		refreshes++
		return "still-invalid", nil // a new but equally invalid credential
	}
	gateway := newGatewaySocket("ws://stub", "still-invalid", time.Second, getCredential, factory.make, recorded.callbacks())
	factory.setOnSend(func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			go fake.serverEnqueue(`{"code":10001,"message":"invalid credential"}`)
		}
	})
	gateway.Connect()

	// One refresh attempt per credential value; afterwards the loop stops
	// with StatusOffline instead of hammering the gateway forever.
	waitFor(t, "offline", 5*time.Second, func() bool {
		status, _ := recorded.statuses.last()
		return status == StatusOffline
	})
	if refreshes != 1 {
		t.Fatalf("getCredential called %d times, want 1", refreshes)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(factory.all()) > 2 {
			t.Fatalf("gateway kept reconnecting: %d sockets", len(factory.all()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	gateway.Disconnect()
}

func TestGatewayConnectIsIdempotent(t *testing.T) {
	factory := &fakeSocketFactory{}
	recorded := &recordedGateway{}
	gateway := newGatewaySocket("ws://stub", "cred", time.Second, nil, factory.make, recorded.callbacks())
	factory.setOnSend(func(fake *fakeSocket, text string) {
		if strings.Contains(text, `"auth"`) {
			go fake.serverEnqueue(`{"code":0,"data":"OK"}`)
		}
	})
	gateway.Connect()
	gateway.Connect()
	gateway.Connect()
	waitFor(t, "online", 2*time.Second, func() bool { return gateway.IsOnline() })
	time.Sleep(100 * time.Millisecond)
	if got := len(factory.all()); got != 1 {
		t.Fatalf("repeated Connect created %d sockets, want 1", got)
	}
	gateway.Disconnect()
}
