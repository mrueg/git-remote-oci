package oci

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Duration
	}{
		{"empty string", "", 0},
		{"integer seconds", "10", 10 * time.Second},
		{"zero seconds", "0", 0},
		{"invalid string", "invalid-seconds", 0},
		{"future HTTP date", time.Now().UTC().Add(30 * time.Second).Format(http.TimeFormat), 29 * time.Second}, // approximate
		{"past HTTP date", "Sun, 06 Nov 1994 08:49:37 GMT", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.input)
			if tt.name == "future HTTP date" {
				if got < 20*time.Second || got > 35*time.Second {
					t.Errorf("parseRetryAfter(%q) = %v, expected approx 30s", tt.input, got)
				}
			} else if got != tt.expected {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestRetryTransport429WithRetryAfter(t *testing.T) {
	var attempts atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attempts.Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 10 * time.Millisecond
	rt.maxInterval = 2 * time.Second

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	start := time.Now()
	resp, err := rt.RoundTrip(req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 OK, got %d", resp.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts.Load())
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("Expected at least 1s delay from Retry-After header, got %v", elapsed)
	}
}

func TestRetryTransportExponentialBackoff(t *testing.T) {
	var attempts atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attempts.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 20 * time.Millisecond
	rt.maxInterval = 100 * time.Millisecond

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 OK, got %d", resp.StatusCode)
	}
	if attempts.Load() != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryTransportContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rt := newRetryTransport(ts.Client().Transport)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	_, err = rt.RoundTrip(req)
	if err == nil {
		t.Fatal("Expected error on cancelled context, got nil")
	}
}

func TestRetryTransportBodyRewind(t *testing.T) {
	var attempts atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload-data" {
			t.Errorf("Expected payload-data, got %q", string(body))
		}
		count := attempts.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 10 * time.Millisecond

	bodyData := []byte("payload-data")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, bytes.NewReader(bodyData))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("Expected status 201 Created, got %d", resp.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts.Load())
	}
}

// TestRetryTransportDoesNotReplayANonRewindableBody is the other half of the
// rewind test: a body with no GetBody cannot be sent twice, and a transport
// that retried it anyway would send an empty second request -- which for a
// blob upload is a truncated blob the registry may well accept. So a 503 on
// such a request has to come back after the first attempt, as the failure it
// is, and never be retried.
func TestRetryTransportDoesNotReplayANonRewindableBody(t *testing.T) {
	var attempts atomic.Int32
	var bodies []string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = time.Millisecond

	// A streaming body: a pipe has no GetBody, and NewRequest cannot invent
	// one for it the way it does for a bytes.Reader.
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, "chunk-data")
		_ = pw.Close()
	}()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch, ts.URL, pr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if req.GetBody != nil {
		t.Fatal("test setup: a pipe body should not be rewindable")
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	// The 503 is the answer: handed back untouched for the caller to treat as
	// the error it is, rather than swallowed by a retry.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the registry's 503 passed through", resp.StatusCode)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("a request whose body cannot be replayed was sent %d times, want 1", got)
	}
	if bodies[0] != "chunk-data" {
		t.Errorf("the one attempt carried body %q, want %q", bodies[0], "chunk-data")
	}
}

// TestCalculateBackoffHonoursRetryAfter pins that a Retry-After is waited out
// on its own ceiling. It used to be clamped to the backoff's maxInterval,
// which left maxRetryAfter with nothing to do and retried into a registry
// that had said when it would be ready.
func TestCalculateBackoffHonoursRetryAfter(t *testing.T) {
	rt := newRetryTransport(nil)
	rt.maxInterval = 100 * time.Millisecond

	if got := rt.calculateBackoff(0, 30*time.Second); got != 30*time.Second {
		t.Errorf("Retry-After of 30s produced a wait of %v; it must be honoured past maxInterval", got)
	}
	if got := rt.calculateBackoff(0, 2*time.Hour); got != maxRetryAfter {
		t.Errorf("Retry-After of 2h produced a wait of %v, want the %v ceiling", got, maxRetryAfter)
	}
	if got := rt.calculateBackoff(5, 0); got > rt.maxInterval {
		t.Errorf("plain backoff of %v exceeds maxInterval %v", got, rt.maxInterval)
	}
}

// TestRoundTripLeavesTheCallerBodyAlone: replaying a body on retry must happen
// on the transport's own copy of the request. Reassigning Body on the caller's
// request is the same contract violation as setting headers on it.
func TestRoundTripLeavesTheCallerBodyAlone(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	rt := newRetryTransport(ts.Client().Transport)
	rt.initialInterval = 10 * time.Millisecond

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	original := req.Body

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if attempts.Load() != 2 {
		t.Fatalf("expected a retry, got %d attempts", attempts.Load())
	}
	if req.Body != original {
		t.Error("the transport replaced Body on the caller's request while retrying")
	}
}
