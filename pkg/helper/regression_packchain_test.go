package helper_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Pack bases form a chain: each push's packfile is cut against the previous
// push's tip. Discovering that chain by reading one manifest's annotations at a
// time is strictly sequential — the reader cannot ask for link N+1 until link N
// arrives — so clone latency was linear in the number of pushes since the last
// gc, before a single byte of packfile moved.
//
// Publishing the whole graph on the `_refs` manifest removes the walk. The
// tests here are about *concurrency*, which is the only thing that changed:
// the same manifests are fetched either way, so counting requests proves
// nothing. Instead the registry refuses to answer until several commit-manifest
// requests are in flight at once. A sequential walk can never satisfy that.

var commitManifestPath = regexp.MustCompile(`/manifests/[0-9a-f]{40}$`)

// concurrencyBarrier holds every matching request until `want` of them are in
// flight together, then releases them all. A reader that fetches serially never
// reaches the barrier and each request is let through on the timeout instead,
// so the test fails on the recorded maximum rather than hanging.
type concurrencyBarrier struct {
	want    int
	timeout time.Duration

	mu       sync.Mutex
	inflight int
	max      int
	gate     chan struct{}
	opened   bool
}

func newConcurrencyBarrier(want int, timeout time.Duration) *concurrencyBarrier {
	return &concurrencyBarrier{want: want, timeout: timeout, gate: make(chan struct{})}
}

func (b *concurrencyBarrier) enter() {
	b.mu.Lock()
	b.inflight++
	if b.inflight > b.max {
		b.max = b.inflight
	}
	reached := b.inflight >= b.want && !b.opened
	if reached {
		b.opened = true
		close(b.gate)
	}
	b.mu.Unlock()

	select {
	case <-b.gate:
	case <-time.After(b.timeout):
	}

	b.mu.Lock()
	b.inflight--
	b.mu.Unlock()
}

func (b *concurrencyBarrier) peak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

// TestPackChainResolvesTheGraphInOneWave is the point of the whole thing.
//
// Five pushes make a chain five manifests deep. Fetching it used to be five
// sequential round trips; with the chain published, all five are requested at
// once. The registry here will not answer a commit-manifest request until three
// are in flight together, which a sequential walk cannot produce.
func TestPackChainResolvesTheGraphInOneWave(t *testing.T) {
	url, reg := v2setupRegistry(t)
	v2seedSeparatePushes(t, url, 5)

	// The chain has to have been published for any of this to be possible.
	// Checking it separately keeps "the writer wrote nothing" from being
	// reported as "the reader did not parallelise".
	chain := packChainOf(t, reg)
	if len(chain) < 5 {
		t.Fatalf("the _refs manifest publishes %d chain entries, want one per push:\n%v", len(chain), chain)
	}

	barrier := newConcurrencyBarrier(3, 3*time.Second)
	reg.Observe(func(method, path string) {
		if method == http.MethodGet && commitManifestPath.MatchString(path) {
			barrier.enter() // never answered here; the barrier only delays
		}
	})

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "clone", url, "dst"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	if peak := barrier.peak(); peak < 3 {
		t.Errorf("at most %d commit manifests were ever requested at once, want 3 or more:\n"+
			"the pack-base chain is being walked one link at a time, which is one "+
			"round trip per push before any packfile transfers", peak)
	}
}

// TestPackChainAbsentStillClones is the compatibility half: a repository
// written before the chain existed has no such layer, and the reader has to
// fall back to walking the annotations.
func TestPackChainAbsentStillClones(t *testing.T) {
	url, reg := v2setupRegistry(t)
	v2seedSeparatePushes(t, url, 4)

	stripPackChain(t, reg)

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "clone", url, "dst"); err != nil {
		t.Fatalf("clone from a repository with no published chain: %v\n%s", err, out)
	}
	if out, err := v2run(t, parent+"/dst", nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
	if out, _ := v2run(t, parent+"/dst", nil, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "4" {
		t.Errorf("cloned %s commits, want 4", strings.TrimSpace(out))
	}
}

// TestPackChainStaleIsCorrectedByTheAnnotations.
//
// The chain is a hint, and a hint that is believed without checking is a way to
// skip a packfile and produce a repository quietly missing objects. A chain
// that claims the tip stands alone must not stop the reader from following the
// tip's real pack-bases annotation.
func TestPackChainStaleIsCorrectedByTheAnnotations(t *testing.T) {
	url, reg := v2setupRegistry(t)
	v2seedSeparatePushes(t, url, 4)

	// Rewrite the chain to a lie: every commit self-contained, no edges at all.
	// A reader that trusts it fetches one packfile and stops.
	corruptPackChain(t, reg, func(chain map[string][]string) map[string][]string {
		lying := make(map[string][]string, len(chain))
		for sha := range chain {
			lying[sha] = nil
		}
		return lying
	})

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "clone", url, "dst"); err != nil {
		t.Fatalf("clone with a lying chain: %v\n%s", err, out)
	}
	dst := parent + "/dst"
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck after a lying chain: %v\n%s", err, out)
	}
	if out, _ := v2run(t, dst, nil, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "4" {
		t.Errorf("cloned %s commits, want 4: the reader believed the chain instead of "+
			"checking each manifest's own pack-bases", strings.TrimSpace(out))
	}
}

// --- helpers ---------------------------------------------------------------

// v2seedSeparatePushes builds a linear history pushed one commit at a time, so
// each commit gets its own manifest and the pack bases form a chain n deep. A
// single push of n commits would produce one manifest and no chain at all.
func v2seedSeparatePushes(t *testing.T, url string, n int) {
	t.Helper()
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", src)
	for i := 0; i < n; i++ {
		name := string(rune('a' + i))
		if err := os.WriteFile(filepath.Join(src, "f"+name+".txt"), []byte(strings.Repeat(name, 64)), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "-C", src, "add", ".")
		git(t, src, "-C", src, "commit", "-q", "-m", "commit "+name)
		git(t, src, "-C", src, "push", "-q", url, "main")
	}
}

// refsManifest returns the parsed `_refs` manifest.
func refsManifest(t *testing.T, reg *registrytest.Registry) ocispec.Manifest {
	t.Helper()
	raw, ok := reg.ManifestBytes(oci.TagRefIndex)
	if !ok {
		t.Fatal("no _refs manifest was ever pushed")
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("_refs manifest: %v", err)
	}
	return manifest
}

// packChainOf reads the published pack-base graph out of the registry.
func packChainOf(t *testing.T, reg *registrytest.Registry) map[string][]string {
	t.Helper()
	manifest := refsManifest(t, reg)
	for _, layer := range manifest.Layers {
		if layer.MediaType != oci.MediaTypePackChain {
			continue
		}
		var chain map[string][]string
		if err := json.Unmarshal(reg.BlobBytes(layer.Digest.String()), &chain); err != nil {
			t.Fatalf("the published chain is not readable: %v", err)
		}
		return chain
	}
	t.Fatal("the _refs manifest carries no pack chain layer")
	return nil
}

// stripPackChain rewrites `_refs` as a build without the chain would have.
func stripPackChain(t *testing.T, reg *registrytest.Registry) {
	t.Helper()
	if err := reg.StripLayers(oci.MediaTypePackChain); err != nil {
		t.Fatalf("StripLayers: %v", err)
	}
}

// corruptPackChain replaces the published chain with whatever transform returns.
func corruptPackChain(t *testing.T, reg *registrytest.Registry, transform func(map[string][]string) map[string][]string) {
	t.Helper()
	replacement, err := json.Marshal(transform(packChainOf(t, reg)))
	if err != nil {
		t.Fatal(err)
	}

	manifest := refsManifest(t, reg)
	for i, layer := range manifest.Layers {
		if layer.MediaType != oci.MediaTypePackChain {
			continue
		}
		manifest.Layers[i] = ocispec.Descriptor{
			MediaType: oci.MediaTypePackChain,
			Digest:    opencontainers.Digest(reg.PutBlob(replacement)),
			Size:      int64(len(replacement)),
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		reg.PutManifest(oci.TagRefIndex, raw)
		return
	}
	t.Fatal("no pack chain layer to corrupt")
}
