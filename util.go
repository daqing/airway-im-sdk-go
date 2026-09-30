package airwayim

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

func trimTrailingSlashes(s string) string {
	return strings.TrimRight(s, "/")
}

func trimLeadingSlashes(s string) string {
	return strings.TrimLeft(s, "/")
}

func joinURL(base, path string) string {
	return trimTrailingSlashes(base) + "/" + trimLeadingSlashes(path)
}

// escapeSegment percent-encodes a path or query value so "/" or "?" in an
// identifier can never change the request shape. RFC 3986 unreserved
// characters stay as-is (a space becomes %20, form encoding's "+" would
// change the value in a path).
func escapeSegment(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// escapeSegments escapes every "/"-separated part of a storage key
// individually, keeping the separators.
func escapeSegments(key string) string {
	parts := strings.Split(key, "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		escaped = append(escaped, escapeSegment(part))
	}
	return strings.Join(escaped, "/")
}

type queryPair struct {
	name  string
	value string
}

// buildQuery renders query pairs in insertion order so generated URLs are
// deterministic. Empty values are skipped (the caller decides optionality).
func buildQuery(pairs []queryPair) string {
	var b strings.Builder
	first := true
	for _, pair := range pairs {
		if pair.value == "" {
			continue
		}
		if first {
			b.WriteByte('?')
			first = false
		} else {
			b.WriteByte('&')
		}
		b.WriteString(escapeSegment(pair.name))
		b.WriteByte('=')
		b.WriteString(escapeSegment(pair.value))
	}
	return b.String()
}

// buildURL joins base, path, and query into a request URL.
func buildURL(base, path string, query []queryPair) string {
	return joinURL(base, path) + buildQuery(query)
}

// randomID returns a random UUIDv4-shaped string (idempotency keys).
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on healthy runtimes; a fixed id would
		// collide messages, so surface the failure instead.
		panic("airwayim: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(b[:])
	return strings.Join([]string{
		hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32],
	}, "-")
}
