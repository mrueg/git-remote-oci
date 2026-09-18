package git_test

import (
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestResolveRefPeelsNestedTags: a tag may point at another tag, and peeling
// one level then hands back a tag object where a commit was promised. That
// object id went into a push as the commit to publish.
func TestResolveRefPeelsNestedTags(t *testing.T) {
	r := newTestRepo(t)
	commit := r.commit(t, "a.txt", "a\n", "first")

	tagger := &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()}
	inner, err := r.git.CreateTag("v1", commit, &gogit.CreateTagOptions{Tagger: tagger, Message: "inner"})
	if err != nil {
		t.Fatalf("CreateTag v1: %v", err)
	}
	outer, err := r.git.CreateTag("v1-signed", inner.Hash(), &gogit.CreateTagOptions{Tagger: tagger, Message: "outer"})
	if err != nil {
		t.Fatalf("CreateTag v1-signed: %v", err)
	}
	if outer.Hash() == inner.Hash() || inner.Hash() == commit {
		t.Fatal("fixture error: the three objects must differ")
	}

	got, err := r.repo.ResolveRef("refs/tags/v1-signed")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != commit {
		t.Errorf("ResolveRef(refs/tags/v1-signed) = %s, want the commit %s (got the inner tag: %v)",
			got, commit, got == inner.Hash())
	}
	if got, err := r.repo.ResolveRef("v1-signed"); err != nil || got != commit {
		t.Errorf("ResolveRef(v1-signed) = %s (err=%v), want %s", got, err, commit)
	}
}
