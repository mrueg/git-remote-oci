package helper_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

// End-to-end coverage for wire protocol v2, served over stateless-connect.
//
// These drive the real `git` binary against the real helper, because the value
// of this path is entirely in whether git accepts what it is sent. The protocol
// is a byte stream with sections that must appear in order, and a server that
// gets it subtly wrong does not return a wrong answer — it produces a hang, or
// a clone that looks fine and is missing objects. Only git can judge that, so
// every case below ends by asking it to: `fsck`, a commit count, or a checkout
// that has to find the blobs it needs.

// The helper under test runs as a subprocess: git spawns it, and everything it
// does happens outside the test binary that started the whole thing. Go
// attributes coverage to the process that produced it, so a thousand lines
// exercised through `git clone` counted as zero — the code was tested and the
// measurement said otherwise, which is worse than either being true on its own.
//
// `go build -cover` fixes that. The binary records what it executed into
// $GOCOVERDIR, one set of files per run, and `go tool covdata` merges them with
// the unit-test profile afterwards. It is off unless GRO_COVERDIR is set, so an
// ordinary `go test` still builds a plain binary and pays nothing; the Makefile
// sets it, creates the directory, and does the merging.
const coverDirEnv = "GRO_COVERDIR"

// buildArgs is the `go build` invocation for the helper binary.
func buildArgs(bin string) []string {
	args := []string{"build", "-o", bin}
	if os.Getenv(coverDirEnv) != "" {
		// Bare -cover, with no -coverpkg. It instruments every package in the
		// main module, which is exactly pkg/... plus main; narrowing it to
		// pkg/... instead leaves the entry point uninstrumented, and then
		// nothing registers the hook that writes the data out — the binary runs
		// and $GOCOVERDIR stays empty.
		//
		// -covermode must match the unit tests', or covdata refuses to merge
		// the two sets: "counter mode clash ... previous file had atomic, new
		// file has set".
		args = append(args, "-cover", "-covermode=atomic")
	}
	return append(args, "github.com/mrueg/git-remote-oci")
}

// instrumentSubprocessCoverage points the spawned helper at the shared
// coverage directory. The subprocess environment is built from os.Environ, so
// setting it here is what carries it through git into the helper.
func instrumentSubprocessCoverage(t *testing.T) {
	t.Helper()
	if dir := os.Getenv(coverDirEnv); dir != "" {
		t.Setenv("GOCOVERDIR", dir)
	}
}

func v2setup(t *testing.T) string {
	t.Helper()
	url, _ := v2setupRegistry(t)
	return url
}

// v2setupRegistry is v2setup for tests that assert on what crossed the wire.
func v2setupRegistry(t *testing.T) (string, *registrytest.Registry) {
	t.Helper()
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "git-remote-oci")
	if out, err := exec.CommandContext(t.Context(), "go", buildArgs(bin)...).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OCI_INSECURE", "1")
	instrumentSubprocessCoverage(t)
	reg := registrytest.New()
	return registrytest.URL(reg.Serve(t)), reg
}

func v2run(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnvWithoutGitDir(), extraEnv...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func v2seed(t *testing.T, url string, commits int) string {
	t.Helper()
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", src)
	for i := 0; i < commits; i++ {
		name := filepath.Join(src, "f"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, bytes.Repeat([]byte("x"), 100+i), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "-C", src, "add", ".")
		git(t, src, "-C", src, "commit", "-q", "-m", "commit"+string(rune('a'+i)))
	}
	git(t, src, "-C", src, "push", "-q", url, "main")
	return src
}

// --- partial clone ---------------------------------------------------------

// TestV2PartialClone is the feature the simple helper protocol cannot express
// at all: `fetch <sha> <name>` is defined as delivering a complete object graph,
// so --filter had nowhere to apply. It also covers the harder half — the lazy
// fetch afterwards, whose wants are blob ids rather than commits, and which is
// where an empty response does not fail but loops: git treats "here is nothing"
// as an answer and asks again on the next access.
func TestV2PartialClone(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 3)

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--filter=blob:none", "--no-checkout", url, "dst")
	t.Logf("partial clone:\n%s", out)
	if err != nil {
		t.Fatalf("partial clone failed: %v", err)
	}
	dst := filepath.Join(parent, "dst")

	if cfg, _ := v2run(t, dst, nil, "config", "--get", "remote.origin.promisor"); !strings.Contains(cfg, "true") {
		t.Errorf("origin is not a promisor remote (%q), so the pack was not filtered", strings.TrimSpace(cfg))
	}
	// The filter has to have actually left the blobs out. A clone that quietly
	// carried them would pass every other check here.
	objects, _ := v2run(t, dst, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
	if n := strings.Count(objects, "blob"); n != 0 {
		t.Errorf("filtered clone still carries %d blobs:\n%s", n, objects)
	}

	// Checking out forces the lazy fetch of every blob the filter omitted.
	out, err = v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"checkout", "main")
	t.Logf("lazy checkout:\n%s", out)
	if err != nil {
		t.Fatalf("lazy fetch on checkout failed: %v", err)
	}
	for _, name := range []string{"fa.txt", "fb.txt", "fc.txt"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%s was not materialised by the lazy fetch: %v", name, err)
		}
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- incremental fetch -----------------------------------------------------

// TestV2IncrementalFetch exercises the negotiation: the client arrives with
// history and sends `have` lines, which have to be checked against the local
// object store before they are acknowledged or passed to pack-objects as
// exclusions.
func TestV2IncrementalFetch(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 2)

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", url, "dst"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	// Push two more commits from the source.
	if err := os.WriteFile(filepath.Join(src, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "three")
	git(t, src, "-C", src, "push", "-q", url, "main")

	out, err := v2run(t, dst, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=true", "fetch", "origin")
	t.Logf("incremental fetch:\n%s", out)
	if err != nil {
		t.Fatalf("incremental fetch failed: %v", err)
	}
	log, _ := v2run(t, dst, nil, "log", "--format=%s", "origin/main")
	t.Logf("log after fetch: %q", log)
	if !strings.Contains(log, "three") {
		t.Errorf("fetch did not bring the new commit: %q", log)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- fallback --------------------------------------------------------------

// TestV2FallbackWhenDisabled pins the half of stateless-connect that makes it
// safe to advertise unconditionally. The capability is offered whatever the
// configuration says, so declining has to work: answering `fallback` must leave
// the simple protocol running on the same pipe, not a helper that has replied
// to a command and then gone quiet.
//
// This is the escape hatch for a registry or a client that turns out to have a
// problem with the v2 path. Now that v2 is the default it is the only way back
// to the simple one, so it matters more than it did when it was the way in.
func TestV2FallbackWhenDisabled(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 2)

	parent := t.TempDir()
	out, err := v2run(t, parent, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=false", "clone", url, "dst")
	t.Logf("fallback clone:\n%s", out)
	if err != nil {
		t.Fatalf("fallback clone failed: %v", err)
	}
	if strings.Contains(out, "command=ls-refs") {
		t.Errorf("v2 was served even though ociremote.protocolV2 is false")
	}
	log, _ := v2run(t, filepath.Join(parent, "dst"), nil, "log", "--format=%s")
	if !strings.Contains(log, "commitb") {
		t.Errorf("fallback clone incomplete: %q", log)
	}
}

// TestV2IsTheDefault pins the switch itself.
//
// Partial clone cannot be expressed through the simple fetch command at all,
// and --depth applied while the pack is built is only possible here, so leaving
// v2 off by default meant the better implementation was the one nobody got
// unless they went looking for the setting. A regression to the old default
// would not fail anything else in this file — every other test asks for v2
// explicitly — so it is asserted here.
func TestV2IsTheDefault(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 2)

	parent := t.TempDir()
	out, err := v2run(t, parent, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"clone", url, "dst")
	if err != nil {
		t.Fatalf("clone with no v2 configuration failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "command=ls-refs") {
		t.Error("an unconfigured clone did not use protocol v2")
	}

	// And it has to actually be usable unconfigured, which is the point of
	// making it the default: --filter is the thing the simple path cannot do.
	filtered := t.TempDir()
	if out, err := v2run(t, filtered, nil, "-c", "protocol.version=2",
		"clone", "--filter=blob:none", url, "dst"); err != nil {
		t.Fatalf("an unconfigured partial clone failed: %v\n%s", err, out)
	}
	dst := filepath.Join(filtered, "dst")
	if cfg, _ := v2run(t, dst, nil, "config", "--get", "remote.origin.promisor"); !strings.Contains(cfg, "true") {
		t.Errorf("origin is not a promisor remote (%q); the filter was not honoured", strings.TrimSpace(cfg))
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- shallow ---------------------------------------------------------------

// TestV2ShallowClone guards a regression rather than a feature. Advertising
// protocol v2 without `fetch=shallow` makes git refuse `clone --depth` outright
// — "Server does not support shallow requests" — so turning v2 on would have
// taken away something the simple path could already do.
func TestV2ShallowClone(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 3)

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--depth", "1", url, "dst")
	t.Logf("shallow clone:\n%s", out)
	if err != nil {
		t.Fatalf("shallow clone failed: %v", err)
	}
	dst := filepath.Join(parent, "dst")

	log, _ := v2run(t, dst, nil, "log", "--format=%s")
	if got := strings.Count(strings.TrimSpace(log), "\n") + 1; got != 1 {
		t.Errorf("shallow clone has %d commits, want 1: %q", got, log)
	}
	if out, err := v2run(t, dst, nil, "rev-parse", "--is-shallow-repository"); !strings.Contains(out, "true") {
		t.Errorf("clone is not marked shallow (%v): %q", err, out)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- deepening ---------------------------------------------------------------

// TestV2Deepen covers the case where the client arrives already shallow, which
// is where the two boundaries have to be told apart. The one it declares is the
// history it is missing; the one the response reports is where it will stop
// next. Registering the old boundary with pack-objects, or trusting a `have`
// from a client that cannot see past its own graft, both produce a pack that
// git accepts and then finds incomplete.
func TestV2Deepen(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 4)

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--depth", "1", url, "dst"); err != nil {
		t.Fatalf("shallow clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	countCommits := func() int {
		log, _ := v2run(t, dst, nil, "log", "--format=%s")
		return strings.Count(strings.TrimSpace(log), "\n") + 1
	}
	if got := countCommits(); got != 1 {
		t.Fatalf("clone has %d commits, want 1", got)
	}

	out, err := v2run(t, dst, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=true", "fetch", "--depth", "3", "origin", "main")
	t.Logf("deepen to 3:\n%s", out)
	if err != nil {
		t.Fatalf("deepen failed: %v", err)
	}
	if out, err := v2run(t, dst, nil, "checkout", "-q", "FETCH_HEAD"); err != nil {
		t.Fatalf("checkout FETCH_HEAD: %v\n%s", err, out)
	}
	if got := countCommits(); got != 3 {
		t.Errorf("after --depth 3 the clone has %d commits, want 3", got)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck after deepen: %v\n%s", err, out)
	}

	// --unshallow asks for everything and must clear the shallow marker.
	out, err = v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"fetch", "--unshallow", "origin", "main")
	t.Logf("unshallow:\n%s", out)
	if err != nil {
		t.Fatalf("unshallow failed: %v", err)
	}
	if out, _ := v2run(t, dst, nil, "rev-parse", "--is-shallow-repository"); !strings.Contains(out, "false") {
		t.Errorf("repository still shallow after --unshallow: %q", out)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck after unshallow: %v\n%s", err, out)
	}
	if got := countCommits(); got != 4 {
		t.Errorf("after --unshallow the clone has %d commits, want 4", got)
	}
}

// --- single branch ---------------------------------------------------------

// TestV2SingleBranch covers ls-refs narrowing. The client names the prefixes it
// cares about and the advertisement is cut down to them, which the simple
// protocol had no way to express: `ref-prefix` is an ls-refs argument, so the
// old `list` output was always every ref in the repository.
func TestV2SingleBranch(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 2)
	git(t, src, "-C", src, "checkout", "-q", "-b", "sibling")
	if err := os.WriteFile(filepath.Join(src, "s.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "sib")
	git(t, src, "-C", src, "push", "-q", url, "sibling")

	parent := t.TempDir()
	out, err := v2run(t, parent, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=true", "clone", "--single-branch", "--branch", "sibling", url, "dst")
	t.Logf("single-branch clone:\n%s", out)
	if err != nil {
		t.Fatalf("single-branch clone failed: %v", err)
	}
	log, _ := v2run(t, filepath.Join(parent, "dst"), nil, "log", "--format=%s")
	if !strings.Contains(log, "sib") {
		t.Errorf("wrong branch cloned: %q", log)
	}
}

// --- push still works ------------------------------------------------------

// TestV2PushStillWorks pins the boundary of what is served. Only
// git-upload-pack goes through protocol v2; a push must still be declined and
// fall through to the simple `push` command, on a helper where v2 is enabled
// and has already served a fetch.
func TestV2PushStillWorks(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 2)

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", url, "dst"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")
	if err := os.WriteFile(filepath.Join(dst, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := v2run(t, dst, nil, "add", "."); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := v2run(t, dst, nil, "commit", "-q", "-m", "pushed"); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	out, err := v2run(t, dst, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=true", "push", "origin", "main")
	t.Logf("push over v2-enabled helper:\n%s", out)
	if err != nil {
		t.Fatalf("push failed: %v", err)
	}
}

// --- empty repository ---------------------------------------------------------

// TestV2CloneEmptyRepository covers the advertisement with nothing to
// advertise. An empty ls-refs response and a fetch that asks for nothing still
// have to be well-formed sections, or git waits for bytes that never arrive.
func TestV2CloneEmptyRepository(t *testing.T) {
	url := v2setup(t)

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", url, "dst")
	t.Logf("clone of an empty repository:\n%s", out)
	if err != nil {
		t.Fatalf("clone of an empty repository failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "dst", ".git")); err != nil {
		t.Errorf("no repository was created: %v", err)
	}
}

// --- partial and shallow together ---------------------------------------------

// TestV2PartialShallowClone combines the two features that each rewrite what
// goes into the pack. The lazy fetches afterwards are the interesting part: they
// arrive carrying both a filter and the clone's shallow state, while asking for
// blobs — objects with no ancestry to measure a depth against.
func TestV2PartialShallowClone(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 3)

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--depth", "1", "--filter=blob:none", url, "dst")
	t.Logf("partial shallow clone:\n%s", out)
	if err != nil {
		t.Fatalf("partial shallow clone failed: %v", err)
	}
	dst := filepath.Join(parent, "dst")

	// Checking out drives the lazy fetch of the blobs the filter omitted.
	if out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"checkout", "-f", "main"); err != nil {
		t.Fatalf("lazy fetch in a shallow partial clone failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "fc.txt")); err != nil {
		t.Errorf("the tip's blob was never materialised: %v", err)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- tags ---------------------------------------------------------------------

// TestV2Tags covers the ref advertisement's one genuinely tricky case. An
// annotated tag's ref points at a *tag object*, which peels to a commit; the
// simple `list` output could only ever report the commit, because that interface
// has no peel form. ls-refs has one, so the two ids have to be told apart — and
// getting them the wrong way round produces a clone that looks right until
// something asks the tag what it points at.
func TestV2Tags(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 2)

	git(t, src, "-C", src, "tag", "light")
	git(t, src, "-C", src, "tag", "-a", "annotated", "-m", "an annotated tag")
	git(t, src, "-C", src, "push", "-q", url, "light", "annotated")

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", url, "dst")
	t.Logf("clone with tags:\n%s", out)
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}
	dst := filepath.Join(parent, "dst")

	tags, _ := v2run(t, dst, nil, "tag", "--list")
	for _, want := range []string{"light", "annotated"} {
		if !strings.Contains(tags, want) {
			t.Errorf("tag %q was not cloned; got %q", want, tags)
		}
	}

	// The annotated tag must have arrived as a tag object, not flattened to the
	// commit it peels to.
	kind, err := v2run(t, dst, nil, "cat-file", "-t", "annotated")
	if err != nil || strings.TrimSpace(kind) != "tag" {
		t.Errorf("annotated tag is %q (err=%v), want a tag object", strings.TrimSpace(kind), err)
	}
	msg, _ := v2run(t, dst, nil, "for-each-ref", "--format=%(contents)", "refs/tags/annotated")
	if !strings.Contains(msg, "an annotated tag") {
		t.Errorf("tag message did not survive: %q", msg)
	}
	// And it must peel to the same commit the branch is on.
	peeled, _ := v2run(t, dst, nil, "rev-parse", "annotated^{commit}")
	head, _ := v2run(t, dst, nil, "rev-parse", "HEAD")
	if strings.TrimSpace(peeled) != strings.TrimSpace(head) {
		t.Errorf("annotated tag peels to %q, want %q", strings.TrimSpace(peeled), strings.TrimSpace(head))
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// TestV2TagsOnlyRepository covers a repository whose branches are all gone but
// whose tags remain. The recorded default branch is cleared when the ref it
// names is deleted, so there is no HEAD to advertise and none is invented — the
// clone succeeds with the tags and no checked-out branch.
func TestV2TagsOnlyRepository(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 1)

	git(t, src, "-C", src, "tag", "v1")
	git(t, src, "-C", src, "push", "-q", url, "v1")
	if out, err := v2run(t, src, nil, "push", "-q", url, ":main"); err != nil {
		t.Skipf("this registry fixture does not support ref deletion: %v\n%s", err, out)
	}

	parent := t.TempDir()
	out, err := v2run(t, parent, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=true", "clone", url, "dst")
	t.Logf("clone of a repository with no branches:\n%s", out)
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}
	// Nothing may be advertised as HEAD: the branch it named is gone, and
	// pointing at a ref that is not there is worse than pointing at nothing.
	if strings.Contains(out, " HEAD") && !strings.Contains(out, "ref-prefix HEAD") {
		t.Errorf("a HEAD was advertised for a repository with no branches:\n%s", out)
	}
	dst := filepath.Join(parent, "dst")
	tags, _ := v2run(t, dst, nil, "tag", "--list")
	if !strings.Contains(tags, "v1") {
		t.Errorf("the surviving tag was not cloned: %q", tags)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// TestV2PartialCloneManyRefs is the case the ref-by-ref search exists for. A
// lazy fetch has no pack-base graph to follow from a blob, so history has to be
// staged and searched; staging all of it works and costs the whole repository
// on every checkout. With several branches present, the fetch must still be
// correct — the search stops at the first ref that resolves the wants, and
// stopping at the wrong point would leave objects missing rather than merely
// cost time.
func TestV2PartialCloneManyRefs(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 2)

	// Several unrelated branches, each with its own blob, pushed separately.
	for _, branch := range []string{"alpha", "beta", "gamma"} {
		git(t, src, "-C", src, "checkout", "-q", "-b", branch, "main")
		if err := os.WriteFile(filepath.Join(src, branch+".txt"), []byte(branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "-C", src, "add", ".")
		git(t, src, "-C", src, "commit", "-q", "-m", "on "+branch)
		git(t, src, "-C", src, "push", "-q", url, branch)
	}
	git(t, src, "-C", src, "checkout", "-q", "main")

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--filter=blob:none", url, "dst"); err != nil {
		t.Fatalf("partial clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	// The default branch first: its blobs should be reachable without the
	// search ever reaching the other branches.
	if out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"checkout", "-f", "main"); err != nil {
		t.Fatalf("lazy fetch on main: %v\n%s", err, out)
	}

	// And a blob that is only on a non-default branch, which the search has to
	// keep going to find.
	if out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"checkout", "-f", "gamma"); err != nil {
		t.Fatalf("lazy fetch on gamma: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dst, "gamma.txt"))
	if err != nil || strings.TrimSpace(string(body)) != "gamma" {
		t.Errorf("gamma.txt = %q (err=%v), want \"gamma\"", string(body), err)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- negotiation ---------------------------------------------------------------

// TestV2IncrementalFetchIsMinimal pins the thing that makes a single round of
// negotiation sufficient here.
//
// The server answers `ready` on the first batch of `have` lines rather than
// trading more, which looks like it should over-send: the batch is only git's
// most recent commits, and the common ancestor may be far older. It does not,
// because exclusion is transitive. `^<have>` removes everything reachable from
// that commit, and a client's recent commits descend from the shared history,
// so excluding them excludes all of it.
//
// The client here has diverged by twenty local commits on top of twenty-five
// shared ones, so every have it offers first is a commit the remote has never
// seen. Only the one genuinely new commit and its tree and blob may cross.
func TestV2IncrementalFetchIsMinimal(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 25)

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", url, "dst"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	countObjects := func() int {
		t.Helper()
		out, _ := v2run(t, dst, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
		return len(strings.Fields(out))
	}

	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(dst, "local.txt"), []byte(strings.Repeat("l", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := v2run(t, dst, nil, "add", "."); err != nil {
			t.Fatalf("add: %v\n%s", err, out)
		}
		if out, err := v2run(t, dst, nil, "commit", "-q", "-m", "local"); err != nil {
			t.Fatalf("local commit: %v\n%s", err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "the one new commit")
	git(t, src, "-C", src, "push", "-q", url, "main")

	before := countObjects()
	if out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"fetch", "origin"); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	gained := countObjects() - before

	// One commit, its tree, its blob. The slack is for git's own bookkeeping;
	// what must not happen is the shared history arriving again, which would
	// be tens of objects.
	if gained < 3 || gained > 8 {
		t.Errorf("the fetch brought %d objects, want about 3 — negotiation is not cutting the history it should", gained)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// --- Git LFS ------------------------------------------------------------------

// TestV2ClonesLFSObjects covers the one thing a v2 fetch has to leave behind.
//
// A packfile carries the LFS *pointer* — a hundred bytes of text naming an
// object stored beside it — and nothing else. The object itself is a separate
// registry layer, and it has to be written into the client's own
// .git/lfs/objects, because that is where git-lfs looks and an oci:// remote
// has no LFS server to ask later. The v2 path stages packfiles into a temporary
// directory it then discards, so it took the pointers and dropped the objects:
// the clone succeeded, fsck passed, and the working tree held stubs.
//
// Both paths are exercised, because the simple one was always right and the
// point is that they now agree.
func TestV2ClonesLFSObjects(t *testing.T) {
	for _, tc := range []struct {
		name string
		v2   bool
	}{
		{"simple protocol", false},
		{"protocol v2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := v2setup(t)

			src := t.TempDir()
			git(t, src, "init", "-q", "-b", "main", src)

			payload := []byte("a large binary payload, stored out of band")
			sum := sha256.Sum256(payload)
			oid := hex.EncodeToString(sum[:])
			pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
				oid, len(payload))
			if err := os.WriteFile(filepath.Join(src, "big.bin"), []byte(pointer), 0o644); err != nil {
				t.Fatal(err)
			}
			objDir := filepath.Join(src, ".git", "lfs", "objects", oid[0:2], oid[2:4])
			if err := os.MkdirAll(objDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(objDir, oid), payload, 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, src, "-C", src, "add", ".")
			git(t, src, "-C", src, "commit", "-q", "-m", "an LFS pointer")
			git(t, src, "-C", src, "push", "-q", url, "main")

			parent := t.TempDir()
			args := []string{"-c", "protocol.version=2"}
			if tc.v2 {
				args = append(args, "-c", "ociremote.protocolV2=true")
			}
			args = append(args, "clone", url, "dst")
			if out, err := v2run(t, parent, nil, args...); err != nil {
				t.Fatalf("clone: %v\n%s", err, out)
			}

			stored := filepath.Join(parent, "dst", ".git", "lfs", "objects", oid[0:2], oid[2:4], oid)
			body, err := os.ReadFile(stored)
			if err != nil {
				t.Fatalf("the LFS object was not stored, so the working tree holds a pointer and nothing behind it: %v", err)
			}
			if !bytes.Equal(body, payload) {
				t.Errorf("stored LFS object is %q, want %q", body, payload)
			}
		})
	}
}

// --- default branch -----------------------------------------------------------

// TestSetHeadChangesWhatACloneChecksOut is the end of the story set-head exists
// for. The subcommand's own tests check the recorded annotation; this checks the
// thing anyone actually cares about — that a fresh clone lands on the branch the
// repository now says is its default, over both protocols.
func TestSetHeadChangesWhatACloneChecksOut(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 1)

	// A second branch, pushed after main, so main is the recorded default.
	git(t, src, "-C", src, "checkout", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(src, "t.txt"), []byte("t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "on trunk")
	git(t, src, "-C", src, "push", "-q", url, "trunk")

	checkedOut := func(dir string) string {
		t.Helper()
		out, _ := v2run(t, dir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		return strings.TrimSpace(out)
	}

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "clone", url, "before"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if got := checkedOut(filepath.Join(parent, "before")); got != "main" {
		t.Fatalf("clone checked out %q before set-head, want main", got)
	}

	// Move it.
	bin := filepath.Join(t.TempDir(), "git-remote-oci")
	if out, err := exec.CommandContext(t.Context(), "go", buildArgs(bin)...).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	setHead := exec.CommandContext(t.Context(), bin, "set-head", url, "trunk")
	setHead.Env = append(os.Environ(), "OCI_INSECURE=1")
	if out, err := setHead.CombinedOutput(); err != nil {
		t.Fatalf("set-head: %v\n%s", err, out)
	}

	// Both protocols read the default from the same recorded annotation, so
	// both have to follow it.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"simple protocol", nil},
		{"protocol v2", []string{"-c", "protocol.version=2", "-c", "ociremote.protocolV2=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := "after-" + strings.ReplaceAll(tc.name, " ", "-")
			args := append(append([]string{}, tc.args...), "clone", url, dst)
			if out, err := v2run(t, parent, nil, args...); err != nil {
				t.Fatalf("clone: %v\n%s", err, out)
			}
			if got := checkedOut(filepath.Join(parent, dst)); got != "trunk" {
				t.Errorf("clone checked out %q, want trunk", got)
			}
		})
	}
}

// --- deepen by date and by ref --------------------------------------------------

// TestV2ShallowSince covers `--shallow-since`, which cuts history by committer
// date rather than by counting generations. It was refused until now: serving it
// wrongly is worse than not serving it, because a client that records a boundary
// not describing the pack it received is quietly corrupt.
func TestV2ShallowSince(t *testing.T) {
	url := v2setup(t)
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", src)

	// Three commits, dated a week apart, so a cut between them is unambiguous.
	dates := []string{
		"2021-01-01T00:00:00+00:00",
		"2021-01-08T00:00:00+00:00",
		"2021-01-15T00:00:00+00:00",
	}
	for i, date := range dates {
		name := filepath.Join(src, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(name, []byte(date+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "-C", src, "add", ".")
		if out, err := v2run(t, src, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date},
			"commit", "-q", "-m", fmt.Sprintf("commit%d", i)); err != nil {
			t.Fatalf("commit: %v\n%s", err, out)
		}
	}
	git(t, src, "-C", src, "push", "-q", url, "main")

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--shallow-since=2021-01-05", url, "dst")
	t.Logf("shallow-since clone:\n%s", out)
	if err != nil {
		t.Fatalf("--shallow-since failed: %v", err)
	}
	dst := filepath.Join(parent, "dst")

	log, _ := v2run(t, dst, nil, "log", "--format=%s")
	// The two commits after the cut, and not the one before it.
	for _, want := range []string{"commit1", "commit2"} {
		if !strings.Contains(log, want) {
			t.Errorf("%s is missing from a --shallow-since clone: %q", want, log)
		}
	}
	if strings.Contains(log, "commit0") {
		t.Errorf("commit0 predates the cut and should not be there: %q", log)
	}
	if out, _ := v2run(t, dst, nil, "rev-parse", "--is-shallow-repository"); !strings.Contains(out, "true") {
		t.Errorf("--shallow-since did not produce a shallow repository: %q", out)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// TestV2ShallowExclude covers `--shallow-exclude`, which cuts at whatever a
// named ref reaches. The excluded ref's history is not reachable from the wants
// — that is the point — so it has to be staged as well, or the exclusion covers
// nothing and the client silently receives more than it asked for.
func TestV2ShallowExclude(t *testing.T) {
	url := v2setup(t)
	src := v2seed(t, url, 2)

	// Tag the history so far, then add a commit past it.
	git(t, src, "-C", src, "tag", "base")
	git(t, src, "-C", src, "push", "-q", url, "base")
	if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "past the tag")
	git(t, src, "-C", src, "push", "-q", url, "main")

	parent := t.TempDir()
	out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--shallow-exclude=base", "--single-branch", "--branch", "main", url, "dst")
	t.Logf("shallow-exclude clone:\n%s", out)
	if err != nil {
		t.Fatalf("--shallow-exclude failed: %v", err)
	}
	dst := filepath.Join(parent, "dst")

	log, _ := v2run(t, dst, nil, "log", "--format=%s")
	if !strings.Contains(log, "past the tag") {
		t.Errorf("the commit after the excluded ref is missing: %q", log)
	}
	if strings.Contains(log, "commita") {
		t.Errorf("history behind the excluded ref was sent anyway: %q", log)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// TestV2DeepenRelative covers `git fetch --deepen=<n>`, which counts from the
// boundary the client already has rather than from the tips.
//
// The numbers here are the ones a real server produces for the same history: a
// depth-1 clone of five commits, deepened by two, holds three commits and is
// shallow at the third. Getting the off-by-one wrong is the whole risk — the
// boundary commit is level zero of the walk and the client already has it, so
// n more generations is n+1 levels.
func TestV2DeepenRelative(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 5)

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--depth", "1", url, "dst"); err != nil {
		t.Fatalf("shallow clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	// Counted from HEAD, which each deepening checks out afresh from
	// FETCH_HEAD — a fetch does not move the branch a shallow clone is on.
	commits := func() []string {
		t.Helper()
		out, err := v2run(t, dst, nil, "log", "--format=%s")
		if err != nil {
			t.Fatalf("git log: %v\n%s", err, out)
		}
		return strings.Fields(strings.TrimSpace(out))
	}
	if got := commits(); len(got) != 1 {
		t.Fatalf("the clone holds %v, want one commit", got)
	}

	out, err := v2run(t, dst, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"-c", "ociremote.protocolV2=true", "fetch", "--deepen=2", "origin", "main")
	t.Logf("deepen by two:\n%s", out)
	if err != nil {
		t.Fatalf("--deepen=2 failed: %v", err)
	}
	// The request has to have been the relative one, or this is really just a
	// test of --depth under another name.
	if !strings.Contains(out, "deepen-relative") {
		t.Errorf("the client did not send deepen-relative; this is not exercising the relative path")
	}

	if out, err := v2run(t, dst, nil, "checkout", "-q", "FETCH_HEAD"); err != nil {
		t.Fatalf("checkout FETCH_HEAD: %v\n%s", err, out)
	}
	// One it had, plus the two it asked for.
	if got := commits(); len(got) != 3 {
		t.Errorf("after --deepen=2 the clone holds %d commits (%v), want 3", len(got), got)
	}
	if out, _ := v2run(t, dst, nil, "rev-parse", "--is-shallow-repository"); !strings.Contains(out, "true") {
		t.Errorf("deepening should leave the repository shallow, not complete: %q", out)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck after deepening: %v\n%s", err, out)
	}

	// And again, to check the second deepen counts from the new boundary
	// rather than the original one.
	if out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"fetch", "--deepen=1", "origin", "main"); err != nil {
		t.Fatalf("second --deepen=1 failed: %v\n%s", err, out)
	}
	if out, err := v2run(t, dst, nil, "checkout", "-q", "FETCH_HEAD"); err != nil {
		t.Fatalf("checkout FETCH_HEAD: %v\n%s", err, out)
	}
	if got := commits(); len(got) != 4 {
		t.Errorf("after a further --deepen=1 the clone holds %d commits (%v), want 4", len(got), got)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck after the second deepening: %v\n%s", err, out)
	}
}

// --- object-info -----------------------------------------------------------

// git does not currently send object-info. The client-side series that would
// use it for `cat-file --batch-check` against a promisor remote was proposed
// upstream and has not landed, so git 2.53 answers such a query by lazily
// *fetching* the object instead. That makes this a command with no client yet
// — which is a reason to test it by speaking the protocol rather than a reason
// not to serve it: the capability is advertised, so anything that does
// implement the client half will use it, and a server that advertises a command
// it gets wrong is worse than one that never offered it.

// pktLine frames a payload the way gitprotocol-common(5) says: four hex digits
// of total length, then the bytes. Written out here rather than reusing the
// helper's own writer, so the test does not agree with the code under test
// about what a packet is.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

const pktFlush = "0000"
const pktDelim = "0001"

// TestV2ObjectInfoServesSizes speaks object-info to the helper directly.
func TestV2ObjectInfoServesSizes(t *testing.T) {
	url := v2setup(t)
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", src)

	const size = 4321
	if err := os.WriteFile(filepath.Join(src, "big.txt"), []byte(strings.Repeat("z", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "one big blob")
	git(t, src, "-C", src, "push", "-q", url, "main")
	blob := git(t, src, "-C", src, "rev-parse", "HEAD:big.txt")

	// A fresh clone with nothing in it, so the size cannot come from local
	// objects and has to be found in the registry.
	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2",
		"clone", "--filter=blob:none", "--no-checkout", url, "dst"); err != nil {
		t.Fatalf("partial clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")
	t.Setenv("GIT_DIR", filepath.Join(dst, ".git"))

	script := "stateless-connect git-upload-pack\n" +
		pktLine("command=object-info\n") +
		pktLine("object-format=sha1\n") +
		pktDelim +
		pktLine("size\n") +
		pktLine("oid "+blob+"\n") +
		pktFlush

	out, err := runHelper(t, url, script)
	if err != nil {
		t.Fatalf("stateless-connect object-info: %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("%s %d", blob, size)) {
		t.Errorf("object-info did not report %s as %d bytes:\n%q", blob, size, out)
	}
	// The attribute line has to come back before the sizes, or the client
	// cannot tell what the numbers are.
	if !strings.Contains(out, pktLine("size\n")) && !strings.Contains(out, "size") {
		t.Errorf("no size attribute line in the response:\n%q", out)
	}
}

// TestV2ObjectInfoAnswersWithoutFetchingAPackfile is the point of putting
// sizes in the index.
//
// Answering used to mean downloading the packfile holding the object and
// measuring it, which for a client trying to decide whether it *wants* the
// object is precisely backwards. The size is now published beside the pack, so
// the answer costs an index read and no history at all.
func TestV2ObjectInfoAnswersWithoutFetchingAPackfile(t *testing.T) {
	url, reg := v2setupRegistry(t)
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", src)

	const size = 4321
	if err := os.WriteFile(filepath.Join(src, "big.txt"), []byte(strings.Repeat("z", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "one big blob")
	git(t, src, "-C", src, "push", "-q", url, "main")
	blob := git(t, src, "-C", src, "rev-parse", "HEAD:big.txt")

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2",
		"clone", "--filter=blob:none", "--no-checkout", url, "dst"); err != nil {
		t.Fatalf("partial clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")
	t.Setenv("GIT_DIR", filepath.Join(dst, ".git"))

	packfile := packfileLayerOf(t, reg, "refs/heads/main")
	mark := len(reg.Requests())

	script := "stateless-connect git-upload-pack\n" +
		pktLine("command=object-info\n") +
		pktLine("object-format=sha1\n") +
		pktDelim +
		pktLine("size\n") +
		pktLine("oid "+blob+"\n") +
		pktFlush

	out, err := runHelper(t, url, script)
	if err != nil {
		t.Fatalf("object-info: %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("%s %d", blob, size)) {
		t.Fatalf("object-info did not report %s as %d bytes:\n%q", blob, size, out)
	}

	for _, req := range reg.Requests()[mark:] {
		if strings.HasPrefix(req, "GET ") && strings.HasSuffix(req, "/blobs/"+packfile) {
			t.Errorf("object-info downloaded the packfile to answer a size question; " +
				"the size is published in the index precisely so it need not")
		}
	}
}

// TestV2ObjectInfoRejectsUnknownAttributes: only `size` is advertised, and
// answering a request for something else with sizes anyway would be a response
// the client cannot parse. An ERR packet names the problem; going quiet reaches
// the user as "the remote end hung up".
func TestV2ObjectInfoRejectsUnknownAttributes(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 1)

	script := "stateless-connect git-upload-pack\n" +
		pktLine("command=object-info\n") +
		pktDelim +
		pktLine("mtime\n") +
		pktFlush

	out, _ := runHelper(t, url, script)
	if !strings.Contains(out, "ERR") || !strings.Contains(out, "size") {
		t.Errorf("an unsupported attribute should come back as an ERR mentioning what is supported:\n%q", out)
	}
}

// TestV2ObjectInfoIsAdvertised: the capability has to be offered, or git never
// sends the command and everything behind it is unreachable.
func TestV2ObjectInfoIsAdvertised(t *testing.T) {
	url := v2setup(t)
	v2seed(t, url, 1)

	parent := t.TempDir()
	out, err := v2run(t, parent, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version=2",
		"clone", url, "dst")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if !strings.Contains(out, "object-info") {
		t.Errorf("object-info was not advertised:\n%s", out)
	}
}
