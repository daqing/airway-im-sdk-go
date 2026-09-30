package airwayim

import (
	"strings"
	"testing"
	"time"
)

func TestSignCredentialMatchesNodeVector(t *testing.T) {
	// Cross-language regression vector generated with the Node.js
	// reference implementation (deps/im/docs/design/identity.md §4.2):
	// claims {uuid: "user-42", name: "alice", iat: 1700000000, exp: 1700086400}.
	const nodeVector = "im1.eyJ1dWlkIjoidXNlci00MiIsIm5hbWUiOiJhbGljZSIsImlhdCI6MTcwMDAwMDAwMCwiZXhwIjoxNzAwMDg2NDAwfQ.w_i4UIKCVjBY0XwnDdeRsnOCHoqgxxl1rDAESDh7VeU"
	credential, err := SignCredential("test-secret", "user-42", "alice", &SignOptions{
		TTLSeconds: 86400,
		Now:        time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if credential != nodeVector {
		t.Fatalf("credential mismatch:\n got  %s\n want %s", credential, nodeVector)
	}
}

func TestSignCredentialIncludesOptionalFields(t *testing.T) {
	credential, err := SignCredential("test-secret", "u1", "bob", &SignOptions{
		Nickname:     "Bob",
		AvatarURL:    "https://example.com/bob.png",
		TokenVersion: 3,
		TTLSeconds:   60,
		Now:          time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := DecodeCredential(credential)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := CredentialClaims{
		UUID:         "u1",
		Name:         "bob",
		Nickname:     "Bob",
		AvatarURL:    "https://example.com/bob.png",
		TokenVersion: 3,
		IssuedAt:     1_700_000_000,
		ExpiresAt:    1_700_000_060,
	}
	if claims != want {
		t.Fatalf("claims mismatch:\n got  %+v\n want %+v", claims, want)
	}
}

func TestSignCredentialOmitsEmptyFields(t *testing.T) {
	credential, err := SignCredential("test-secret", "u1", "bob", &SignOptions{
		Now: time.Unix(0, 0),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := DecodeCredential(credential)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if claims.UUID != "u1" || claims.Name != "bob" || claims.Nickname != "" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.IssuedAt != 0 {
		t.Fatalf("iat = %d, want 0", claims.IssuedAt)
	}
	if claims.ExpiresAt != 86400 {
		t.Fatalf("exp = %d, want 86400 (default TTL)", claims.ExpiresAt)
	}
}

func TestSignCredentialOmitsExpWhenTTLNegative(t *testing.T) {
	credential, err := SignCredential("test-secret", "u1", "bob", &SignOptions{
		TTLSeconds: -1,
		Now:        time.Unix(0, 0),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := DecodeCredential(credential)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if claims.ExpiresAt != 0 {
		t.Fatalf("exp = %d, want omitted", claims.ExpiresAt)
	}
	if !VerifyCredentialSignature(credential, "test-secret") {
		t.Fatal("signature should verify")
	}
}

func TestSignCredentialRejectsOutOfRangeFields(t *testing.T) {
	if _, err := SignCredential("test-secret", strings.Repeat("x", 65), "bob", nil); err == nil {
		t.Fatal("expected error for over-long uuid")
	}
	if _, err := SignCredential("test-secret", "u1", "", nil); err == nil {
		t.Fatal("expected error for empty name")
	}
	if _, err := SignCredential("test-secret", "u1", "bob", &SignOptions{Nickname: strings.Repeat("x", 65)}); err == nil {
		t.Fatal("expected error for over-long nickname")
	}
	if _, err := SignCredential("test-secret", "u1", "bob", &SignOptions{AvatarURL: strings.Repeat("x", 2049)}); err == nil {
		t.Fatal("expected error for over-long avatarUrl")
	}
	if _, err := SignCredential("test-secret", "u1", "bob", &SignOptions{TokenVersion: -5}); err == nil {
		t.Fatal("expected error for negative tokenVersion")
	}
}

func TestDecodeCredentialRoundTrip(t *testing.T) {
	const nodeVector = "im1.eyJ1dWlkIjoidXNlci00MiIsIm5hbWUiOiJhbGljZSIsImlhdCI6MTcwMDAwMDAwMCwiZXhwIjoxNzAwMDg2NDAwfQ.w_i4UIKCVjBY0XwnDdeRsnOCHoqgxxl1rDAESDh7VeU"
	claims, err := DecodeCredential(nodeVector)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if claims.UUID != "user-42" || claims.Name != "alice" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.IssuedAt != 1_700_000_000 || claims.ExpiresAt != 1_700_086_400 {
		t.Fatalf("unexpected timestamps: %+v", claims)
	}
}

func TestDecodeCredentialRejectsMalformedInput(t *testing.T) {
	for _, bad := range []string{"", "not-a-credential", "im2.aaa.bbb", "im1.aaa", "im1.aaa.bbb.ccc"} {
		if _, err := DecodeCredential(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestVerifyCredentialSignature(t *testing.T) {
	const nodeVector = "im1.eyJ1dWlkIjoidXNlci00MiIsIm5hbWUiOiJhbGljZSIsImlhdCI6MTcwMDAwMDAwMCwiZXhwIjoxNzAwMDg2NDAwfQ.w_i4UIKCVjBY0XwnDdeRsnOCHoqgxxl1rDAESDh7VeU"
	if !VerifyCredentialSignature(nodeVector, "test-secret") {
		t.Fatal("valid signature must verify")
	}
	if VerifyCredentialSignature(nodeVector, "wrong-secret") {
		t.Fatal("wrong secret must not verify")
	}
	parts := strings.SplitN(nodeVector, ".", 3)
	tampered := parts[1]
	if strings.HasPrefix(tampered, "A") {
		tampered = "B" + tampered[1:]
	} else {
		tampered = "A" + tampered[1:]
	}
	if VerifyCredentialSignature("im1."+tampered+"."+parts[2], "test-secret") {
		t.Fatal("tampered payload must not verify")
	}
	if VerifyCredentialSignature("", "test-secret") || VerifyCredentialSignature("garbage", "test-secret") {
		t.Fatal("malformed input must not verify")
	}
}

func TestCredentialExpiry(t *testing.T) {
	const nodeVector = "im1.eyJ1dWlkIjoidXNlci00MiIsIm5hbWUiOiJhbGljZSIsImlhdCI6MTcwMDAwMDAwMCwiZXhwIjoxNzAwMDg2NDAwfQ.w_i4UIKCVjBY0XwnDdeRsnOCHoqgxxl1rDAESDh7VeU"
	if IsCredentialExpired(nodeVector, time.Unix(1_700_086_399, 0)) {
		t.Fatal("credential must not be expired one second before exp")
	}
	if !IsCredentialExpired(nodeVector, time.Unix(1_700_086_400, 0)) {
		t.Fatal("credential must be expired at exp")
	}
	never, err := SignCredential("test-secret", "u1", "bob", &SignOptions{
		TTLSeconds: -1,
		Now:        time.Unix(0, 0),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if IsCredentialExpired(never, time.Unix(4_102_444_800, 0)) {
		t.Fatal("credential without exp must never expire")
	}
}
