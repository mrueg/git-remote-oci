package helper_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/helper"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Every push ends by rewriting the `_refs` index, and what it wrote used to be
// the whole ref set the session had listed at `list for-push` -- with this
// push's refs layered on top. That listing is a snapshot. A ref another client
// advanced between the listing and the write was in it at its old value, and
// the merge overlaid every entry unconditionally, so the other client's push
// was quietly reverted by one that never touched its ref.
//
// The fix is to tell the index update only about the refs this batch actually
// moved, which is the only thing a push has any authority over.

// syncWriter is a strings.Builder safe to read while a Helper is writing to
// it from another goroutine.
type syncWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// waitForOutput polls until the helper's output satisfies cond.
func waitForOutput(t *testing.T, out *syncWriter, cond func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond(out.String()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for helper output; have:\n%s", out.String())
}

// publishedRefs reads the `_refs` index the way a stranger would: through a
// client with no caches of its own to answer from.
func publishedRefs(t *testing.T, reg *registrytest.Registry) map[string]oci.RefEntry {
	t.Helper()
	client := registrytest.Client(t, reg.LastServer())
	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	return refs
}

// TestPushDoesNotRevertRefsAnotherClientAdvanced is the interleaving itself:
// A lists, B moves a ref and finishes, A pushes a different ref.
func TestPushDoesNotRevertRefsAnotherClientAdvanced(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	registry := registrytest.URL(ts)

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	// Two branches on the remote.
	git(t, src, "checkout", "-q", "-b", "dev")
	devOld := commitFile(t, src, "dev.txt", "dev v1\n", "dev one")
	git(t, src, "checkout", "-q", "main")
	if out, err := runHelper(t, registry,
		"list for-push\npush refs/heads/main:refs/heads/main\npush refs/heads/dev:refs/heads/dev\n\n"); err != nil {
		t.Fatalf("seeding push failed: %v (output %q)", err, out)
	}

	// Both advance locally. Which client pushes which is decided below.
	git(t, src, "checkout", "-q", "dev")
	devNew := commitFile(t, src, "dev.txt", "dev v2\n", "dev two")
	git(t, src, "checkout", "-q", "main")
	mainNew := commitFile(t, src, "main.txt", "main v2\n", "main two")
	if devNew == devOld {
		t.Fatal("dev did not advance")
	}

	// Client A lists, and then waits.
	inR, inW := io.Pipe()
	var aOut syncWriter
	a, err := helper.NewHelper("origin", registry, inR, &aOut)
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	aDone := make(chan error, 1)
	go func() { aDone <- a.Run(context.Background()) }()

	if _, err := io.WriteString(inW, "list for-push\n"); err != nil {
		t.Fatalf("write to helper A: %v", err)
	}
	waitForOutput(t, &aOut, func(s string) bool { return strings.HasSuffix(s, "\n\n") })
	if !strings.Contains(aOut.String(), devOld+" refs/heads/dev") {
		t.Fatalf("A's listing should show dev at %s:\n%s", shortID(devOld), aOut.String())
	}

	// Client B pushes dev and completes.
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/dev:refs/heads/dev\n\n"); err != nil || !strings.Contains(out, "ok refs/heads/dev") {
		t.Fatalf("B's push of dev failed: %v (output %q)", err, out)
	}
	if got := publishedRefs(t, reg)["refs/heads/dev"].SHA; got != devNew {
		t.Fatalf("after B's push dev is %s, want %s", shortID(got), shortID(devNew))
	}

	// Client A pushes main, from a listing that still has dev at the old
	// commit.
	if _, err := io.WriteString(inW, "push refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("write to helper A: %v", err)
	}
	_ = inW.Close()
	if err := <-aDone; err != nil {
		t.Fatalf("A's push failed: %v (output %q)", err, aOut.String())
	}
	if !strings.Contains(aOut.String(), "ok refs/heads/main") {
		t.Fatalf("A's push of main should succeed:\n%s", aOut.String())
	}

	refs := publishedRefs(t, reg)
	if got := refs["refs/heads/main"].SHA; got != mainNew {
		t.Errorf("main is %s after A's push, want %s", shortID(got), shortID(mainNew))
	}
	if got := refs["refs/heads/dev"].SHA; got != devNew {
		t.Errorf("A's push of main reverted dev to %s; B had pushed %s", shortID(got), shortID(devNew))
	}
}

// TestFailedBatchDoesNotRewriteRefsIndex: a batch in which nothing moved has
// nothing to say to the index, and used to say it anyway -- the whole listing,
// snapshot and all.
func TestFailedBatchDoesNotRewriteRefsIndex(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	// Another client holds the lock, so the only ref in the batch is refused
	// before a byte of it is uploaded.
	if _, err := client.AcquireRefLock(context.Background(), "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("AcquireRefLock: %v", err)
	}

	before := registrytest.Digest(reg.RawManifest(t, oci.TagRefIndex))
	var mu sync.Mutex
	var indexWrites []string
	reg.Observe(func(method, path string) {
		if method == http.MethodPut && strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex) {
			mu.Lock()
			indexWrites = append(indexWrites, method+" "+path)
			mu.Unlock()
		}
	})

	out, err := runHelper(t, registrytest.URL(ts), "list for-push\npush refs/heads/main:refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "error refs/heads/main") || strings.Contains(out, "ok refs/heads/main") {
		t.Fatalf("expected the locked ref to be refused, got:\n%s", out)
	}

	mu.Lock()
	writes := append([]string(nil), indexWrites...)
	mu.Unlock()
	if len(writes) > 0 {
		t.Errorf("a batch in which every ref failed rewrote the _refs index: %v", writes)
	}
	if after := registrytest.Digest(reg.RawManifest(t, oci.TagRefIndex)); after != before {
		t.Errorf("_refs changed from %s to %s across a batch that pushed nothing", before, after)
	}
}

// TestListRefusesUnsupportedFormatEvenWithRefTags pins that the version check
// cannot be bypassed.
//
// discoverRemoteRefs fell through to tag enumeration whenever the index could
// not be read -- for any reason. A repository declaring a format this build
// does not implement was therefore refused by the index and then listed from
// its ref tags anyway, which is exactly the reader the version exists to stop.
func TestListRefusesUnsupportedFormatEvenWithRefTags(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	unsupported := "2"
	if unsupported == oci.FormatVersion {
		t.Fatalf("test data %q is the current format version; pick another", unsupported)
	}
	if err := reg.SetManifestAnnotation(oci.TagRefIndex, oci.AnnotationFormatVersion, unsupported); err != nil {
		t.Fatalf("SetManifestAnnotation: %v", err)
	}

	out, err := runHelper(t, registrytest.URL(ts), "list\n\n")
	if err == nil {
		t.Fatalf("list read a repository in an unsupported format instead of refusing it:\n%s", out)
	}
	if !errors.Is(err, oci.ErrUnsupportedFormat) {
		t.Errorf("list failed, but not with ErrUnsupportedFormat: %v", err)
	}
	if strings.Contains(out, "refs/heads/main") {
		t.Errorf("list advertised refs from a repository it should have refused:\n%s", out)
	}
}
