package helper_test

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

// blobBytesStored sums the packfile blobs the registry holds.
func blobBytesStored(reg *registrytest.Registry) int {
	total := 0
	for _, b := range reg.Blobs() {
		total += len(b)
	}
	return total
}

// bigFile builds a deterministic payload that does not compress to nothing.
func bigFile(seed uint64, size int) []byte {
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b9))
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte('a' + rng.IntN(26))
	}
	return buf
}

// TestThinPacksDeltaAgainstTheirBases pins that a small change to a large file
// costs roughly the change, not the file.
//
// go-git's encoder only deltifies within the object set it is handed, so a
// packfile cut against a base could never delta against that base: every push
// re-uploaded the whole file. Shelling out to `git pack-objects --thin` can,
// and it is safe precisely because pack-bases guarantees the reader has already
// imported those bases before this pack is indexed.
func TestThinPacksDeltaAgainstTheirBases(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	// Stated rather than assumed: this counts every byte the registry receives,
	// and a shallow snapshot would add a second self-contained copy of the tip.
	// That is a real cost but not this test's subject, which is whether the
	// *incremental* packfile deltas against its base.
	git(t, src, "config", "ociremote.shallowSnapshot", "false")

	const size = 512 * 1024
	original := bigFile(1, size)

	if err := os.WriteFile(filepath.Join(src, "big.bin"), original, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git(t, src, "add", "big.bin")
	git(t, src, "commit", "-q", "-m", "add a large file")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("first push failed: %v (output %q)", err, out)
	}
	afterFirst := blobBytesStored(reg)

	// Change a few bytes in the middle. The object is entirely new to git, but
	// almost all of its content is already on the registry.
	modified := bytes.Clone(original)
	copy(modified[size/2:], []byte("CHANGED"))
	if err := os.WriteFile(filepath.Join(src, "big.bin"), modified, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git(t, src, "add", "big.bin")
	git(t, src, "commit", "-q", "-m", "change seven bytes")
	tip := git(t, src, "rev-parse", "HEAD")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("second push failed: %v (output %q)", err, out)
	}
	secondPush := blobBytesStored(reg) - afterFirst

	t.Logf("first push stored %d bytes; the seven-byte change added %d", afterFirst, secondPush)

	// A non-thin pack re-stores the whole file. A thin one stores a delta.
	// Half the original is a generous bar that still fails loudly on a
	// regression to full copies.
	if secondPush > size/2 {
		t.Errorf("the second push added %d bytes for a seven-byte change; "+
			"a thin pack should delta against the base, not re-store %d bytes", secondPush, size)
	}

	// And the result must still clone: a thin pack is only valid because its
	// bases are fetched first and git index-pack --fix-thin completes it.
	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)
	if out, err := runHelper(t, registry, "list\nfetch "+tip+" refs/heads/main\n\n"); err != nil {
		t.Fatalf("fetch of a thin pack failed: %v (output %q)", err, out)
	}
	assertComplete(t, dst, tip)

	got := runGitOut(t, dst, "cat-file", "-p", tip+":big.bin")
	if !bytes.Equal([]byte(got), modified) {
		t.Errorf("big.bin came back wrong: got %d bytes, want %d", len(got), len(modified))
	}
}
