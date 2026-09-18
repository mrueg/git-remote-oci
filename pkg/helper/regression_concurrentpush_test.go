package helper_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// TestConcurrentPushesToDifferentRefsKeepBoth drives two helpers against one
// registry at the same time, each pushing its own branch, and checks that the
// registry ends up holding both.
//
// Ref locks are per ref, so nothing serialises two pushes to different refs;
// the only thing between them is the compare-and-swap on the _refs index. That
// is the path that used to rewind a ref another client had advanced -- every
// push wrote the whole ref set it had listed at `list for-push` -- and it was
// covered only by the e2e benchmark. Under -race this also covers the client
// caches the two helpers do not share and the ones the registry fake does.
func TestConcurrentPushesToDifferentRefsKeepBoth(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	// Two branches with distinct tips, so each push carries its own packfile
	// and its own ref entry rather than the same commit twice.
	src := newWorkRepo(t)
	shaA := commitFile(t, src, "a.txt", "a\n", "commit on a")
	git(t, src, "branch", "a")
	git(t, src, "checkout", "-q", "-b", "b", "HEAD~1")
	shaB := commitFile(t, src, "b.txt", "b\n", "commit on b")
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	// Both helpers list first, so each has seen the (empty) remote before the
	// other's push lands: exactly the snapshot a rewind would be written from.
	type result struct {
		out string
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, ref := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			script := "list for-push\npush refs/heads/" + ref + ":refs/heads/" + ref + "\n\n"
			out, err := runHelper(t, registry, script)
			results[i] = result{out: out, err: err}
		}()
	}
	wg.Wait()

	// Acquiring the _refs index lock is check, write, read back -- the
	// distribution API has no compare-and-swap on a tag, which the README's
	// limitations table says out loud -- so two clients starting together can
	// both come out believing they hold it. Two pushes to different refs
	// against a fast registry hit that window readily. It used to make the
	// loser report its ref as not reliably visible although its index write
	// had landed; now the loser reads the published index back and either
	// confirms its entry or redoes the update, so both pushes must say "ok".
	for i, ref := range []string{"a", "b"} {
		if results[i].err != nil {
			t.Fatalf("push of refs/heads/%s failed: %v (output %q)", ref, results[i].err, results[i].out)
		}
		if out := results[i].out; !strings.Contains(out, "ok refs/heads/"+ref) {
			t.Errorf("push of refs/heads/%s did not report ok:\n%s", ref, out)
		}
	}

	// A fresh client, so nothing is answered from a cache either push filled.
	client, err := oci.NewClient(registry, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	for ref, want := range map[string]string{"refs/heads/a": shaA, "refs/heads/b": shaB} {
		entry, ok := refs[ref]
		if !ok {
			t.Errorf("%s is missing from the _refs index after the concurrent pushes: %v", ref, refs)
			continue
		}
		if entry.SHA != want {
			t.Errorf("%s in _refs is %s, want %s: one push rewound the other", ref, entry.SHA, want)
		}
	}

	// The ref tags are what a reader falls back to; they must agree.
	for ref, want := range map[string]string{"a": shaA, "b": shaB} {
		if got := shaOfRef(t, reg, ref); got != want {
			t.Errorf("ref tag for refs/heads/%s resolves to %s, want %s", ref, got, want)
		}
	}
}
