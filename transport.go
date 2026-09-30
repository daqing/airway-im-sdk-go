package airwayim

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// HTTPRequest is a single HTTP call the SDK core wants performed.
type HTTPRequest struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    []byte
}

// HTTPResponse is the transport-level result of one HTTP call.
type HTTPResponse struct {
	Status int
	Body   []byte
}

// HTTPTransport is the transport seam the SDK core talks to;
// NewHTTPTransport is the default. Implement it to route requests through
// your own stack (custom TLS handling, proxies, instrumentation).
type HTTPTransport interface {
	Send(ctx context.Context, request *HTTPRequest) (*HTTPResponse, error)
}

// NewHTTPTransport returns the default net/http transport with a
// per-request timeout. Transport failures surface as non-nil errors; the
// SDK maps them to *IMError with status 0 so callers can retry.
func NewHTTPTransport(timeout time.Duration) HTTPTransport {
	return &httpTransport{client: &http.Client{Timeout: timeout}}
}

type httpTransport struct {
	client *http.Client
}

func (t *httpTransport) Send(ctx context.Context, request *HTTPRequest) (*HTTPResponse, error) {
	var body io.Reader
	if request.Body != nil {
		body = bytes.NewReader(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, body)
	if err != nil {
		return nil, err
	}
	for name, value := range request.Headers {
		req.Header.Set(name, value)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &HTTPResponse{Status: resp.StatusCode, Body: data}, nil
}
