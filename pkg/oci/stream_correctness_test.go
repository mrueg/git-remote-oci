package oci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// spyCloser records whether the underlying stream was closed.
type spyCloser struct {
	io.Reader
	closed bool
}

func (s *spyCloser) Close() error {
	s.closed = true
	return nil
}

// TestDecompressStreamClosesUnderlyingStream pins that closing the wrapper
// closes what it wraps, for every media type it handles.
//
// Note on scope: an earlier iteration of this change claimed the decompressor
// itself leaked goroutines. It does not - zstd.NewReader without
// WithDecoderConcurrency decodes synchronously, and 20 un-closed decoders were
// measured to add exactly zero goroutines. Closing the decompressor is still
// the correct thing to do (it releases its buffers deterministically and
// surfaces any close error rather than discarding it), but it is hygiene, not
// a leak fix.
func TestDecompressStreamClosesUnderlyingStream(t *testing.T) {
	payload := bytes.Repeat([]byte("git packfile payload "), 64)

	for _, mode := range []struct{ name, mediaType string }{
		{"zstd", MediaTypeGitPackfileZstd},
		{"gzip", MediaTypeGitPackfileGzip},
		{"none", MediaTypeGitPackfile},
	} {
		t.Run(mode.name, func(t *testing.T) {
			compressed, _ := compressForTest(t, payload, mode.name)

			spy := &spyCloser{Reader: bytes.NewReader(compressed)}
			rc, err := DecompressStream(spy, mode.mediaType)
			if err != nil {
				t.Fatalf("DecompressStream: %v", err)
			}

			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("payload round-trip mismatch: %d vs %d bytes", len(got), len(payload))
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !spy.closed {
				t.Error("closing the decompressing reader did not close the underlying stream")
			}
		})
	}
}

// TestRoundTripDoesNotMutateRequest pins the http.RoundTripper contract: the
// caller owns the request, and the transport must not modify it. The no-cache
// headers for the index tags used to be set on the caller's request directly.
func TestRoundTripDoesNotMutateRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		ts.URL+"/v2/x/manifests/"+TagRefIndex, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := req.Header.Get("Cache-Control"); got != "" {
		t.Errorf("RoundTrip mutated the caller's request: Cache-Control=%q", got)
	}
}

// TestRoundTripReturnsOpenBody covers a case where the transport handed back a
// response whose body it had already closed: a retriable status on a request
// whose body cannot be replayed.
func TestRoundTripReturnsOpenBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = time.Millisecond

	// A body with no GetBody cannot be rewound, so the transport must give up
	// after the first attempt rather than retry.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL,
		io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = nil

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body was already closed: %v", err)
	}
	if string(body) != "unavailable" {
		t.Errorf("body = %q, want %q", body, "unavailable")
	}
}

// TestIsRetriableErrorClassification: permanent faults must not be retried.
func TestIsRetriableErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"malformed url", errors.New("parse \"://\": missing protocol scheme"), false},
		{"unsupported scheme", errors.New("unsupported protocol scheme \"gopher\""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetriableError(tt.err); got != tt.want {
				t.Errorf("isRetriableError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
