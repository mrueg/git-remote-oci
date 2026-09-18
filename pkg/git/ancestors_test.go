package git_test

import (
	"bytes"
	"fmt"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// TestAncestorsAmong pins the one-walk answer against IsAncestor's per-call
// answer: the two must agree on every candidate, and the set must treat the
// descendant as its own ancestor the way IsAncestor does.
func TestAncestorsAmong(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit(t, "a.txt", "a\n", "first")
	second := r.commit(t, "a.txt", "b\n", "second")
	third := r.commit(t, "a.txt", "c\n", "third")
	absent := plumbing.NewHash("0123456789012345678901234567890123456789")

	candidates := []plumbing.Hash{first, second, third, absent}
	got, err := r.repo.AncestorsAmong(third, candidates)
	if err != nil {
		t.Fatalf("AncestorsAmong: %v", err)
	}
	for _, c := range candidates {
		want, err := r.repo.IsAncestor(c, third)
		if err != nil {
			t.Fatalf("IsAncestor(%s): %v", c, err)
		}
		if got[c] != want {
			t.Errorf("AncestorsAmong reports %s as ancestor=%v, IsAncestor says %v", c, got[c], want)
		}
	}
	if got[absent] {
		t.Error("a commit that is not in the repository was reported as an ancestor")
	}

	// A descendant that is not an ancestor of the candidates: the walk from
	// `first` reaches neither later commit.
	got, err = r.repo.AncestorsAmong(first, []plumbing.Hash{second, third})
	if err != nil {
		t.Fatalf("AncestorsAmong: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("walking from the root found ancestors among its descendants: %v", got)
	}

	// No candidates: nothing to walk for, and nothing found.
	got, err = r.repo.AncestorsAmong(third, nil)
	if err != nil {
		t.Fatalf("AncestorsAmong with no candidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("no candidates, yet %d found", len(got))
	}

	// An unknown descendant cannot be walked at all, as with IsAncestor.
	if _, err := r.repo.AncestorsAmong(absent, []plumbing.Hash{first}); err == nil {
		t.Error("AncestorsAmong succeeded for an absent descendant")
	}
}

// TestCreatePackfileFromListMatchesCreatePackfile: handing the packer a
// precomputed list changes nothing about the pack it writes.
func TestCreatePackfileFromListMatchesCreatePackfile(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit(t, "a.txt", "a\n", "first")
	second := r.commit(t, "a.txt", "b\n", "second")

	objects, err := r.repo.RevList(second, []plumbing.Hash{first})
	if err != nil {
		t.Fatalf("RevList: %v", err)
	}
	if len(objects) == 0 {
		t.Fatal("RevList listed nothing for a range with one new commit")
	}

	var plain, listed bytes.Buffer
	if err := r.repo.CreatePackfileTo(&plain, second, []plumbing.Hash{first}); err != nil {
		t.Fatalf("CreatePackfileTo: %v", err)
	}
	if err := r.repo.CreatePackfileFromListTo(&listed, second, []plumbing.Hash{first}, objects); err != nil {
		t.Fatalf("CreatePackfileFromListTo: %v", err)
	}
	if plain.Len() == 0 || listed.Len() == 0 {
		t.Fatalf("empty pack: plain=%d listed=%d bytes", plain.Len(), listed.Len())
	}
	// Both go through git pack-objects on the thin path, so the bytes agree.
	if !bytes.Equal(plain.Bytes(), listed.Bytes()) {
		t.Errorf("thin packs differ: plain=%d bytes, listed=%d bytes", plain.Len(), listed.Len())
	}

	// The list only matters to the pure-Go fallback, reached when git cannot
	// be run at all. With nothing on PATH both calls take it: one walks for
	// its own list, the other encodes the one it was handed, and the packs
	// must still agree.
	t.Setenv("PATH", t.TempDir())
	plain.Reset()
	listed.Reset()
	if err := r.repo.CreatePackfileTo(&plain, second, []plumbing.Hash{first}); err != nil {
		t.Fatalf("CreatePackfileTo (fallback): %v", err)
	}
	if err := r.repo.CreatePackfileFromListTo(&listed, second, []plumbing.Hash{first}, objects); err != nil {
		t.Fatalf("CreatePackfileFromListTo (fallback): %v", err)
	}
	if plain.Len() == 0 || !bytes.Equal(plain.Bytes(), listed.Bytes()) {
		t.Errorf("fallback packs differ: plain=%d bytes, listed=%d bytes", plain.Len(), listed.Len())
	}
}

// benchRepo is a history shaped like a repository with many branches: a
// main line of 200 commits and branches diverged from its root, which is
// the case where per-ref ancestor checks are worst -- each branch tip is not
// an ancestor of main's tip, so IsAncestor walks the whole main line to say
// no.
func benchRepo(b *testing.B, branches int) (*testRepo, plumbing.Hash, []plumbing.Hash) {
	b.Helper()
	const mainLen = 200
	r := newTestRepo(b)
	root := r.commit(b, "a.txt", "root\n", "root")
	tips := make([]plumbing.Hash, 0, branches)
	for i := range branches {
		r.checkout(b, root)
		tips = append(tips, r.commit(b, fmt.Sprintf("b%d.txt", i), "x\n", fmt.Sprintf("branch %d", i)))
	}
	r.checkout(b, root)
	tip := root
	for i := range mainLen {
		tip = r.commit(b, "a.txt", fmt.Sprintf("%d\n", i), fmt.Sprintf("main %d", i))
	}
	return r, tip, tips
}

// checkout moves the worktree to a commit, detached, so the next commit
// starts a new line of history from it.
func (r *testRepo) checkout(tb testing.TB, h plumbing.Hash) {
	tb.Helper()
	if err := r.wt.Checkout(&gogit.CheckoutOptions{Hash: h, Force: true}); err != nil {
		tb.Fatalf("Checkout(%s): %v", h, err)
	}
}

// BenchmarkIsAncestorPerRef is the old shape: one walk per remote ref.
func BenchmarkIsAncestorPerRef(b *testing.B) {
	r, tip, tips := benchRepo(b, 50)
	b.ResetTimer()
	for range b.N {
		for _, c := range tips {
			if _, err := r.repo.IsAncestor(c, tip); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkAncestorsAmong is the new shape: one walk for every remote ref.
func BenchmarkAncestorsAmong(b *testing.B) {
	r, tip, tips := benchRepo(b, 50)
	b.ResetTimer()
	for range b.N {
		if _, err := r.repo.AncestorsAmong(tip, tips); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPushWalksSeparate is the old shape of a push's object listing: the
// LFS scan and the pack index each walking the range.
func BenchmarkPushWalksSeparate(b *testing.B) {
	r, tip, _ := benchRepo(b, 0)
	b.ResetTimer()
	for range b.N {
		if _, err := r.repo.ScanLFSPointers(tip, nil); err != nil {
			b.Fatal(err)
		}
		if _, err := r.repo.PackedObjects(tip, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPushWalksShared is the new shape: one RevList, both consumers.
func BenchmarkPushWalksShared(b *testing.B) {
	r, tip, _ := benchRepo(b, 0)
	b.ResetTimer()
	for range b.N {
		objects, err := r.repo.RevList(tip, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := r.repo.ScanLFSPointersIn(objects); err != nil {
			b.Fatal(err)
		}
		_ = r.repo.PackedObjectsOf(objects)
	}
}
