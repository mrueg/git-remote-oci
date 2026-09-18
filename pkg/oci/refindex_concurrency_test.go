package oci_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
	shaC = "3333333333333333333333333333333333333333"
)

// captureRefIndex pushes refs and returns the raw _refs manifest that produced.
//
// Snapshotting the bytes lets a test put the registry into a given state
// without going through PushRichRefIndex, which takes the index lock. A real
// competing writer can slip past that lock — it is advisory, and a registry has
// no compare-and-swap — so a test that acquires it properly could not reproduce
// the race at all; it would just block until the wait expired.
func captureRefIndex(t *testing.T, reg *registrytest.Registry, client *oci.Client, refs map[string]oci.RefEntry) []byte {
	t.Helper()

	if err := client.PushRichRefIndex(context.Background(), refs, nil); err != nil {
		t.Fatalf("PushRichRefIndex: %v", err)
	}
	return reg.RawManifest(t, oci.TagRefIndex)
}

// setRefIndex replaces the published _refs manifest without taking the lock.
func setRefIndex(reg *registrytest.Registry, raw []byte) {
	reg.PutManifest(oci.TagRefIndex, raw)
}

// TestRefIndexUpdateDetectsAConcurrentWriter pins the optimistic-concurrency
// check.
//
// The index lock cannot be relied upon: acquisition is check-then-write, so two
// clients can both believe they hold it. The loser used to silently overwrite
// the winner with a merge computed from state that was already stale.
//
// Here another writer lands *while* this one is merging. It must be noticed and
// the merge redone against the new state.
func TestRefIndexUpdateDetectsAConcurrentWriter(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	before := captureRefIndex(t, reg, client, map[string]oci.RefEntry{
		"refs/heads/main": {SHA: shaA},
	})
	after := captureRefIndex(t, reg, client, map[string]oci.RefEntry{
		"refs/heads/main":  {SHA: shaA},
		"refs/heads/other": {SHA: shaB},
	})

	// Rewind, then swap in the competing state the first time this push reads
	// the index during its merge.
	setRefIndex(reg, before)
	var fired atomic.Bool
	reg.Observe(func(method, path string) {
		if method == http.MethodGet && strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex) {
			if fired.CompareAndSwap(false, true) {
				setRefIndex(reg, after)
			}
		}
	})

	client.ClearManifestCache()
	if err := client.PushRichRefIndex(ctx, map[string]oci.RefEntry{"refs/heads/mine": {SHA: shaC}}, nil); err != nil {
		t.Fatalf("push under contention failed: %v", err)
	}

	reg.Observe(nil)
	client.ClearManifestCache()

	final, err := client.FetchRichRefIndex(ctx)
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	// Both writers' refs must survive: ours because we wrote it, theirs because
	// the retry re-merged against the state they had left behind.
	for ref, want := range map[string]string{
		"refs/heads/main":  shaA,
		"refs/heads/other": shaB,
		"refs/heads/mine":  shaC,
	} {
		got, ok := final[ref]
		if !ok {
			t.Errorf("%s was lost from the index; the concurrent writer was clobbered", ref)
			continue
		}
		if got.SHA != want {
			t.Errorf("%s = %s, want %s", ref, got.SHA, want)
		}
	}
}

// TestRefIndexUpdateReportsPersistentContention pins that losing repeatedly is
// reported rather than silently dropped.
func TestRefIndexUpdateReportsPersistentContention(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	// A distinct index state for every read the push will perform. Alternating
	// between two states is not enough: a single attempt reads the index more
	// than once, so an even number of flips lands back on the baseline and the
	// push sees no conflict at all.
	states := make([][]byte, 0, 16)
	for i := range 16 {
		sha := shaA[:38] + pad(i)
		states = append(states, captureRefIndex(t, reg, client, map[string]oci.RefEntry{"refs/heads/main": {SHA: sha}}))
	}

	setRefIndex(reg, states[0])
	var seq atomic.Int32
	reg.Observe(func(method, path string) {
		if method == http.MethodGet && strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex) {
			next := int(seq.Add(1))
			setRefIndex(reg, states[next%len(states)])
		}
	})

	client.ClearManifestCache()
	err := client.PushRichRefIndex(ctx, map[string]oci.RefEntry{"refs/heads/mine": {SHA: shaC}}, nil)
	if err == nil {
		t.Fatal("persistent contention should be reported, not silently ignored")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Errorf("error should say it gave up retrying, got: %v", err)
	}
}

// pad renders n as two hex digits, so a synthetic SHA stays 40 characters.
func pad(n int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(n/16)%16], hex[n%16]})
}
