package oci

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Resumable uploads are only worth having if a failure mid-transfer costs a
// chunk instead of the whole push, so that is what these test: not that a
// chunked upload works, but that an interrupted one resumes from where the
// registry says it got to. The interesting failure is the silent one — a retry
// that re-sends from the wrong offset produces a blob that is wrong rather than
// short, and the only thing that notices is the digest check on the closing
// PUT.

// chunkedRegistry implements just enough of the distribution upload protocol to
// exercise the client: POST to open, PATCH per chunk, GET for progress, PUT to
// close and verify.
type chunkedRegistry struct {
	mu sync.Mutex
	// content accumulates what has been PATCHed, in order.
	content []byte
	// patches records the Content-Range of every PATCH that was accepted.
	patches []string
	// failNextPatch, when set, rejects the next PATCH. The bytes are kept, so
	// a client that retries from zero corrupts the blob rather than merely
	// wasting the chunk — which is the mistake worth catching.
	failNextPatch bool
	// keepBytesOnFailure decides whether the rejected chunk is retained.
	keepBytesOnFailure bool
	// refusePatch models a registry that takes monolithic uploads but not
	// chunked ones: the session opens, and PATCH is rejected. patchStatus
	// chooses how it says so; registries are not consistent about it.
	refusePatch bool
	patchStatus int
	// completed is the digest the closing PUT was given.
	completed string
	// monolithic records a PUT that carried the whole body itself.
	monolithic bool
	// cancelled records a DELETE against the upload session.
	cancelled bool
	// failEveryPatch rejects every chunk, to drive an upload that gives up
	// after making progress.
	failEveryPatch bool
	// loseFinalResponse keeps the bytes of the PATCH that completes a blob
	// of blobSize bytes but answers it with a 500: the registry has
	// everything, and the client was never told.
	loseFinalResponse bool
	blobSize          int
}

func (r *chunkedRegistry) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", "/v2/test/blobs/uploads/session")
			w.Header().Set("Range", "0-0")
			w.WriteHeader(http.StatusAccepted)

		case req.Method == http.MethodPatch:
			body := readAllOrFail(t, req)
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.refusePatch {
				status := r.patchStatus
				if status == 0 {
					status = http.StatusMethodNotAllowed
				}
				w.WriteHeader(status)
				return
			}
			if r.failEveryPatch && len(r.content) > 0 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if r.failNextPatch {
				r.failNextPatch = false
				if r.keepBytesOnFailure {
					r.content = append(r.content, body...)
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			r.content = append(r.content, body...)
			if r.loseFinalResponse && len(r.content) == r.blobSize {
				r.loseFinalResponse = false
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			r.patches = append(r.patches, req.Header.Get("Content-Range"))
			w.Header().Set("Location", "/v2/test/blobs/uploads/session")
			w.Header().Set("Range", fmt.Sprintf("0-%d", len(r.content)-1))
			w.WriteHeader(http.StatusAccepted)

		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/blobs/uploads/"):
			r.mu.Lock()
			have := len(r.content)
			r.mu.Unlock()
			if have > 0 {
				w.Header().Set("Range", fmt.Sprintf("0-%d", have-1))
			}
			w.Header().Set("Location", "/v2/test/blobs/uploads/session")
			w.WriteHeader(http.StatusNoContent)

		case req.Method == http.MethodDelete:
			r.mu.Lock()
			r.cancelled = true
			r.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case req.Method == http.MethodPut:
			body := readAllOrFail(t, req)
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(body) > 0 {
				// The fallback path sends everything in the closing PUT.
				r.content = body
				r.monolithic = true
			}
			r.completed = req.URL.Query().Get("digest")
			w.Header().Set("Docker-Content-Digest", r.completed)
			w.WriteHeader(http.StatusCreated)

		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	return mux
}

func readAllOrFail(t *testing.T, req *http.Request) []byte {
	t.Helper()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := req.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf
		}
	}
}

// chunkedFixtureChunk is the upload chunk size every fixture uses. The tests
// vary the blob size against it, never the other way round.
const chunkedFixtureChunk = 4 << 10

// chunkedFixture builds a client pointed at a chunked registry, plus a staged
// blob of the given size, uploaded in chunkedFixtureChunk-byte pieces.
func chunkedFixture(t *testing.T, size int64) (*Client, *chunkedRegistry, ocispec.Descriptor, *os.File) {
	t.Helper()
	reg := &chunkedRegistry{}
	ts := httptest.NewServer(reg.handler(t))
	t.Cleanup(ts.Close)

	client, err := NewClient(strings.TrimPrefix(ts.URL, "http://")+"/test", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.UploadChunkSize = chunkedFixtureChunk

	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	return client, reg, ocispec.Descriptor{
		MediaType: MediaTypeGitPackfile,
		Digest:    opencontainers.FromBytes(payload),
		Size:      size,
	}, file
}

// assertUploaded checks the registry ended up with exactly the blob described.
func assertUploaded(t *testing.T, reg *chunkedRegistry, desc ocispec.Descriptor) {
	t.Helper()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if got := opencontainers.FromBytes(reg.content); got != desc.Digest {
		t.Errorf("the registry assembled %d bytes digesting to %s, want %d bytes of %s",
			len(reg.content), got, desc.Size, desc.Digest)
	}
	if reg.completed != desc.Digest.String() {
		t.Errorf("the upload was closed with digest %q, want %q", reg.completed, desc.Digest)
	}
}

func TestChunkedUploadSendsEveryByteOnce(t *testing.T) {
	const size = 10 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	if err := client.pushBlobResumable(context.Background(), desc, file); err != nil {
		t.Fatalf("pushBlobResumable: %v", err)
	}
	assertUploaded(t, reg, desc)

	reg.mu.Lock()
	patches := append([]string(nil), reg.patches...)
	reg.mu.Unlock()

	// 10 KiB in 4 KiB chunks is three requests, the last one short. Ranges are
	// inclusive on both ends, which is the detail most easily got wrong: an
	// off-by-one here overlaps or drops a byte and only the digest notices.
	want := []string{"0-4095", "4096-8191", "8192-10239"}
	if len(patches) != len(want) {
		t.Fatalf("sent %d chunks (%v), want %d", len(patches), patches, len(want))
	}
	for i := range want {
		if patches[i] != want[i] {
			t.Errorf("chunk %d had Content-Range %q, want %q", i, patches[i], want[i])
		}
	}
}

// TestChunkedUploadResumesAfterAFailedChunk is the whole point.
//
// The registry rejects one chunk *after* keeping its bytes, which is the nasty
// case: the client cannot tell from the error how much landed. Asking the
// registry rather than assuming is the only thing that produces a correct blob,
// and a client that re-sent from its own idea of the offset would duplicate the
// chunk and fail the digest check.
func TestChunkedUploadResumesAfterAFailedChunk(t *testing.T) {
	const size = 10 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	reg.mu.Lock()
	reg.failNextPatch = true
	reg.keepBytesOnFailure = true
	reg.mu.Unlock()

	if err := client.pushBlobResumable(context.Background(), desc, file); err != nil {
		t.Fatalf("an upload that lost one chunk should have resumed: %v", err)
	}
	assertUploaded(t, reg, desc)
}

// TestChunkedUploadResumesWhenTheChunkWasLost covers the other half: the chunk
// failed and the registry kept nothing, so the same bytes have to be sent again.
func TestChunkedUploadResumesWhenTheChunkWasLost(t *testing.T) {
	const size = 10 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	reg.mu.Lock()
	reg.failNextPatch = true
	reg.keepBytesOnFailure = false
	reg.mu.Unlock()

	if err := client.pushBlobResumable(context.Background(), desc, file); err != nil {
		t.Fatalf("an upload whose chunk was dropped should have re-sent it: %v", err)
	}
	assertUploaded(t, reg, desc)
}

// TestChunkedUploadFallsBackWhenRefused: a registry that takes uploads but not
// chunked ones must not break the push. The refusal has to come before any
// content crosses the wire, so trying costs a round trip rather than a
// transfer.
func TestChunkedUploadFallsBackWhenRefused(t *testing.T) {
	const size = 10 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	reg.mu.Lock()
	reg.refusePatch = true
	reg.mu.Unlock()

	err := client.pushBlobResumable(context.Background(), desc, file)
	if err == nil {
		t.Fatal("a registry refusing the session should not report success")
	}
	if !strings.Contains(err.Error(), ErrChunkedUnsupported.Error()) {
		t.Errorf("error %v does not identify itself as unsupported-chunking, so the "+
			"caller cannot tell it apart from a real upload failure", err)
	}
	reg.mu.Lock()
	sent := len(reg.content)
	reg.mu.Unlock()
	if sent != 0 {
		t.Errorf("%d bytes were sent before the fallback; refusing has to be free", sent)
	}

	// And the fallback itself completes the push.
	reg.mu.Lock()
	reg.refusePatch = true
	reg.mu.Unlock()
	if err := client.pushPackfileBlob(context.Background(), desc, file); err != nil {
		t.Fatalf("the monolithic fallback failed: %v", err)
	}
	assertUploaded(t, reg, desc)
	reg.mu.Lock()
	monolithic := reg.monolithic
	reg.mu.Unlock()
	if !monolithic {
		t.Error("the fallback did not send the blob in one request")
	}
}

// TestSmallBlobsAreNotChunked: below the threshold there is nothing to resume,
// and the extra round trips would be spent for nothing.
func TestSmallBlobsAreNotChunked(t *testing.T) {
	client, reg, desc, file := chunkedFixture(t, 1<<10)

	if err := client.pushPackfileBlob(context.Background(), desc, file); err != nil {
		t.Fatalf("pushPackfileBlob: %v", err)
	}
	reg.mu.Lock()
	patches := len(reg.patches)
	reg.mu.Unlock()
	if patches != 0 {
		t.Errorf("a blob smaller than one chunk was sent in %d PATCHes; it should go whole", patches)
	}
}

// TestChunkingDisabledSendsWhole pins the escape hatch, for a registry that
// mishandles PATCH in some way this cannot detect.
func TestChunkingDisabledSendsWhole(t *testing.T) {
	client, reg, desc, file := chunkedFixture(t, 10<<10)
	client.UploadChunkSize = 0

	if err := client.pushPackfileBlob(context.Background(), desc, file); err != nil {
		t.Fatalf("pushPackfileBlob: %v", err)
	}
	reg.mu.Lock()
	patches := len(reg.patches)
	reg.mu.Unlock()
	if patches != 0 {
		t.Errorf("chunking is off but %d PATCHes were sent", patches)
	}
}

// TestChunkedUploadFallsBackOnAnyFirstChunkFailure.
//
// Registries do not agree on how to say "not supported". Distribution's own
// implementation answers a malformed Content-Range with 500, which is
// indistinguishable from a server having a bad day — and if the first chunk
// never lands, the distinction does not matter: chunking is layered over a path
// that already worked, so it must fall back rather than fail a push that would
// previously have succeeded.
func TestChunkedUploadFallsBackOnAnyFirstChunkFailure(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusRequestedRangeNotSatisfiable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, reg, desc, file := chunkedFixture(t, 10<<10)
			reg.mu.Lock()
			reg.refusePatch = true
			reg.patchStatus = status
			reg.mu.Unlock()

			if err := client.pushPackfileBlob(context.Background(), desc, file); err != nil {
				t.Fatalf("a registry answering PATCH with %d should have fallen back, not failed the push: %v",
					status, err)
			}
			assertUploaded(t, reg, desc)
		})
	}
}

// TestChunkedUploadCancelsTheSessionWhenItGivesUp.
//
// An upload that stops part-way leaves the registry holding what it accepted
// against a session nothing will ever close. Distribution keeps that until its
// own timeout — hours of storage nobody can see or reclaim — and the spec has a
// DELETE for exactly this case.
func TestChunkedUploadCancelsTheSessionWhenItGivesUp(t *testing.T) {
	const size = 20 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	// The first chunk lands, every one after it fails. That is the shape that
	// must not fall back to a monolithic retry -- the session works, the
	// transfer does not -- so the upload gives up, and owes a cancellation.
	reg.mu.Lock()
	reg.failEveryPatch = true
	reg.mu.Unlock()

	if err := client.pushBlobResumable(context.Background(), desc, file); err == nil {
		t.Fatal("an upload whose chunks all fail should not report success")
	}

	reg.mu.Lock()
	cancelled := reg.cancelled
	reg.mu.Unlock()
	if !cancelled {
		t.Error("the upload session was abandoned without being cancelled; the registry " +
			"holds the partial blob until its own timeout")
	}
}

// TestChunkedUploadDoesNotCancelAfterSuccess: the closing PUT ends the session,
// and a DELETE afterwards would ask the registry to discard what it just
// accepted.
func TestChunkedUploadDoesNotCancelAfterSuccess(t *testing.T) {
	const size = 10 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	if err := client.pushBlobResumable(context.Background(), desc, file); err != nil {
		t.Fatalf("pushBlobResumable: %v", err)
	}
	assertUploaded(t, reg, desc)

	reg.mu.Lock()
	cancelled := reg.cancelled
	reg.mu.Unlock()
	if cancelled {
		t.Error("a completed upload was cancelled afterwards")
	}
}

// TestChunkedUploadCompletesWhenTheFinalResponseIsLost.
//
// The last chunk lands and only its response is lost. The retry's probe then
// finds the registry holding the whole blob, and there is nothing left to
// send -- but the loop used to carry the failed PATCH's error out with it and
// fail a push the registry had, in fact, fully accepted.
func TestChunkedUploadCompletesWhenTheFinalResponseIsLost(t *testing.T) {
	const size = 10 << 10
	client, reg, desc, file := chunkedFixture(t, size)

	reg.mu.Lock()
	reg.loseFinalResponse = true
	reg.blobSize = size
	reg.mu.Unlock()

	if err := client.pushBlobResumable(context.Background(), desc, file); err != nil {
		t.Fatalf("an upload whose final response was lost should have completed: %v", err)
	}
	assertUploaded(t, reg, desc)

	reg.mu.Lock()
	cancelled := reg.cancelled
	reg.mu.Unlock()
	if cancelled {
		t.Error("a completed upload was cancelled afterwards")
	}
}
