package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type mockRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte // reference/tag -> manifest JSON
	tags      []string
	// refuseDelete makes the registry answer every manifest DELETE with 405,
	// which is how several hosted registries behave.
	refuseDelete bool
	// failDeleteWith, when non-zero, answers every manifest DELETE with that
	// status, standing in for a transient failure rather than a policy.
	failDeleteWith int
	// intercept, when set, runs before any normal handling. Returning true
	// means it wrote the response itself and the default path is skipped.
	//
	// It is called without m.mu held, so it may lock and mutate the maps above.
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string][]byte),
		tags:      make([]string, 0),
	}
}

func (m *mockRegistry) Server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		m.mu.Lock()
		hook := m.intercept
		m.mu.Unlock()
		if hook != nil && hook(w, r) {
			return
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		if path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// List tags: /v2/<repo>/tags/list
		if strings.HasSuffix(path, "/tags/list") {
			resp := map[string]any{
				"name": "test-repo",
				"tags": m.tags,
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Blob upload initiation: POST /v2/<repo>/blobs/uploads/
		if r.Method == http.MethodPost && strings.Contains(path, "/blobs/uploads/") {
			w.Header().Set("Location", "/v2/test-repo/blobs/uploads/session-1")
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Blob upload PUT: PUT /v2/<repo>/blobs/uploads/session-1?digest=...
		if r.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/") {
			digest := r.URL.Query().Get("digest")
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// A conformant registry verifies that the uploaded bytes hash to
			// the digest the client claimed, and rejects the upload otherwise.
			// Modelling that matters: without it the mock happily stores a blob
			// under a digest that does not describe it, which is exactly the
			// bug this package has to avoid producing.
			if digest != "" && opencontainers.FromBytes(data).String() != digest {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":[{"code":"DIGEST_INVALID"}]}`))
				return
			}
			m.blobs[digest] = data
			w.WriteHeader(http.StatusCreated)
			return
		}

		// Fetch blob: GET /v2/<repo>/blobs/<digest>
		if r.Method == http.MethodGet && strings.Contains(path, "/blobs/") {
			parts := strings.Split(path, "/blobs/")
			digest := parts[len(parts)-1]
			if data, ok := m.blobs[digest]; ok {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(data)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Push manifest: PUT /v2/<repo>/manifests/<tag-or-digest>
		if r.Method == http.MethodPut && strings.Contains(path, "/manifests/") {
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			m.manifests[ref] = data

			// Keep track of tags
			found := false
			for _, t := range m.tags {
				if t == ref {
					found = true
					break
				}
			}
			if !found {
				m.tags = append(m.tags, ref)
			}

			digest := opencontainers.FromBytes(data).String()
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
			return
		}

		// Delete manifest: DELETE /v2/<repo>/manifests/<tag-or-digest>
		if r.Method == http.MethodDelete && strings.Contains(path, "/manifests/") {
			if m.failDeleteWith != 0 {
				w.WriteHeader(m.failDeleteWith)
				return
			}
			if m.refuseDelete {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"manifest delete is disabled"}]}`))
				return
			}
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]
			for tag, data := range m.manifests {
				if tag == ref || opencontainers.FromBytes(data).String() == ref {
					delete(m.manifests, tag)
					newTags := make([]string, 0, len(m.tags))
					for _, t := range m.tags {
						if t != tag {
							newTags = append(newTags, t)
						}
					}
					m.tags = newTags
				}
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Fetch manifest: GET or HEAD /v2/<repo>/manifests/<tag-or-digest>
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(path, "/manifests/") {
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]
			data, ok := m.manifests[ref]
			if !ok {
				// A digest reference resolves to whichever tag carries it.
				// Without this, anything that fetches by digest - including
				// the referrers indexing oras does before a manifest delete -
				// sees a 404.
				for _, candidate := range m.manifests {
					if opencontainers.FromBytes(candidate).String() == ref {
						data, ok = candidate, true
						break
					}
				}
			}
			if ok {
				digest := opencontainers.FromBytes(data).String()
				w.Header().Set("Docker-Content-Digest", digest)
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
				if r.Method == http.MethodGet {
					_, _ = w.Write(data)
				}
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	})

	return httptest.NewServer(mux)
}

func TestOCIClientPushAndFetch(t *testing.T) {
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	refName := "refs/heads/main"
	refTag := "main"
	packfileData := []byte("mock-git-packfile-data")

	err = pushCommitImage(ctx, client, commitSHA, refName, refTag, packfileData)
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
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "1111111111111111111111111111111111111111"

	// Push with empty refTag
	err = pushCommitImage(ctx, client, commitSHA, "refs/heads/custom", "", []byte("pack"))
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
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "2222222222222222222222222222222222222222"

	// 1. Short read (expected 10 bytes, provided 5)
	err = client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", RefTag: "main"}, bytes.NewReader([]byte("12345")), 10)
	if err == nil || !strings.Contains(err.Error(), "packfile size mismatch") {
		t.Errorf("Expected short read error, got: %v", err)
	}

	// 2. Excess bytes (expected 5 bytes, provided 10)
	err = client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", RefTag: "main"}, bytes.NewReader([]byte("1234567890")), 5)
	if err == nil || !strings.Contains(err.Error(), "exceeds expected size") {
		t.Errorf("Expected excess bytes error, got: %v", err)
	}

	// 3. Exact match (expected 5 bytes, provided 5)
	err = client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", RefTag: "main"}, bytes.NewReader([]byte("12345")), 5)
	if err != nil {
		t.Errorf("Expected success on exact size match, got: %v", err)
	}
}

func TestPushCommitImageInvalidSHA(t *testing.T) {
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()

	// Invalid SHA (too short)
	err = pushCommitImage(ctx, client, "invalid-sha", "refs/heads/main", "main", []byte("pack"))
	if err == nil || !strings.Contains(err.Error(), "invalid commit SHA") {
		t.Errorf("Expected invalid commit SHA error, got: %v", err)
	}
}

func TestPushCommitImageTagCollision(t *testing.T) {
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	commitSHA := "3333333333333333333333333333333333333333"

	// Push with refTag equal to commitSHA
	refName := "refs/heads/" + commitSHA
	err = pushCommitImage(ctx, client, commitSHA, refName, commitSHA, []byte("pack-data"))
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

	// Push with refTag equal to a DIFFERENT 40-hex SHA string
	differentSHA := "4444444444444444444444444444444444444444"
	diffRefName := "refs/heads/" + differentSHA
	err = pushCommitImage(ctx, client, commitSHA, diffRefName, differentSHA, []byte("pack-data-2"))
	if err != nil {
		t.Fatalf("PushCommitImage for different 40-hex refTag failed: %v", err)
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
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/v1.0.0") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json; charset=utf-8")
			manifest := ocispec.Manifest{
				Versioned: ocispec.Manifest{}.Versioned,
				Annotations: map[string]string{
					ocispec.AnnotationRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
				},
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()
	manifest, err := client.FetchManifest(ctx, "v1.0.0")
	if err != nil {
		t.Fatalf("FetchManifest failed for Content-Type with parameters: %v", err)
	}
	if manifest.Annotations[ocispec.AnnotationRevision] != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("Unexpected revision in manifest: %v", manifest.Annotations[ocispec.AnnotationRevision])
	}
}

func TestListRefsIgnoresNonGitPackfileManifests(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"latest"}})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/latest") {
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			// Non-git manifest (standard container image layer)
			manifest := ocispec.Manifest{
				Versioned: ocispec.Manifest{}.Versioned,
				Layers: []ocispec.Descriptor{
					{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 100},
				},
				Annotations: map[string]string{
					ocispec.AnnotationRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
				},
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	url := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	client, err := oci.NewClient(url, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

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

	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

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
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

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

			err := client.PushCommitStream(ctx, oci.CommitPush{CommitSHA: commitSHA, RefName: "refs/heads/main", RefTag: "main"}, bytes.NewReader(rawPayload), int64(len(rawPayload)))
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
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

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
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

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
