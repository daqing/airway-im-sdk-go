// Package airwayim is the Go SDK for the Airway IM plugin: it wraps the
// backend's REST API, the WebSocket gateway protocol, and the
// server-to-server credential-minting endpoint into ready-to-use Go
// interfaces, so Go backends and Go client applications never have to
// implement credentials, the gateway first-frame authentication,
// heartbeats, reconnect-with-backoff, or sequence-based catch-up sync
// themselves.
//
// The public API mirrors airway-im-sdk-ts and airway-im-sdk-swift one to
// one; the package is standard-library only.
//
// A typical client integration:
//
//	im := airwayim.New(
//	    "https://im.example.com", // IM backend (:1905)
//	    "wss://im.example.com",   // gateway (:1910); "" for REST-only use
//	    credential,               // host-issued credential from your backend
//	)
//	im.OnMessage(func(message airwayim.ChatMessage) { ... })
//	im.Connect()
//	direct, err := im.CreateDirect(ctx, otherUserUUID)
//	direct.OnMessage(func(message airwayim.ChatMessage) { ... })
//	_, err = direct.Send(ctx, "hi", nil)
//
// A typical server integration mints credentials for logged-in users:
//
//	internal := airwayim.NewInternalClient(
//	    "http://127.0.0.1:1906",              // internal listener, private network only
//	    os.Getenv("IM_INTERNAL_SECRET"))
//	minted, err := internal.MintCredential(ctx, airwayim.MintOptions{
//	    UUID: user.UUID, Name: user.Username})
//
// Versioned in lockstep with the other Airway IM SDKs (currently 0.7.0).
package airwayim
