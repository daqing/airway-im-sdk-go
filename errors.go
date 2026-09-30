package airwayim

import "fmt"

// Business codes from the backend's {code, data, message} envelope
// (deps/im/docs/api/openapi.md).
const (
	CodeInternalError        = 10000
	CodeInvalidCredential    = 10001
	CodeInvalidRequest       = 10003
	CodePermissionDenied     = 10005
	CodeConversationNotFound = 11001
	CodeIdempotencyKeyReused = 11002
)

// IMError is the error type every SDK call returns: Code is the envelope
// business code (10000-11002, or -1 when the body was not an envelope or
// the failure happened before a response arrived), Status the HTTP status
// code (0 for transport failures: network error, timeout).
type IMError struct {
	Code    int
	Status  int
	Message string
}

func (e *IMError) Error() string {
	return fmt.Sprintf("IMError(code: %d, status: %d, message: %s)", e.Code, e.Status, e.Message)
}

// IsAuthError reports whether the credential is missing, malformed,
// revoked, or expired.
func (e *IMError) IsAuthError() bool {
	return e.Code == CodeInvalidCredential || e.Status == 401
}

// imErr wraps err as a transport-level IMError (no HTTP status) when it is
// not already an *IMError.
func imErr(err error) *IMError {
	if e, ok := err.(*IMError); ok {
		return e
	}
	return &IMError{Code: -1, Status: 0, Message: err.Error()}
}
