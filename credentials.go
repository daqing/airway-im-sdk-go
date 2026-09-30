package airwayim

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// CredentialPrefix starts every Airway-signed credential.
	CredentialPrefix = "im1."

	// DefaultTTLSeconds is the default lifetime applied by SignCredential
	// (and the backend's minting endpoint) when no TTL is given.
	DefaultTTLSeconds = 86400
)

// CredentialClaims is the decoded claims segment of an
// im1.<payload>.<signature> credential. Unknown fields are ignored;
// IssuedAt/ExpiresAt are unix seconds.
type CredentialClaims struct {
	UUID         string `json:"uuid"`
	Name         string `json:"name"`
	Nickname     string `json:"nickname,omitempty"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	TokenVersion int    `json:"token_version,omitempty"`
	IssuedAt     int64  `json:"iat,omitempty"`
	ExpiresAt    int64  `json:"exp,omitempty"`
}

// SignOptions carries the optional SignCredential inputs.
type SignOptions struct {
	Nickname     string
	AvatarURL    string
	TokenVersion int
	// TTLSeconds controls the exp claim: 0 applies DefaultTTLSeconds
	// (86400), a negative value omits exp entirely (service credentials
	// only — a credential without expiry never expires).
	TTLSeconds int
	// Now overrides the clock; the zero value uses time.Now().
	Now time.Time
}

// SignCredential signs (uuid, name) with the shared IM_AUTH_SECRET into an
// im1.<payload>.<sig> HMAC-SHA256 credential. Optional display fields are
// write-only-when-present: passing empty strings never clobbers a richer
// profile stored earlier in the IM users table.
//
// Only use it when your backend is trusted with IM_AUTH_SECRET (that is
// reserved for the Airway project's own side); third-party platform
// backends never hold that secret and must use InternalClient /
// MintCredential instead.
func SignCredential(secret, uuid, name string, opts *SignOptions) (string, error) {
	if opts == nil {
		opts = &SignOptions{}
	}
	if err := validateLength(uuid, 1, 64, "uuid"); err != nil {
		return "", err
	}
	if err := validateLength(name, 1, 64, "name"); err != nil {
		return "", err
	}
	if opts.Nickname != "" {
		if err := validateLength(opts.Nickname, 1, 64, "nickname"); err != nil {
			return "", err
		}
	}
	if opts.AvatarURL != "" {
		if err := validateLength(opts.AvatarURL, 1, 2048, "avatarUrl"); err != nil {
			return "", err
		}
	}
	if opts.TokenVersion < 0 {
		return "", &IMError{Code: -1, Status: 0, Message: "tokenVersion must be >= 1"}
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	issuedAt := now.Unix()
	claims := CredentialClaims{
		UUID:         uuid,
		Name:         name,
		Nickname:     opts.Nickname,
		AvatarURL:    opts.AvatarURL,
		TokenVersion: opts.TokenVersion,
		IssuedAt:     issuedAt,
	}
	switch {
	case opts.TTLSeconds == 0:
		claims.ExpiresAt = issuedAt + DefaultTTLSeconds
	case opts.TTLSeconds > 0:
		claims.ExpiresAt = issuedAt + int64(opts.TTLSeconds)
		// negative: omit exp entirely
	}

	// The signature covers the exact bytes, so the claims JSON must keep
	// the reference key order (uuid, name, nickname, avatar_url,
	// token_version, iat, exp) — the struct above defines it, and HTML
	// escaping stays off to match the Node reference encoder.
	payloadJSON, err := marshalJSONNoEscape(claims)
	if err != nil {
		return "", &IMError{Code: -1, Status: 0, Message: fmt.Sprintf("encode claims: %v", err)}
	}
	payload := base64URL(payloadJSON)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(CredentialPrefix + payload))
	signature := base64URL(mac.Sum(nil))
	return CredentialPrefix + payload + "." + signature, nil
}

// DecodeCredential returns the claims segment of a credential. It returns
// an *IMError (code -1) on malformed input.
func DecodeCredential(credential string) (CredentialClaims, error) {
	parts, err := credentialSegments(credential)
	if err != nil {
		return CredentialClaims{}, err
	}
	data, err := dataFromBase64URL(parts[1])
	if err != nil {
		return CredentialClaims{}, &IMError{Code: -1, Status: 0, Message: "malformed credential: invalid payload"}
	}
	var claims CredentialClaims
	if err := json.Unmarshal(data, &claims); err != nil {
		return CredentialClaims{}, &IMError{Code: -1, Status: 0, Message: "malformed credential: invalid payload"}
	}
	return claims, nil
}

// VerifyCredentialSignature recomputes the HMAC over the received payload
// and compares in constant time. It never treats malformed input as an
// error, it returns false instead.
func VerifyCredentialSignature(credential, secret string) bool {
	parts, err := credentialSegments(credential)
	if err != nil {
		return false
	}
	signature, err := dataFromBase64URL(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)
	return len(signature) == len(expected) &&
		subtle.ConstantTimeCompare(signature, expected) == 1
}

// IsCredentialExpired reports whether exp has passed; a credential without
// exp never expires here (the backend treats it the same way). Malformed
// input reports not expired.
func IsCredentialExpired(credential string, now time.Time) bool {
	claims, err := DecodeCredential(credential)
	if err != nil {
		return false
	}
	return claims.IsExpired(now)
}

// IsExpired reports whether exp has passed; zero means the credential has
// no expiry claim.
func (c CredentialClaims) IsExpired(now time.Time) bool {
	if c.ExpiresAt == 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return c.ExpiresAt <= now.Unix()
}

// ---- Plumbing ----

// marshalJSONNoEscape encodes v without HTML escaping and without the
// trailing newline json.Encoder appends.
func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// credentialSegments splits into exactly im1.<payload>.<signature>.
func credentialSegments(credential string) ([]string, error) {
	parts := strings.Split(credential, ".")
	if len(parts) != 3 || parts[0] != "im1" {
		return nil, &IMError{
			Code:    -1,
			Status:  0,
			Message: "malformed credential: expected im1.<payload>.<signature>",
		}
	}
	return parts, nil
}

func base64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func dataFromBase64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func validateLength(value string, min, max int, field string) error {
	if len(value) < min || len(value) > max {
		return &IMError{Code: -1, Status: 0, Message: fmt.Sprintf("%s must be %d-%d characters", field, min, max)}
	}
	return nil
}
