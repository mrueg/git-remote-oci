package helper_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

// fetchWithinTimeout runs a fetch and fails if it does not return.
//
// The timeout is the assertion. Nothing in this tool carries a deadline — the
// top-level context is only cancelled by a signal — so a fetch that blocks
// blocks until the user notices and kills it.
func fetchWithinTimeout(t *testing.T, registry, script string, within time.Duration) error {
	t.Helper()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runHelper(t, registry, script)
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		return r.err
	case <-time.After(within):
		t.Fatalf("fetch did not return within %s; the pack-base graph deadlocked it", within)
		return nil
	}
}

// TestSelfReferentialPackBaseFailsRatherThanHanging is the regression test for
// a fetch that never returned.
//
// The walk used to recurse into each pack base and wait for it. A manifest
// naming itself as its own base made the goroutine holding that commit's claim
// wait on the claim it was itself responsible for completing, and because no
// operation carries a deadline the fetch blocked until the user killed it.
// Resolving the graph before importing turns that into a graph property, found
// by inspection instead of by hanging.
func TestSelfReferentialPackBaseFailsRatherThanHanging(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	tip := commitFile(t, src, "second.txt", "second\n", "commit two")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("push failed: %v (output %q)", err, out)
	}

	reg.SetPackBases(t, tip, tip)
	reg.SetPackBases(t, "main", tip)

	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)

	err := fetchWithinTimeout(t, registry, "list\nfetch "+tip+" refs/heads/main\n\n", 30*time.Second)
	if err == nil {
		t.Fatal("a manifest that is its own pack base was accepted")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should say the graph has a cycle, got: %v", err)
	}
}

// TestMutualPackBaseCycleFailsRatherThanHanging is the two-node case. The
// single-node one could be caught by a cheap self-reference check; this one
// cannot, and is why the fix is a topological sort rather than an equality
// test.
func TestMutualPackBaseCycleFailsRatherThanHanging(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	first := commitFile(t, src, "second.txt", "second\n", "commit two")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("first push failed: %v (output %q)", err, out)
	}
	tip := commitFile(t, src, "third.txt", "third\n", "commit three")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("second push failed: %v (output %q)", err, out)
	}

	// tip -> first is genuine; first -> tip closes the loop.
	reg.SetPackBases(t, first, tip)

	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)

	err := fetchWithinTimeout(t, registry, "list\nfetch "+tip+" refs/heads/main\n\n", 30*time.Second)
	if err == nil {
		t.Fatal("a mutual pack-base cycle was accepted")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should say the graph has a cycle, got: %v", err)
	}
}
