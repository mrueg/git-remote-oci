package gc_test

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/gc"
	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// This file covers gc.Run itself, which had no unit test at all: gc_test.go
// exercises oci.ClassifyTag, a different package. The consolidate-then-prune
// ordering is the whole design and it is destructive when wrong, so it is worth
// pinning somewhere faster and more targeted than the container end-to-end run.

// TestRunConsolidatesThenPrunes is the ordering the whole design rests on.
//
// Commit-id tags are pack bases. Pruning one before its replacement exists
// would strand every manifest naming it, so consolidation has to come first and
// the surviving ref manifest has to be self-contained afterwards.
func TestRunConsolidatesThenPrunes(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	before := reg.Tags()
	if len(before) < 4 {
		t.Fatalf("expected a tag per push plus the index, got %v", before)
	}

	res, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc.Run: %v", err)
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}
	if res.TagsAfter >= res.TagsBefore {
		t.Errorf("gc did not reduce the tag count: %d -> %d", res.TagsBefore, res.TagsAfter)
	}

	// The surviving ref must be self-contained: nothing may remain that points
	// at a commit tag gc has removed.
	// A fresh client, so nothing is answered from the cache the push filled.
	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive gc: %v", err)
	}
	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil {
		t.Fatalf("the consolidated manifest has unreadable pack bases: %v", err)
	}
	if len(bases) != 0 {
		t.Errorf("the consolidated pack still declares bases %v; it must stand alone", bases)
	}
	if rev := manifest.Annotations[ocispec.AnnotationRevision]; rev != tip {
		t.Errorf("ref manifest points at %s, want the tip %s", rev, tip)
	}
}

// TestRunRepublishesThePackIndex.
//
// Consolidation replaces a ref's packfile, which invalidates whatever object
// index the original pushes published — so gc has to build a new one rather
// than carry the old one over or drop it. Dropping it is safe (a reader treats
// a missing index as "unknown" and stages the ref to look) and wasteful in
// exactly the wrong place: a consolidated packfile is the entire history, so it
// is the download a lazy fetch most wants to avoid guessing at.
func TestRunRepublishesThePackIndex(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	if _, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}}); err != nil {
		t.Fatalf("gc.Run: %v", err)
	}

	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive gc: %v", err)
	}
	index, ok := reader.FetchPackIndex(context.Background(), manifest)
	if !ok {
		t.Fatal("the consolidated manifest publishes no pack index, so every lazy fetch " +
			"against this repository has to stage the whole history to find one blob")
	}

	// The consolidated pack covers all three commits, so the index has to as
	// well — an index describing only the last push would be worse than none,
	// because a reader believes it and skips the pack that has the object.
	if !oci.PackIndexContains(index, []string{tip}) {
		t.Errorf("the index does not list the tip %s", tip)
	}
	all, err := repo.PackedObjects(plumbing.NewHash(tip), nil)
	if err != nil {
		t.Fatalf("listing the objects the consolidated pack should hold: %v", err)
	}
	for _, obj := range all {
		if !oci.PackIndexContains(index, []string{obj.OID}) {
			t.Errorf("the index omits %s, which the consolidated pack contains", obj.OID)
		}
		// Sizes are the point of the v2 index: object-info answers from them.
		if size, has := oci.PackIndexSize(index, obj.OID); !has || size != obj.Size {
			t.Errorf("the index records %s as %d bytes (present=%v), want %d",
				obj.OID, size, has, obj.Size)
		}
	}
}

// TestRunDryRunTouchesNothing: a dry run reports and writes nothing.
func TestRunDryRunTouchesNothing(t *testing.T) {
	reg := registrytest.New()
	client := registrytest.Client(t, reg.Serve(t))
	repo, _ := registrytest.SeedRepository(t, client, 3)

	before := reg.Tags()

	res, err := gc.Run(context.Background(), client, repo, gc.Options{
		DryRun: true,
		Logf:   func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("gc.Run: %v", err)
	}
	if res.RefsConsolidated == 0 {
		t.Error("a dry run should still report what it would consolidate")
	}

	after := reg.Tags()
	if len(after) != len(before) {
		t.Errorf("a dry run changed the tag list: %v -> %v", before, after)
	}
	if deleted := len(reg.Deletions()); deleted != 0 {
		t.Errorf("a dry run issued %d deletions", deleted)
	}
}

// TestRunOnRegistryRefusingDeletion: consolidation still has to happen where
// manifests cannot be removed. The space is not reclaimed, but nothing is lost
// and the repository stays clonable.
func TestRunOnRegistryRefusingDeletion(t *testing.T) {
	reg := registrytest.New()
	reg.RefuseDelete = true
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, _ := registrytest.SeedRepository(t, client, 3)

	res, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc must not fail on a registry that refuses deletion: %v", err)
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}
	if res.TagsUnprunable == 0 {
		t.Error("tags that could not be pruned should be counted and reported")
	}

	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive: %v", err)
	}
	if bases, _ := oci.ParsePackBases(manifest.Annotations); len(bases) != 0 {
		t.Errorf("the pack was not consolidated: bases %v", bases)
	}
}

// TestRunOnEmptyRepositoryIsANoOp guards the degenerate case.
func TestRunOnEmptyRepositoryIsANoOp(t *testing.T) {
	reg := registrytest.New()
	client := registrytest.Client(t, reg.Serve(t))

	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", "-q", "-b", "main", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	res, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc.Run on an empty repository: %v", err)
	}
	if res.RefsConsolidated != 0 {
		t.Errorf("consolidated %d refs on an empty repository", res.RefsConsolidated)
	}
}

// TestRunRequiresALogger pins the contract rather than letting a nil call panic
// deep inside the run.
func TestRunRequiresALogger(t *testing.T) {
	reg := registrytest.New()
	client := registrytest.Client(t, reg.Serve(t))
	if _, err := gc.Run(context.Background(), client, nil, gc.Options{}); err == nil {
		t.Error("gc.Run without a logger should be refused")
	}
}

// TestRunRebuildsThePackChain.
//
// The published pack-base chain names the intermediate commit manifests that
// step 2 deletes. Carrying those edges forward would leave a graph pointing at
// manifests that no longer exist, growing by one entry per push forever --
// which is the accumulation gc exists to undo. After a run the chain must
// describe only what survives: one self-contained entry per ref.
func TestRunRebuildsThePackChain(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	if _, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}}); err != nil {
		t.Fatalf("gc.Run: %v", err)
	}

	reader := registrytest.Client(t, ts)
	chain, ok := reader.FetchPackChain(context.Background())
	if !ok {
		t.Fatal("gc published no pack chain, so every clone afterwards walks the annotations one at a time")
	}
	if len(chain) != 1 {
		t.Errorf("the chain has %d entries after gc, want exactly one per ref: %v", len(chain), chain)
	}
	bases, present := chain[tip]
	if !present {
		t.Fatalf("the chain says nothing about the surviving tip %s: %v", tip, chain)
	}
	if len(bases) != 0 {
		t.Errorf("the consolidated tip still declares bases %v; it must stand alone", bases)
	}
}

// TestRunWithoutALocalClone is what makes gc schedulable.
//
// Consolidation needs git objects, and they used to have to be in a local
// clone -- so the one maintenance job that most wants to run unattended next to
// the registry was the one job that needed a machine holding a full clone of
// every repository it looked after. The objects are in the registry; gc fetches
// what it does not have.
func TestRunWithoutALocalClone(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 3)

	before := reg.Tags()

	// A nil repository is the honest version of "this process has no clone".
	// A fresh client too, so nothing is answered out of the caches the seeding
	// pushes filled.
	reader := registrytest.Client(t, ts)
	res, err := gc.Run(context.Background(), reader, nil, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc without a local clone: %v", err)
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}
	if res.TagsAfter >= res.TagsBefore {
		t.Errorf("gc did not reduce the tag count: %d -> %d (before: %v)", res.TagsBefore, res.TagsAfter, before)
	}

	// The consolidated pack has to be self-contained and actually hold the
	// history -- a run that fetched nothing would produce an empty pack and
	// still report success on every count above.
	verifier := registrytest.Client(t, ts)
	manifest, err := verifier.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive gc: %v", err)
	}
	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil || len(bases) != 0 {
		t.Errorf("the consolidated pack declares bases %v (err=%v); it must stand alone", bases, err)
	}
	index, ok := verifier.FetchPackIndex(context.Background(), manifest)
	if !ok {
		t.Fatal("no pack index on the consolidated manifest")
	}
	if !oci.PackIndexContains(index, []string{tip}) {
		t.Errorf("the repacked history does not contain the tip %s, so nothing was actually fetched", tip)
	}
}

// TestRunFetchesOnlyWhatIsMissing: a clone that already holds the history must
// not pay for a download it does not need.
func TestRunFetchesOnlyWhatIsMissing(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, _ := registrytest.SeedRepository(t, client, 3)

	var log strings.Builder
	if _, err := gc.Run(context.Background(), client, repo, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	}); err != nil {
		t.Fatalf("gc.Run: %v", err)
	}
	if strings.Contains(log.String(), "fetching") {
		t.Errorf("gc downloaded history it already had:\n%s", log.String())
	}
}

// Compaction rewrites ref manifests and deletes commit tags, from a snapshot of
// the ref index taken when the run began. Nothing in a registry is
// transactional, so a push landing in between used to be lost three ways over:
// the ref manifest republished at the older tip, the commit tag that push had
// just published deleted as unreachable, and `_refs` written back from the
// stale snapshot -- the merge inside that write gives the caller precedence
// unconditionally, so the older SHA won.
//
// All three are silent. The pushing client was told `ok` before any of it.
//
// That was survivable while gc was a command someone ran deliberately, at a
// quiet moment. It stopped being survivable when a push started triggering it,
// because a repository busy enough to need compacting is one with other pushes
// going on.

// TestRunLeavesAConcurrentlyMovedRefAlone.
//
// A push writes the commit manifest, then the ref manifest, then `_refs`, so a
// ref manifest ahead of the index is exactly what an in-flight push looks like.
// gc must notice and leave that ref where it is.
func TestRunLeavesAConcurrentlyMovedRefAlone(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	// Someone else's push, landed after this run would have read the index.
	const theirTip = "1234567890abcdef1234567890abcdef12345678"
	reg.SetRefRevision(t, "refs/heads/main", theirTip)

	var log strings.Builder
	res, err := gc.Run(context.Background(), client, repo, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	if err != nil {
		t.Fatalf("gc.Run: %v", err)
	}

	if res.RefsConsolidated != 0 {
		t.Errorf("repacked %d ref(s); the only ref had moved and must have been left alone:\n%s",
			res.RefsConsolidated, log.String())
	}
	if res.CommitTagsPruned != 0 {
		t.Errorf("pruned %d commit tag(s) from a view that was already stale; "+
			"one of them may be a pack base the other push was cut against:\n%s",
			res.CommitTagsPruned, log.String())
	}
	if !strings.Contains(log.String(), "moved to") {
		t.Errorf("nothing in the output says why the ref was skipped:\n%s", log.String())
	}

	// And the ref manifest must still say what the other push set, not what
	// this run's snapshot said.
	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("fetch ref manifest: %v", err)
	}
	if rev := manifest.Annotations[ocispec.AnnotationRevision]; rev != theirTip {
		t.Errorf("the ref was rewound from %s to %s", theirTip, rev)
	}
	if tip == theirTip {
		t.Fatal("fixture error: the two tips must differ")
	}
}

// TestRunLeavesALockedRefAlone: a push holding the ref's lock is a push in
// flight. gc takes the same lock, and not getting it is a reason to skip rather
// than to wait -- a push blocked behind a repack is the delay compaction most
// needs to avoid causing.
func TestRunLeavesALockedRefAlone(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, _ := registrytest.SeedRepository(t, client, 3)

	// A different client, so this is somebody else's lock.
	pusher := registrytest.Client(t, ts)
	if _, err := pusher.AcquireRefLock(context.Background(), "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("could not simulate a push in flight: %v", err)
	}

	var log strings.Builder
	res, err := gc.Run(context.Background(), client, repo, gc.Options{
		Logf: func(format string, a ...any) { fmt.Fprintf(&log, format, a...) },
	})
	if err != nil {
		t.Fatalf("gc.Run: %v", err)
	}
	if res.RefsConsolidated != 0 {
		t.Errorf("repacked %d ref(s) while a push held the lock:\n%s", res.RefsConsolidated, log.String())
	}
	if res.CommitTagsPruned != 0 {
		t.Errorf("pruned %d commit tag(s) while a push was in flight:\n%s", res.CommitTagsPruned, log.String())
	}
}

// TestRunTakesAndReleasesTheRefLock: gc must not leave the lock behind, or the
// next push blocks until the TTL runs out.
func TestRunTakesAndReleasesTheRefLock(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, _ := registrytest.SeedRepository(t, client, 3)

	if _, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}}); err != nil {
		t.Fatalf("gc.Run: %v", err)
	}

	other := registrytest.Client(t, ts)
	locked, info, err := other.IsLocked(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if locked {
		t.Errorf("gc left refs/heads/main locked (%+v); the next push would wait out the TTL", info)
	}
}

// TestRunDoesNotRewindRefsIndexOnAConcurrentPush is the third of the three
// paths, and the one the guards above do not cover.
//
// Even having consolidated cleanly, gc republishes `_refs` at the end. The
// merge inside that call layers the caller's entries over whatever is currently
// published, with the caller winning unconditionally -- so handing it the
// snapshot from the start of the run writes a ref that moved in the meantime
// back to where it used to be. The digest check in that write does not help: it
// retries the merge, and the stale value wins the retry too.
//
// The move here lands while gc is mid-run, triggered off gc's own first write.
func TestRunDoesNotRewindRefsIndexOnAConcurrentPush(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	const theirTip = "fedcba9876543210fedcba9876543210fedcba98"
	if tip == theirTip {
		t.Fatal("fixture error: the two tips must differ")
	}

	// Somebody else advances the ref the first time gc writes anything: after
	// gc has read the index, before it republishes it.
	var once sync.Once
	var moveErr error
	reg.Observe(func(method, path string) {
		if method != http.MethodPut || !strings.Contains(path, "/manifests/") {
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

	reader := registrytest.Client(t, ts)
	after, fetchErr := reader.FetchRichRefIndex(context.Background())
	if fetchErr != nil {
		t.Fatalf("read the ref index back: %v", fetchErr)
	}
	if got := after["refs/heads/main"].SHA; got != theirTip {
		t.Errorf("_refs says refs/heads/main is %s, want %s: gc republished its opening "+
			"snapshot and rewound a change it had already been told about.\nRun log:\n%s",
			got, theirTip, log.String())
	}
}
