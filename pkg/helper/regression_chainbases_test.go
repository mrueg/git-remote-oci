package helper_test

import (
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// phantomBase is a well-formed commit id no registry in these tests serves.
const phantomBase = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// protocolPaths are the two fetch paths that resolve the pack graph: protocol
// v2, the default, and the simple path git falls back to. Both walk the chain.
var protocolPaths = []struct {
	name string
	args []string
}{
	{name: "v2", args: nil},
	{name: "simple", args: []string{"-c", "ociremote.protocolV2=false"}},
}

// TestPackChainNamingAPrunedManifestStillClones.
//
// The chain is advisory in both directions. A stale chain that omits an edge
// costs a round trip, which TestPackChainStaleIsCorrectedByTheAnnotations
// pins; a stale chain that names a manifest the registry no longer has -- what
// a client that outlived a gc republishes from the edges it remembered -- used
// to fail the whole fetch as "packed against ... could not be fetched", so
// every clone of the repository failed on a hint. Only a manifest's own
// pack-bases annotation is normative, and none here names the phantom.
func TestPackChainNamingAPrunedManifestStillClones(t *testing.T) {
	for _, path := range protocolPaths {
		t.Run(path.name, func(t *testing.T) {
			url, reg := v2setupRegistry(t)
			v2seedSeparatePushes(t, url, 4)

			corruptPackChain(t, reg, func(chain map[string][]string) map[string][]string {
				for sha, bases := range chain {
					chain[sha] = append(bases, phantomBase)
				}
				return chain
			})

			parent := t.TempDir()
			args := append(append([]string{}, path.args...), "clone", url, "dst")
			if out, err := v2run(t, parent, nil, args...); err != nil {
				t.Fatalf("clone failed because the published pack chain names a manifest the registry does not serve: %v\n%s", err, out)
			}
			dst := parent + "/dst"
			if out, err := v2run(t, dst, nil, "fsck"); err != nil {
				t.Fatalf("fsck after cloning past a phantom chain entry: %v\n%s", err, out)
			}
			if out, _ := v2run(t, dst, nil, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "4" {
				t.Errorf("cloned %s commits, want 4", strings.TrimSpace(out))
			}
		})
	}
}

// TestPackChainCannotExcuseADeclaredBase is the other half: the chain being
// advisory must not soften a base a manifest's own annotation declares. Here
// the chain and the annotation both name the dropped manifest, and the fetch
// has to fail the way it does with no chain at all.
//
// The simple path reports the broken dependency by name. Protocol v2 hands a
// failed graph to the promisor fallback, which ends in a generic refusal, so
// only the failure itself is asserted there.
func TestPackChainCannotExcuseADeclaredBase(t *testing.T) {
	for _, path := range protocolPaths {
		t.Run(path.name, func(t *testing.T) {
			url, reg := v2setupRegistry(t)
			v2seedSeparatePushes(t, url, 2)

			var commits []string
			for _, tag := range reg.Tags() {
				if oci.IsCommitID(tag) {
					commits = append(commits, tag)
				}
			}
			if len(commits) != 2 {
				t.Fatalf("fixture error: expected two commit tags, got %v", commits)
			}
			base := commits[0]
			if chain := packChainOf(t, reg); len(chain[commits[1]]) != 1 || chain[commits[1]][0] != base {
				t.Fatalf("fixture error: the chain should say the tip was packed against %s: %v", base, chain)
			}

			reg.DropManifest(base)

			parent := t.TempDir()
			args := append(append([]string{}, path.args...), "clone", url, "dst")
			out, err := v2run(t, parent, nil, args...)
			if err == nil {
				t.Fatal("clone succeeded even though the tip's declared pack base is gone; a declared base that cannot be fetched is an error, whatever the chain says")
			}
			if path.name == "simple" && !strings.Contains(out, "packed against") {
				t.Errorf("the failure should name the missing base, got:\n%s", out)
			}
		})
	}
}
