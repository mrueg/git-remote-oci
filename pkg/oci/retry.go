package oci

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

// retryTransport wraps an http.RoundTripper with rate-limit (HTTP 429 Retry-After)
// handling, exponential backoff, and jitter.
type retryTransport struct {
	base            http.RoundTripper
	maxRetries      int
	initialInterval time.Duration
	maxInterval     time.Duration
	// observe, when set, is shown each response. It is how the client learns
	// that its credentials were accepted at least once, which is what
	// separates "these credentials are wrong" from "these credentials stopped
	// working part-way through". It gets the whole response rather than the
	// status because *which* request succeeded matters: a 200 from the token
	// endpoint says nothing about access to the repository.
	observe func(resp *http.Response)
}

func newRetryTransport(base http.RoundTripper) *retryTransport {
	if base == nil {
		base = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   100,
			MaxConnsPerHost:       200,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			WriteBufferSize:       64 * 1024,
			ReadBufferSize:        64 * 1024,
		}
	}
	return &retryTransport{
		base:            base,
		maxRetries:      3,
		initialInterval: 50 * time.Millisecond,
		maxInterval:     5 * time.Second,
	}
}

// isMutableMetadataPath reports whether a request reads something that changes
// under us and must not be served from a cache.
//
// The indexes are the obvious case. Locks matter just as much and were missed:
// a stale read either sees a lock that has been released or misses one that is
// held, and either way the locking it feeds is worse than useless.
func isMutableMetadataPath(path string) bool {
	const manifests = "/manifests/"
	i := strings.LastIndex(path, manifests)
	if i < 0 {
		return false
	}
	ref := path[i+len(manifests):]
	return ref == TagRefIndex ||
		ref == TagOCIIndex ||
		ref == lfs.TagLFSLocks ||
		strings.HasPrefix(ref, LockTagPrefix)
}

// RoundTrip implements http.RoundTripper.
//
// Per the RoundTripper contract it must not modify the supplied request, must
// not return both a nil response and a nil error, and must never hand back a
// response whose body it has already closed.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before touching anything: the caller owns req, and mutating it is a
	// contract violation that also races other users of the same request. This
	// used to clone only when headers were added, but the retry loop below
	// also reassigns Body on each replay, which is the same violation.
	req = req.Clone(req.Context())
	if req.URL != nil && isMutableMetadataPath(req.URL.Path) {
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
	}

	// A request can only be retried if its body can be replayed. A nil body is
	// trivially replayable; otherwise GetBody must be available.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	var lastErr error

	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}

		// Rewind the body before replaying. A nil body needs nothing; anything
		// else needs GetBody, whose absence is what `replayable` records.
		if attempt > 0 && req.GetBody != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				return nil, fmt.Errorf("cannot replay request body for retry: %w", bodyErr)
			}
			req.Body = body
		}

		resp, err := t.base.RoundTrip(req)
		if err != nil {
			lastErr = err
			if !replayable || !isRetriableError(err) || attempt == t.maxRetries {
				return nil, err
			}
			if waitErr := t.wait(req.Context(), t.calculateBackoff(attempt, 0)); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if t.observe != nil {
			t.observe(resp)
		}

		if !isRetriableStatus(resp.StatusCode) || attempt == t.maxRetries || !replayable {
			// Hand the response back untouched, body still open.
			return resp, nil
		}

		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		// Drain a little before closing so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		lastErr = fmt.Errorf("registry returned %s", resp.Status)

		if waitErr := t.wait(req.Context(), t.calculateBackoff(attempt, retryAfter)); waitErr != nil {
			return nil, waitErr
		}
	}

	// Unreachable in practice: every path above either returns or continues,
	// and the final attempt always returns. Guard anyway so we never return
	// (nil, nil).
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed after %d attempts", t.maxRetries+1)
	}
	return nil, lastErr
}

// wait sleeps for d unless ctx is cancelled first.
func (t *retryTransport) wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *retryTransport) calculateBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		// A Retry-After is the registry saying when it will serve us again, so
		// it is honoured on its own ceiling rather than the backoff's: clamping
		// it to maxInterval, as this used to, retried a 429 at five seconds
		// when the registry had asked for thirty and got another 429 for it.
		return min(retryAfter, maxRetryAfter)
	}

	backoffFloat := float64(t.initialInterval) * math.Pow(2, float64(attempt))
	backoff := time.Duration(backoffFloat)
	if backoff > t.maxInterval {
		backoff = t.maxInterval
	}

	// Jitter only spreads retries across concurrent clients; it is not a
	// security parameter, so a non-cryptographic source is appropriate.
	jitter := time.Duration(rand.Float64() * float64(backoff) * 0.25) //nolint:gosec // G404: retry jitter, not security-sensitive
	wait := backoff + jitter
	if wait > t.maxInterval {
		wait = t.maxInterval
	}
	return wait
}

func isRetriableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}

// isRetriableError reports whether err is worth another attempt.
//
// The previous version returned true for *every* non-net error, so a bad URL,
// an unsupported protocol or a TLS verification failure was retried three
// times before surfacing, and false for non-timeout net.Errors such as
// connection-refused, which is exactly the case worth retrying. Retry
// transient transport conditions only.
func isRetriableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A failed TLS handshake or an untrusted certificate will fail identically
	// on every attempt.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		// Timeouts and connection-level failures are transient.
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// Anything else (malformed URL, unsupported scheme, auth plumbing) is a
	// programming or configuration fault and will not fix itself.
	return false
}

// maxRetryAfter bounds the delay a registry can request. The value is
// registry-controlled, so it is both clamped and overflow-checked here rather
// than trusting the caller to notice.
//
// A minute, not an hour: a push that sits silent for an hour because a header
// said so is indistinguishable from one that hung, and a registry asking for
// longer than this is better reported than waited for.
const maxRetryAfter = 60 * time.Second

func parseRetryAfter(headerVal string) time.Duration {
	headerVal = strings.TrimSpace(headerVal)
	if headerVal == "" {
		return 0
	}

	if seconds, err := strconv.ParseInt(headerVal, 10, 64); err == nil && seconds > 0 {
		// seconds * time.Second overflows int64 nanoseconds above roughly
		// 9.2e9 seconds and wraps negative, so compare before multiplying.
		if seconds > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}

	dateFormats := []string{
		http.TimeFormat,
		time.RFC850,
		time.ANSIC,
	}
	for _, fmtStr := range dateFormats {
		if parsedTime, err := time.Parse(fmtStr, headerVal); err == nil {
			wait := time.Until(parsedTime)
			if wait > maxRetryAfter {
				return maxRetryAfter
			}
			if wait > 0 {
				return wait
			}
			return 0
		}
	}

	return 0
}
