package gc_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/gc"
	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Compaction is the one operation that deletes, and every deletion it makes is
// justified by a packfile it wrote a moment earlier. These tests are the cases
// where that justification was not actually there: a source repository that
// did not hold the history, packs imported in an order index-pack could not
// complete, and a registry left describing manifests that were already gone.

// gitIn runs git in dir with a fixed identity and no user configuration.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushOne publishes one commit the way a push does: a thin packfile cut
// against bases, and a commit manifest naming them.
func pushOne(t *testing.T, client *oci.Client, repo *git.Repository, push oci.CommitPush, want string, bases []string) {
	t.Helper()
	haves := make([]plumbing.Hash, 0, len(bases))
	for _, b := range bases {
		haves = append(haves, plumbing.NewHash(b))
	}
	var pack bytes.Buffer
	if err := repo.CreatePackfileTo(&pack, plumbing.NewHash(want), haves); err != nil {
		t.Fatalf("CreatePackfileTo(%s): %v", want, err)
	}
	push.PackBases = bases
	if push.RefTag == "" {
		push.RefTag = oci.EncodeRefTag(push.RefName)
	}
	if err := client.PushCommitStream(context.Background(), push, bytes.NewReader(pack.Bytes()), int64(pack.Len())); err != nil {
		t.Fatalf("push %s: %v", push.RefName, err)
	}
}

// importedObjects fetches a ref's packfile as a stranger would and imports it
// into a fresh bare repository, so that what the pack really holds can be
// asked of git rather than inferred from an index.
func importedObjects(t *testing.T, ts *httptest.Server, refName string) string {
	t.Helper()
	ctx := context.Background()
	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(ctx, oci.EncodeRefTag(refName))
	if err != nil {
		t.Fatalf("fetch the ref manifest for %s: %v", refName, err)
	}
	stream, err := reader.FetchPackfileStream(ctx, manifest)
	if err != nil {
		t.Fatalf("fetch the packfile for %s: %v", refName, err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("close the packfile stream for %s: %v", refName, err)
		}
	}()

	bare := filepath.Join(t.TempDir(), "check.git")
	gitIn(t, t.TempDir(), "init", "-q", "--bare", bare)
	into, err := git.OpenRepositoryAt(bare)
	if err != nil {
		t.Fatalf("open the scratch repository: %v", err)
	}
	if _, err := into.ImportPackfile(stream); err != nil {
		t.Fatalf("import the packfile for %s: %v", refName, err)
	}
	return bare
}

func hasObject(t *testing.T, bare, sha string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+bare, "cat-file", "-e", sha)
	return cmd.Run() == nil
}

// TestRunFromAShallowCloneRepacksFromTheRegistry is the data-loss case.
//
// A depth-1 clone holds every tip and none of the history, and `git
// pack-objects --revs` stops at the shallow boundary without a word. gc used
// to check only that the tip commit was present, pack from the clone, publish
// the truncated result as the ref's whole history, and then prune the
// manifests that held the rest -- the only copies of it. It now treats a
// shallow clone as holding nothing and repacks from the registry.
func TestRunFromAShallowCloneRepacksFromTheRegistry(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 3)
	seedDir := filepath.Dir(os.Getenv("GIT_DIR"))
	root := gitIn(t, seedDir, "rev-list", "--max-parents=0", "HEAD")

	shallowDir := filepath.Join(t.TempDir(), "shallow")
	gitIn(t, t.TempDir(), "clone", "-q", "--depth", "1", "file://"+seedDir, shallowDir)
	shallow, err := git.OpenRepositoryAt(filepath.Join(shallowDir, ".git"))
	if err != nil {
		t.Fatalf("open the shallow clone: %v", err)
	}
	if !shallow.IsShallow() {
		t.Fatal("fixture error: the depth-1 clone does not report itself as shallow")
	}
	if _, err := shallow.GetCommitInfo(plumbing.NewHash(tip)); err != nil {
		t.Fatalf("fixture error: the shallow clone should hold the tip %s: %v", tip, err)
	}

	var log strings.Builder
	res, err := gc.Run(context.Background(), registrytest.Client(t, ts), shallow, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	if err != nil {
		t.Fatalf("gc.Run from a shallow clone: %v\n%s", err, log.String())
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}
	if !strings.Contains(log.String(), "shallow") {
		t.Errorf("the log does not say the local clone was passed over for being shallow:\n%s", log.String())
	}

	bare := importedObjects(t, ts, "refs/heads/main")
	if !hasObject(t, bare, root) {
		t.Fatalf("the consolidated pack does not contain the root commit %s: gc packed the "+
			"shallow clone's truncated history and then pruned the only copies of the rest", root)
	}
	if !hasObject(t, bare, tip) {
		t.Errorf("the consolidated pack does not contain the tip %s", tip)
	}
}

// TestRunFromAPartialCloneRepacksFromTheRegistry: the same hazard in a
// different shape. A blob:none clone holds every commit and no blobs, and a
// pack built from it would be missing the file contents.
func TestRunFromAPartialCloneRepacksFromTheRegistry(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 3)
	seedDir := filepath.Dir(os.Getenv("GIT_DIR"))
	blob := gitIn(t, seedDir, "rev-parse", "HEAD:f0.txt")

	partialDir := filepath.Join(t.TempDir(), "partial")
	gitIn(t, t.TempDir(), "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+seedDir, partialDir)
	partial, err := git.OpenRepositoryAt(filepath.Join(partialDir, ".git"))
	if err != nil {
		t.Fatalf("open the partial clone: %v", err)
	}
	if !partial.IsPartial() {
		t.Fatal("fixture error: the blob:none clone does not report itself as partial")
	}

	var log strings.Builder
	if _, err := gc.Run(context.Background(), registrytest.Client(t, ts), partial, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	}); err != nil {
		t.Fatalf("gc.Run from a partial clone: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "partial") {
		t.Errorf("the log does not say the local clone was passed over for being partial:\n%s", log.String())
	}

	bare := importedObjects(t, ts, "refs/heads/main")
	if !hasObject(t, bare, blob) {
		t.Fatalf("the consolidated pack does not contain blob %s: gc packed a partial clone", blob)
	}
}

// TestRunHydratesAMergeDiamondInImportOrder.
//
// Hydration imports thin packs, and index-pack can only complete one once its
// bases are on disk. The order used to be breadth-first from the tips with
// first-seen marking, which is not a topological order: a tip that is also
// another tip's base stayed in the first level beside it, and the order
// within a level was map iteration. So for a feature branch F packed against
// main M, and a main tip M2 packed against F, importing M2 first failed about
// half the time. The order is now a post-order walk: every base before what
// was cut against it.
//
// The merge modifies a file the branch introduced, so M2's pack really does
// carry deltas against F's objects; a pack of one small commit would import
// in any order and prove nothing.
func TestRunHydratesAMergeDiamondInImportOrder(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main", ".")
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", name)
	}

	// M: the base of everything.
	write("base.txt", "base\n")
	gitIn(t, dir, "commit", "-q", "-m", "M")
	m := gitIn(t, dir, "rev-parse", "HEAD")
	pushOne(t, client, repo, oci.CommitPush{CommitSHA: m, RefName: "refs/heads/main"}, m, nil)

	// F: a branch adding a large, repetitive file, packed against M.
	gitIn(t, dir, "checkout", "-q", "-b", "feature")
	write("data.txt", strings.Repeat("the quick brown fox jumps over the lazy dog\n", 200))
	gitIn(t, dir, "commit", "-q", "-m", "F")
	f := gitIn(t, dir, "rev-parse", "HEAD")
	pushOne(t, client, repo, oci.CommitPush{CommitSHA: f, RefName: "refs/heads/feature"}, f, []string{m})

	// M2: main merges the branch and then edits its file, so the new tree and
	// blob are deltas against F's. Packed against both F and M.
	gitIn(t, dir, "checkout", "-q", "main")
	gitIn(t, dir, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	write("data.txt", strings.Repeat("the quick brown fox jumps over the lazy dog\n", 200)+"and one more line\n")
	gitIn(t, dir, "commit", "-q", "-m", "M2")
	m2 := gitIn(t, dir, "rev-parse", "HEAD")
	pushOne(t, client, repo, oci.CommitPush{CommitSHA: m2, RefName: "refs/heads/main"}, m2, []string{f, m})

	if err := client.PushRichRefIndex(context.Background(), map[string]oci.RefEntry{
		"refs/heads/main":    {SHA: m2},
		"refs/heads/feature": {SHA: f},
	}, nil); err != nil {
		t.Fatalf("push ref index: %v", err)
	}

	// No local clone, so every pack comes from the registry and has to be
	// imported in an order index-pack can complete.
	var log strings.Builder
	res, err := gc.Run(context.Background(), registrytest.Client(t, ts), nil, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	if err != nil {
		t.Fatalf("gc.Run over a merge diamond: %v\n%s", err, log.String())
	}
	if res.RefsConsolidated != 2 {
		t.Errorf("consolidated %d refs, want 2", res.RefsConsolidated)
	}

	blob := gitIn(t, dir, "rev-parse", "HEAD:data.txt")
	bare := importedObjects(t, ts, "refs/heads/main")
	for _, sha := range []string{m, f, m2, blob} {
		if !hasObject(t, bare, sha) {
			t.Errorf("the consolidated main pack does not contain %s", sha)
		}
	}
}

// TestRunDoesNotRewindRefsIndexOnAPushDuringTheIndexWrite is M3 exactly: a
// push landing after gc re-read the index and before it wrote it back.
//
// The write merges the caller's entries over the live index with the caller
// winning, and retries the merge on a digest conflict -- so a snapshot handed
// in here, however recently taken, wins the retry too and the other push is
// rewound. gc moves no ref and now hands in none, so the merge takes the live
// index and the push survives.
//
// The move is triggered off the index lock's own acquisition, which sits
// between the merge and the write: the one moment a snapshot is guaranteed
// stale.
func TestRunDoesNotRewindRefsIndexOnAPushDuringTheIndexWrite(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	const theirTip = "0123456789abcdef0123456789abcdef01234567"
	if tip == theirTip {
		t.Fatal("fixture error: the two tips must differ")
	}

	var once sync.Once
	var moveErr error
	reg.Observe(func(method, path string) {
		// The index lock, not the per-ref lock consolidation takes.
		if method != http.MethodPut || !strings.Contains(path, "/manifests/"+oci.LockTagPrefix) ||
			strings.HasSuffix(path, oci.LockTagPrefix+"main") {
			return
		}
		once.Do(func() { moveErr = reg.SetIndexedRef("refs/heads/main", theirTip) })
	})

	var log strings.Builder
	_, runErr := gc.Run(context.Background(), client, repo, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	reg.Observe(nil)
	if moveErr != nil {
		t.Fatalf("could not simulate the concurrent move: %v", moveErr)
	}
	if runErr != nil {
		t.Fatalf("gc.Run: %v\n%s", runErr, log.String())
	}

	after, err := registrytest.Client(t, ts).FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("read the ref index back: %v", err)
	}
	if got := after["refs/heads/main"].SHA; got != theirTip {
		t.Errorf("_refs says refs/heads/main is %s, want %s: gc's index write carried a stale "+
			"snapshot and rewound a push that landed during it.\nRun log:\n%s", got, theirTip, log.String())
	}
}

// TestRunDoesNotRewindRefsIndexOnAPushDuringPruning: the same push, landing
// while gc is deleting commit tags.
//
// This used to be the widest window of all -- the whole prune phase sat
// between the index re-read and the write-back -- and it closes only because
// the index is now written *before* anything is pruned. The move is injected
// on the first deletion after the index write, and the test insists that
// interleaving actually happened rather than passing because it did not.
func TestRunDoesNotRewindRefsIndexOnAPushDuringPruning(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	const theirTip = "89abcdef0123456789abcdef0123456789abcdef"
	if tip == theirTip {
		t.Fatal("fixture error: the two tips must differ")
	}

	var indexWritten atomic.Bool
	var moved atomic.Bool
	var moveErr error
	reg.Observe(func(method, path string) {
		switch {
		case method == http.MethodPut && strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex):
			indexWritten.Store(true)
		case method == http.MethodDelete && indexWritten.Load() && moved.CompareAndSwap(false, true):
			moveErr = reg.SetIndexedRef("refs/heads/main", theirTip)
		}
	})

	var log strings.Builder
	res, runErr := gc.Run(context.Background(), client, repo, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	reg.Observe(nil)
	if moveErr != nil {
		t.Fatalf("could not simulate the concurrent move: %v", moveErr)
	}
	if runErr != nil {
		t.Fatalf("gc.Run: %v\n%s", runErr, log.String())
	}
	if !moved.Load() {
		t.Fatalf("the move was never injected: no deletion followed the index write, so the "+
			"interleaving this test is about did not happen.\nRun log:\n%s", log.String())
	}
	if res.CommitTagsPruned == 0 {
		t.Fatalf("nothing was pruned, so the move did not land during pruning.\nRun log:\n%s", log.String())
	}

	after, err := registrytest.Client(t, ts).FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("read the ref index back: %v", err)
	}
	if got := after["refs/heads/main"].SHA; got != theirTip {
		t.Errorf("_refs says refs/heads/main is %s, want %s: a push that landed during pruning was rewound.\nRun log:\n%s",
			got, theirTip, log.String())
	}
}

// failingRefsWrite fronts a registry with a proxy that answers every write of
// the `_refs` manifest with a server error and passes everything else through.
//
// A test needs to fail one specific request from the outside, and the shared
// registry's hook can observe a request but not answer it.
func failingRefsWrite(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.TagRefIndex) {
			http.Error(w, "the registry is having a bad day", http.StatusInternalServerError)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	return front
}

// TestRunDoesNotPruneWhenTheIndexWriteFails is M4.
//
// The pack chain on `_refs` names the intermediate manifests pruning removes,
// and it is rewritten by the index write. Pruning first and writing second
// meant a failed write left the chain naming manifests that were already gone
// -- and a reader following the chain treats a missing manifest as fatal. The
// write now comes first, so a failure leaves the repository exactly as
// consolidated: larger than it could be, and clonable.
func TestRunDoesNotPruneWhenTheIndexWriteFails(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, _ := registrytest.SeedRepository(t, client, 3)

	commitTags := func() []string {
		var tags []string
		for _, tag := range reg.Tags() {
			if oci.ClassifyTag(tag) == oci.TagClassCommit {
				tags = append(tags, tag)
			}
		}
		return tags
	}
	before := commitTags()
	if len(before) != 3 {
		t.Fatalf("fixture error: expected 3 commit tags, got %v", before)
	}

	front := failingRefsWrite(t, ts)
	var log strings.Builder
	_, err := gc.Run(context.Background(), registrytest.Client(t, front), repo, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	if err == nil {
		t.Fatalf("gc.Run reported success although the ref index could not be written\n%s", log.String())
	}
	if !strings.Contains(err.Error(), "ref index") {
		t.Errorf("the error does not say what failed: %v", err)
	}

	if after := commitTags(); len(after) != len(before) {
		t.Errorf("commit tags were pruned before the index write failed: %v -> %v; the published "+
			"pack chain still names them, so the repository is now unclonable", before, after)
	}

	// Everything the chain names must still be there, which is what a reader
	// that trusts the chain needs to be true.
	verifier := registrytest.Client(t, ts)
	chain, _ := verifier.FetchPackChain(context.Background())
	for sha, bases := range chain {
		for _, named := range append([]string{sha}, bases...) {
			if _, err := verifier.FetchManifest(context.Background(), named); err != nil {
				t.Errorf("the pack chain names %s, which the registry no longer serves: %v", named, err)
			}
		}
	}
}

// TestRunKeepsAnnotatedTagMetadata is M5.
//
// FORMAT.md §5 puts an annotated tag's tagger, message, signature and tag
// object on the ref manifest, and consolidation replaces that manifest. It
// used to republish it bare, so every annotated tag in a compacted repository
// came back as a lightweight one. The index entry recorded the same facts at
// push time, and they are carried across from there.
func TestRunKeepsAnnotatedTagMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local bool
	}{
		{"from the local clone", true},
		{"hydrated from the registry", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registrytest.New()
			ts := reg.Serve(t)
			client := registrytest.Client(t, ts)
			repo, tip := registrytest.SeedRepository(t, client, 3)
			seedDir := filepath.Dir(os.Getenv("GIT_DIR"))
			parent := gitIn(t, seedDir, "rev-parse", "HEAD~1")

			gitIn(t, seedDir, "tag", "-a", "v1", "-m", "release one")
			tagObject := gitIn(t, seedDir, "rev-parse", "v1")
			if gitIn(t, seedDir, "rev-parse", "v1^{commit}") != tip {
				t.Fatal("fixture error: the tag does not point at the tip")
			}
			info, err := repo.GetAnnotatedTagInfo("refs/tags/v1")
			if err != nil {
				t.Fatalf("GetAnnotatedTagInfo: %v", err)
			}

			// The tag's push: its packfile is built from the tag object so
			// the object is included, cut against the commit's parent, which
			// is the newest base with a manifest of its own.
			pushOne(t, client, repo, oci.CommitPush{
				CommitSHA: tip,
				RefName:   "refs/tags/v1",
				TagAnnotations: map[string]string{
					oci.AnnotationGitTagger:     info.Tagger,
					oci.AnnotationGitTagMessage: info.Message,
					oci.AnnotationGitTagSig:     info.Signature,
					oci.AnnotationGitTagObj:     info.ObjectHash,
				},
			}, tagObject, []string{parent})
			want := oci.RefEntry{
				SHA:        tip,
				Tagger:     info.Tagger,
				TagMessage: info.Message,
				TagSig:     info.Signature,
				TagObject:  tagObject,
			}
			if err := client.PushRichRefIndex(context.Background(), map[string]oci.RefEntry{
				"refs/heads/main": {SHA: tip},
				"refs/tags/v1":    want,
			}, nil); err != nil {
				t.Fatalf("push ref index: %v", err)
			}

			source := repo
			if !tc.local {
				source = nil
			}
			var log strings.Builder
			res, err := gc.Run(context.Background(), registrytest.Client(t, ts), source, gc.Options{
				Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
			})
			if err != nil {
				t.Fatalf("gc.Run: %v\n%s", err, log.String())
			}
			if res.RefsConsolidated != 2 {
				t.Errorf("consolidated %d refs, want 2", res.RefsConsolidated)
			}

			reader := registrytest.Client(t, ts)
			manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/tags/v1"))
			if err != nil {
				t.Fatalf("the tag's ref manifest did not survive gc: %v", err)
			}
			for key, wantValue := range map[string]string{
				oci.AnnotationGitTagObj:     tagObject,
				oci.AnnotationGitTagger:     info.Tagger,
				oci.AnnotationGitTagMessage: "release one",
				ocispec.AnnotationRevision:  tip,
			} {
				if got := manifest.Annotations[key]; got != wantValue {
					t.Errorf("after gc the tag's ref manifest has %s=%q, want %q", key, got, wantValue)
				}
			}
			if bases, err := oci.ParsePackBases(manifest.Annotations); err != nil || len(bases) != 0 {
				t.Errorf("the consolidated tag manifest declares bases %v (err=%v)", bases, err)
			}

			// The pack must carry the tag object itself, or the ref resolves
			// to a commit and the tag is lightweight on the next clone.
			bare := importedObjects(t, ts, "refs/tags/v1")
			if !hasObject(t, bare, tagObject) {
				t.Errorf("the consolidated pack for refs/tags/v1 does not contain the tag object %s", tagObject)
			}
			cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+bare, "cat-file", "-t", tagObject)
			if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "tag" {
				t.Errorf("%s is a %s in the repacked history, want a tag", tagObject, strings.TrimSpace(string(out)))
			}

			after, err := reader.FetchRichRefIndex(context.Background())
			if err != nil {
				t.Fatalf("read the ref index back: %v", err)
			}
			if got := after["refs/tags/v1"]; got != want {
				t.Errorf("the index entry for refs/tags/v1 changed under gc:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}
