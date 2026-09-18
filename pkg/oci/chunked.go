package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Resumable blob upload.
//
// A packfile goes to the registry as one blob in one request. When that request
// fails at 90% of a four-gigabyte push -- a dropped connection, a proxy timeout,
// a token expiring mid-stream -- everything already sent is thrown away and the
// next attempt starts from zero. On a link bad enough to break a long upload,
// retrying the whole upload is not obviously going to finish either.
//
// The distribution spec has an answer: POST to open a session, PATCH each chunk
// with a Content-Range, PUT to close it. A failed chunk costs a chunk, and the
// registry will say how much it already has, so a retry resumes rather than
// restarts.
//
// This is deliberately not the default for everything. Chunking a small blob
// adds round trips to buy resumability that is worth nothing -- there is
// nothing to resume. So it applies above a threshold, and only to blobs staged
// on disk, which can be re-read from an offset.

const (
	// DefaultChunkSize is how much is sent per request once chunking kicks in.
	//
	// Large enough that the extra round trips do not matter next to the
	// transfer, small enough that losing one is not painful. It is also the
	// threshold: a blob that fits in a single chunk is sent the old way,
	// because a one-chunk "resumable" upload resumes at zero.
	DefaultChunkSize int64 = 32 << 20

	// chunkUploadAttempts is how many times one chunk is retried before the
	// push fails. Each retry re-asks the registry what it holds, so these are
	// attempts at making progress rather than attempts at the same thing.
	chunkUploadAttempts = 3
)

// ErrChunkedUnsupported means the registry would not take a chunked upload.
//
// It is not a push failure. The caller falls back to sending the blob whole,
// which is what it would have done anyway.
var ErrChunkedUnsupported = errors.New("the registry does not support chunked blob uploads")

// pushBlobResumable uploads a disk-staged blob in chunks, resuming from
// whatever the registry reports it already holds when a chunk fails.
//
// It returns ErrChunkedUnsupported before sending any content if the registry
// will not open a chunked session, so the caller can fall back without having
// wasted the transfer.
func (c *Client) pushBlobResumable(ctx context.Context, desc ocispec.Descriptor, file *os.File) error {
	location, err := c.startBlobUpload(ctx)
	if err != nil {
		return err
	}

	// An upload that stops part-way leaves the registry holding whatever was
	// accepted, against a session it has no reason to believe is finished.
	// Distribution keeps that until its own timeout, which is measured in
	// hours and is storage nobody can see or reclaim. The spec provides a
	// DELETE for exactly this, so a push that gives up says so.
	//
	// Only on the failure paths: a completed upload's session is closed by the
	// PUT, and cancelling it afterwards would ask the registry to discard the
	// blob it just accepted.
	done := false
	defer func() {
		if !done {
			c.cancelBlobUpload(ctx, location)
		}
	}()

	chunk := c.UploadChunkSize
	var offset int64
	// Whether the registry has accepted anything at all. Tracked explicitly
	// rather than inferred from offset, because offset can be moved by the
	// progress probe below and then no longer says what was *accepted*.
	var progressed bool

	for offset < desc.Size {
		end := min(offset+chunk, desc.Size)

		var lastErr error
		for attempt := 0; attempt < chunkUploadAttempts; attempt++ {
			// Ask what the registry actually has rather than assuming the
			// failed chunk landed nowhere. A PATCH can fail after the bytes
			// were accepted, and re-sending from the wrong offset produces a
			// corrupt blob that only fails at the closing PUT.
			//
			if attempt > 0 {
				resumed, next, probeErr := c.blobUploadOffset(ctx, location)
				if probeErr != nil {
					lastErr = probeErr
					continue
				}
				// A session that has received nothing reports `Range: 0-0` on
				// several registries -- the POST that opens it does so here --
				// which is also how it would report holding a single byte. No
				// chunk is one byte, so before anything has been accepted that
				// spelling means empty. Reading it as "one byte in" would
				// re-send from offset 1 and build a blob off by one, which
				// nothing notices until the closing digest check.
				if !progressed && resumed <= 1 {
					resumed = 0
				}
				offset, location = resumed, next
				if offset >= desc.Size {
					// The registry already holds the whole blob: the last
					// PATCH landed and only its response was lost. That is
					// not a failure to carry out of the loop, and doing so
					// used to fail a push the registry had in fact accepted.
					lastErr = nil
					end = desc.Size
					break
				}
				end = min(offset+chunk, desc.Size)
			}

			next, patchErr := c.patchChunk(ctx, location, file, offset, end)
			if patchErr == nil {
				location = next
				lastErr = nil
				progressed = true
				break
			}
			lastErr = patchErr
		}
		if lastErr != nil {
			if !progressed {
				// Nothing was ever accepted, so this registry is not going to
				// take a chunked upload however it chose to say so -- a 500 on
				// a Content-Range it dislikes is as final as a 405, and only
				// the second could have been recognised in advance. Report it as unsupported so
				// the caller falls back: chunking is an optimisation layered
				// over a path that worked, and it must never turn a push that
				// used to succeed into one that fails.
				return fmt.Errorf("%w: %w", ErrChunkedUnsupported, lastErr)
			}
			// Past the first chunk the session demonstrably works, so this is a
			// transfer failure rather than an incompatibility. Re-sending the
			// whole blob would waste everything already accepted and is no more
			// likely to finish.
			return fmt.Errorf("failed to upload bytes %d-%d of %d: %w", offset, end-1, desc.Size, lastErr)
		}
		offset = end
	}

	if err := c.finishBlobUpload(ctx, location, desc); err != nil {
		return err
	}
	done = true
	return nil
}

// cancelBlobUpload asks the registry to discard an unfinished upload.
//
// Best effort, and deliberately silent: this runs on a path that is already
// returning an error, and a failure to tidy up must not replace the reason the
// push failed with a complaint about the tidying. Registries that do not
// implement the verb answer 404 or 405, which is the same outcome as not
// having asked.
//
// On a context of its own, because the common reason to be here is that the
// original one was cancelled -- and a cancelled context cannot make the request
// that releases the resource the cancellation stranded.
func (c *Client) cancelBlobUpload(ctx context.Context, location string) {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cancelCtx, http.MethodDelete, location, nil)
	if err != nil {
		return
	}
	resp, err := c.do(req)
	if err != nil {
		return
	}
	drain(resp)
}

// startBlobUpload opens an upload session and returns where to send to.
func (c *Client) startBlobUpload(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryURL("blobs/uploads/"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Length", "0")

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusAccepted {
		// 201 means the registry completed it as a monolithic upload of
		// nothing, and anything else means it did not understand the request.
		// Either way there is no session to PATCH into.
		return "", fmt.Errorf("%w (POST returned %s)", ErrChunkedUnsupported, resp.Status)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("%w (no Location on the upload session)", ErrChunkedUnsupported)
	}
	return c.resolveLocation(location)
}

// patchChunk sends file[offset:end) and returns where the next chunk goes.
func (c *Client) patchChunk(ctx context.Context, location string, file *os.File, offset, end int64) (string, error) {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", fmt.Errorf("failed to seek the staged blob to %d: %w", offset, err)
	}
	length := end - offset

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, location, io.LimitReader(file, length))
	if err != nil {
		return "", err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	// Inclusive on both ends, per the distribution spec -- not the half-open
	// range Go's io package uses, and not HTTP's "bytes " prefixed form.
	req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", offset, end-1))

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusAccepted {
		// Only on the first chunk. A registry that takes one PATCH and then
		// refuses the next has a problem with this upload, not with chunking,
		// and retrying is the right response to that.
		if offset == 0 && isChunkingRefusal(resp.StatusCode) {
			return "", fmt.Errorf("%w (PATCH returned %s)", ErrChunkedUnsupported, resp.Status)
		}
		return "", fmt.Errorf("PATCH returned %s", resp.Status)
	}
	if next := resp.Header.Get("Location"); next != "" {
		return c.resolveLocation(next)
	}
	// A registry that returns no new Location expects the session URL reused.
	return location, nil
}

// isChunkingRefusal reports whether a status means "this registry does not do
// chunked uploads" rather than "this upload went wrong".
//
// Registries disagree about how to say it: some reject the verb, some report
// the session URL as missing, some call it a bad request. All three mean the
// same thing to a caller that has a monolithic path to fall back to.
func isChunkingRefusal(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusBadRequest,
		http.StatusNotImplemented, http.StatusUnsupportedMediaType:
		return true
	}
	return false
}

// blobUploadOffset asks how much of the blob the registry already holds.
//
// The Range header is inclusive, so "0-1023" means 1024 bytes are in and the
// next chunk starts at 1024. An absent Range means nothing has landed.
func (c *Client) blobUploadOffset(ctx context.Context, location string) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return 0, location, err
	}

	resp, err := c.do(req)
	if err != nil {
		return 0, location, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, location, fmt.Errorf("could not read the upload's progress: %s", resp.Status)
	}
	next := location
	if l := resp.Header.Get("Location"); l != "" {
		if resolved, resolveErr := c.resolveLocation(l); resolveErr == nil {
			next = resolved
		}
	}

	rangeHeader := resp.Header.Get("Range")
	if rangeHeader == "" {
		return 0, next, nil
	}
	_, last, found := strings.Cut(strings.TrimPrefix(rangeHeader, "bytes="), "-")
	if !found {
		return 0, next, nil
	}
	end, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if err != nil || end < 0 {
		return 0, next, nil
	}
	return end + 1, next, nil
}

// finishBlobUpload closes the session, which is where the registry verifies the
// digest against everything it was sent.
func (c *Client) finishBlobUpload(ctx context.Context, location string, desc ocispec.Descriptor) error {
	final, err := url.Parse(location)
	if err != nil {
		return err
	}
	query := final.Query()
	query.Set("digest", desc.Digest.String())
	final.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, final.String(), nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("failed to complete the upload of %s: %s", desc.Digest, resp.Status)
	}
	return nil
}

// registryURL builds a /v2/<repository>/<path> URL for this client's repo.
func (c *Client) registryURL(path string) string {
	scheme := "https"
	if c.Repo.PlainHTTP {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v2/%s/%s", scheme, c.Repo.Reference.Host(), c.Repo.Reference.Repository, path)
}

// resolveLocation turns a Location header into an absolute URL.
//
// Registries return both forms -- an absolute URL to a separate upload host, or
// a path relative to the registry -- and the spec permits either.
func (c *Client) resolveLocation(location string) (string, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("the registry returned an unusable upload location %q: %w", location, err)
	}
	if parsed.IsAbs() {
		return parsed.String(), nil
	}
	base, err := url.Parse(c.registryURL(""))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(parsed).String(), nil
}

// do sends a request through the repository's authenticated client, so chunked
// uploads get the same credentials, token refresh and transport as everything
// else.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	client := c.Repo.Client
	if client == nil {
		return http.DefaultClient.Do(req)
	}
	return client.Do(req)
}

// drain closes a response body, reading what is left so the connection can be
// reused rather than torn down between chunks.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// pushPackfileBlob uploads a packfile, resumably when that is possible and
// worth it.
//
// Chunking needs two things: a body that can be re-read from an offset, and
// enough bytes for restarting to hurt. The packfile is staged in a file (see
// spool.go) so the first holds whenever the caller passes that file through;
// the second is what UploadChunkSize decides. Anything else -- a small pack, a
// reader that is not a file, chunking turned off -- goes as one request, which
// is what every push did before.
//
// A registry that will not take a chunked upload is not an error. The upload
// falls back to a single request, which is what every push did before this
// existed -- chunking must never turn a push that used to succeed into one that
// fails, so any refusal on the very first chunk becomes a fallback rather than
// a failure, whatever status the registry chose to express it with.
func (c *Client) pushPackfileBlob(ctx context.Context, desc ocispec.Descriptor, content io.Reader) error {
	file, seekable := content.(*os.File)
	if seekable && c.UploadChunkSize > 0 && desc.Size > c.UploadChunkSize {
		err := c.pushBlobResumable(ctx, desc, file)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrChunkedUnsupported) {
			return err
		}
		// Rewind: the fallback reads the same file from the beginning, and a
		// failed session may have left it anywhere.
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return fmt.Errorf("failed to rewind the staged packfile after falling back: %w", seekErr)
		}
	}
	return c.Repo.Push(ctx, desc, content)
}
