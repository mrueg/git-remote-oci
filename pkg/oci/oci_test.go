package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestOCIClientPushAndFetch(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	refName := "refs/heads/main"
	packfileData := []byte("mock-git-packfile-data")

	err = pushCommitImage(ctx, client, commitSHA, refName, true, packfileData)
	if err != nil {
		t.Fatalf("PushCommitImage failed: %v", err)
	}

	// Verify ListRefs
	refs, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if refs[refName] != commitSHA {
		t.Errorf("Expected ref %s -> %s, got %s", refName, commitSHA, refs[refName])
	}

	// Fetch Manifest
	manifest, err := client.FetchManifest(ctx, commitSHA)
	if err != nil {
		t.Fatalf("FetchManifest failed: %v", err)
	}
	if manifest.Annotations[ocispec.AnnotationRevision] != commitSHA {
		t.Errorf("Expected revision %s, got %s", commitSHA, manifest.Annotations[ocispec.AnnotationRevision])
	}

	// Fetch the packfile layer. The buffered FetchPackfileLayer was removed as
	// unused public API; the streaming form is what the helper actually calls.
	rc, err := client.FetchPackfileStream(ctx, manifest)
	if err != nil {
		t.Fatalf("FetchPackfileStream failed: %v", err)
	}
	fetchedData, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("reading the packfile stream failed: %v", err)
	}
	if !bytes.Equal(fetchedData, packfileData) {
		t.Errorf("Expected packfile content %v, got %v", packfileData, fetchedData)
	}
}

func TestPushCommitImageEmptyRefTag(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "1111111111111111111111111111111111111111"

	// Push without a ref tag
	err = pushCommitImage(ctx, client, commitSHA, "refs/heads/custom", false, []byte("pack"))
	if err != nil {
		t.Fatalf("PushCommitImage failed: %v", err)
	}

	// Verify "latest" tag was NOT published
	_, err = client.FetchManifest(ctx, "latest")
	if err == nil {
		t.Errorf("Expected error fetching 'latest' tag, but 'latest' tag was published!")
	}
}

func TestPushCommitStreamSizeValidation(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "2222222222222222222222222222222222222222"

	// 1. Short read (expected 10 bytes, provided 5)
	err = client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", WriteRefTag: true}, bytes.NewReader([]byte("12345")), 10)
	if err == nil || !strings.Contains(err.Error(), "packfile size mismatch") {
		t.Errorf("Expected short read error, got: %v", err)
	}

	// 2. Excess bytes (expected 5 bytes, provided 10)
	err = client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", WriteRefTag: true}, bytes.NewReader([]byte("1234567890")), 5)
	if err == nil || !strings.Contains(err.Error(), "exceeds expected size") {
		t.Errorf("Expected excess bytes error, got: %v", err)
	}

	// 3. Exact match (expected 5 bytes, provided 5)
	err = client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", WriteRefTag: true}, bytes.NewReader([]byte("12345")), 5)
	if err != nil {
		t.Errorf("Expected success on exact size match, got: %v", err)
	}
}

func TestPushCommitImageInvalidSHA(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()

	// Invalid SHA (too short)
	err = pushCommitImage(ctx, client, "invalid-sha", "refs/heads/main", true, []byte("pack"))
	if err == nil || !strings.Contains(err.Error(), "invalid commit SHA") {
		t.Errorf("Expected invalid commit SHA error, got: %v", err)
	}
}

func TestPushCommitImageTagCollision(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "3333333333333333333333333333333333333333"

	// Push a ref whose name encodes to the commit id itself
	refName := "refs/heads/" + commitSHA
	err = pushCommitImage(ctx, client, commitSHA, refName, true, []byte("pack-data"))
	if err != nil {
		t.Fatalf("PushCommitImage failed: %v", err)
	}

	// Verify commitSHA tag manifest does NOT contain AnnotationGitRef (remains ref-agnostic)
	commitManifest, err := client.FetchManifest(ctx, commitSHA)
	if err != nil {
		t.Fatalf("FetchManifest for commitSHA failed: %v", err)
	}
	if commitManifest.Annotations[oci.AnnotationGitRef] != "" {
		t.Errorf("Expected commitSHA tag manifest to be ref-agnostic, but found AnnotationGitRef %q", commitManifest.Annotations[oci.AnnotationGitRef])
	}

	// Verify ListRefs still surfaces the ref
	refs, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if refs[refName] != commitSHA {
		t.Errorf("Expected ref %s -> %s, got %s", refName, commitSHA, refs[refName])
	}

	// Push a ref whose name encodes to a DIFFERENT 40-hex SHA string
	differentSHA := "4444444444444444444444444444444444444444"
	diffRefName := "refs/heads/" + differentSHA
	err = pushCommitImage(ctx, client, commitSHA, diffRefName, true, []byte("pack-data-2"))
	if err != nil {
		t.Fatalf("PushCommitImage for a different 40-hex ref name failed: %v", err)
	}

	refs2, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if refs2[diffRefName] != commitSHA {
		t.Errorf("Expected different 40-hex ref %s -> %s, got %s", diffRefName, commitSHA, refs2[diffRefName])
	}
}

func TestFetchManifestMediaTypeWithParameters(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	// A registry that decorates the media type with a parameter. The shared
	// fake answers with the bare type, so this one request is answered here.
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: ocispec.Manifest{}.Versioned,
		Annotations: map[string]string{
			ocispec.AnnotationRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/manifests/v1.0.0") {
			return false
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json; charset=utf-8")
		w.Header().Set("Docker-Content-Digest", registrytest.Digest(manifest))
		_, _ = w.Write(manifest)
		return true
	})

	client := registrytest.Client(t, ts)
	ctx := context.Background()
	fetched, err := client.FetchManifest(ctx, "v1.0.0")
	if err != nil {
		t.Fatalf("FetchManifest failed for Content-Type with parameters: %v", err)
	}
	if fetched.Annotations[ocispec.AnnotationRevision] != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("Unexpected revision in manifest: %v", fetched.Annotations[ocispec.AnnotationRevision])
	}
}

func TestListRefsIgnoresNonGitPackfileManifests(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	// A repository holding an ordinary container image: a manifest whose only
	// layer is a filesystem tarball, tagged the way images are.
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: ocispec.Manifest{}.Versioned,
		MediaType: ocispec.MediaTypeImageManifest,
		Layers: []ocispec.Descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 100},
		},
		Annotations: map[string]string{
			ocispec.AnnotationRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	reg.PutManifest("latest", manifest)

	client := registrytest.Client(t, ts)
	ctx := context.Background()
	refs, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("Expected 0 refs from non-git container image, got %d: %v", len(refs), refs)
	}
}
func TestOCIClientAuthEnv(t *testing.T) {
	t.Setenv("OCI_USERNAME", "testuser")
	t.Setenv("OCI_PASSWORD", "testpass")

	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	if client == nil || client.Repo == nil {
		t.Fatalf("Expected non-nil client and repository")
	}
}

func TestPushCommitStreamCompression(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	rawPayload := []byte("streaming-packfile-compression-test-payload-abcdef1234567890")

	modes := []string{"gzip", "zstd", "none"}
	for i, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OCI_COMPRESSION", mode)
			commitSHA := fmt.Sprintf("555555555555555555555555555555555555555%d", i)

			err := client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", WriteRefTag: true}, bytes.NewReader(rawPayload), int64(len(rawPayload)))
			if err != nil {
				t.Fatalf("PushCommitStream failed for mode %s: %v", mode, err)
			}

			manifest, err := client.FetchManifest(ctx, commitSHA)
			if err != nil {
				t.Fatalf("FetchManifest failed: %v", err)
			}

			packStream, err := client.FetchPackfileStream(ctx, manifest)
			if err != nil {
				t.Fatalf("FetchPackfileStream failed: %v", err)
			}
			defer func() { _ = packStream.Close() }()

			decompressed, err := io.ReadAll(packStream)
			if err != nil {
				t.Fatalf("io.ReadAll packStream failed: %v", err)
			}

			if string(decompressed) != string(rawPayload) {
				t.Errorf("Decompressed stream content mismatch: expected %q, got %q", rawPayload, decompressed)
			}
		})
	}
}

func TestRefIndex(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	refMap := map[string]string{
		"refs/heads/main": "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"refs/tags/v1.0":  "1a4893d2ba420eecc404dca472ad7403fabc73c3",
	}

	err = pushRefIndex(ctx, client, refMap)
	if err != nil {
		t.Fatalf("PushRichRefIndex failed: %v", err)
	}

	fetchedRefs, err := client.FetchRefIndex(ctx)
	if err != nil {
		t.Fatalf("FetchRefIndex failed: %v", err)
	}

	if len(fetchedRefs) != 2 || fetchedRefs["refs/heads/main"] != refMap["refs/heads/main"] {
		t.Errorf("RefIndex mismatch: expected %v, got %v", refMap, fetchedRefs)
	}

	// Verify ListRefs returns fast index result
	listRefs, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if listRefs["refs/tags/v1.0"] != "1a4893d2ba420eecc404dca472ad7403fabc73c3" {
		t.Errorf("ListRefs fast index lookup failed, got %v", listRefs)
	}
}

func TestDeleteRef(t *testing.T) {
	mock := registrytest.New()
	ts := mock.Serve(t)

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	refMap := map[string]string{
		"refs/heads/main":    "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"refs/heads/feature": "1a4893d2ba420eecc404dca472ad7403fabc73c3",
	}

	if err := pushRefIndex(ctx, client, refMap); err != nil {
		t.Fatalf("PushRichRefIndex failed: %v", err)
	}

	// Delete feature ref
	if err := client.DeleteRef(ctx, "refs/heads/feature"); err != nil {
		t.Fatalf("DeleteRef failed: %v", err)
	}

	refs, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if _, exists := refs["refs/heads/feature"]; exists {
		t.Errorf("refs/heads/feature still exists after DeleteRef")
	}
	if refs["refs/heads/main"] != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("refs/heads/main missing or modified: %v", refs)
	}

	// Delete main ref
	if err := client.DeleteRef(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("DeleteRef failed: %v", err)
	}

	refsAfter, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs failed: %v", err)
	}
	if len(refsAfter) != 0 {
		t.Errorf("Expected empty refs after deleting all refs, got: %v", refsAfter)
	}
}

// TestNewClientForURLPlainHTTPRule pins the one decision every entry point
// shares. The remote helper and the CLI subcommands used to each carry their
// own copy of it, so a change to one silently left the other reaching the same
// registry over a different scheme.
func TestNewClientForURLPlainHTTPRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		insecure string
		want     bool
	}{
		{"localhost is implicitly plain", "oci://localhost:5000/org/repo", "", true},
		{"loopback ip is implicitly plain", "127.0.0.1:5000/org/repo", "", true},
		{"a hosted registry is not", "oci://ghcr.io/org/repo", "", false},
		{"OCI_INSECURE=1 forces plain", "oci://ghcr.io/org/repo", "1", true},
		{"OCI_INSECURE=true forces plain", "oci://ghcr.io/org/repo", "true", true},
		{"any other value does not", "oci://ghcr.io/org/repo", "yes", false},
		{"a host merely starting with localhost is not looped back",
			"oci://localhost.example.com/org/repo", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := oci.NewClientForURL(tc.url, func(k string) string {
				if k == "OCI_INSECURE" {
					return tc.insecure
				}
				return ""
			})
			if err != nil {
				t.Fatalf("oci.NewClientForURL(%q): %v", tc.url, err)
			}
			if got := c.Repo.PlainHTTP; got != tc.want {
				t.Errorf("PlainHTTP = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewClientForURLStripsScheme: the oci:// prefix is ours, not the
// registry's, and must never reach the reference parser.
func TestNewClientForURLStripsScheme(t *testing.T) {
	withScheme, err := oci.NewClientForURL("oci://ghcr.io/org/repo", func(string) string { return "" })
	if err != nil {
		t.Fatalf("with scheme: %v", err)
	}
	without, err := oci.NewClientForURL("ghcr.io/org/repo", func(string) string { return "" })
	if err != nil {
		t.Fatalf("without scheme: %v", err)
	}
	if withScheme.Repo.Reference.String() != without.Repo.Reference.String() {
		t.Errorf("oci:// changed the reference: %q vs %q",
			withScheme.Repo.Reference.String(), without.Repo.Reference.String())
	}
}
