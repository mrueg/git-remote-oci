package helper_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/helper"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type mockRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	// byDigest retains every manifest ever pushed, keyed by digest. A real
	// registry keeps a manifest addressable by digest after its tag moves on;
	// without that, re-tagging a superseded manifest - which is how an atomic
	// push rolls back - cannot work.
	byDigest map[string][]byte
	tags     []string
	// intercept, when set, runs before any normal handling. Returning true
	// means it wrote the response itself and the default path is skipped.
	//
	// It is called without m.mu held, so it must not touch the maps above.
	intercept func(w http.ResponseWriter, r *http.Request) bool
	// requests records "METHOD path" for every request served, so tests can
	// assert that something did *not* happen.
	requests []string
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string][]byte),
		byDigest:  make(map[string][]byte),
	}
}

func (m *mockRegistry) Server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+path)
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

		if strings.HasSuffix(path, "/tags/list") {
			resp := map[string]any{
				"name": "test-repo",
				"tags": m.tags,
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if r.Method == http.MethodPost && strings.Contains(path, "/blobs/uploads/") {
			w.Header().Set("Location", "/v2/test-repo/blobs/uploads/session-1")
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if r.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/") {
			digest := r.URL.Query().Get("digest")
			data, _ := io.ReadAll(r.Body)
			m.blobs[digest] = data
			w.WriteHeader(http.StatusCreated)
			return
		}

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

		if r.Method == http.MethodPut && strings.Contains(path, "/manifests/") {
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]
			data, _ := io.ReadAll(r.Body)
			m.manifests[ref] = data
			m.byDigest[opencontainers.FromBytes(data).String()] = data

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

		// HEAD is how ORAS resolves a tag to a descriptor. Without it, every
		// Resolve fails as "not found" and deletions silently no-op.
		if r.Method == http.MethodHead && strings.Contains(path, "/manifests/") {
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]
			data, ok := m.manifests[ref]
			if !ok {
				data, ok = m.byDigest[ref]
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", opencontainers.FromBytes(data).String())
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method == http.MethodDelete && strings.Contains(path, "/manifests/") {
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]

			// The reference may be a tag or a digest; drop both the tag entry
			// and any tag whose manifest has the requested digest.
			deleted := false
			for tag, data := range m.manifests {
				if tag == ref || opencontainers.FromBytes(data).String() == ref {
					delete(m.manifests, tag)
					m.tags = slices.DeleteFunc(m.tags, func(t string) bool { return t == tag })
					deleted = true
				}
			}
			if !deleted {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(path, "/manifests/") {
			parts := strings.Split(path, "/manifests/")
			ref := parts[len(parts)-1]
			data, ok := m.manifests[ref]
			if !ok {
				data, ok = m.byDigest[ref]
			}
			if ok {
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
				w.Header().Set("Docker-Content-Digest", opencontainers.FromBytes(data).String())
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
				_, _ = w.Write(data)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	})

	return httptest.NewServer(mux)
}

func TestHelperEndToEnd(t *testing.T) {
	// 1. Create temporary directory for source repo
	srcDir := t.TempDir()

	// Initialise git repo using go-git
	repo, err := gogit.PlainInit(srcDir, false)
	if err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	testFilePath := filepath.Join(srcDir, "README.md")
	if err := os.WriteFile(testFilePath, []byte("# Test Repository\n"), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	_, err = w.Add("README.md")
	if err != nil {
		t.Fatalf("Failed to add file: %v", err)
	}

	commitHash, err := w.Commit("Initial commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	// 2. Start mock registry
	mock := newMockRegistry()
	ts := mock.Server()
	defer ts.Close()

	ociURL := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"

	// Set GIT_DIR to srcDir/.git
	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))

	headRef, err := repo.Head()
	if err != nil {
		t.Fatalf("Failed to get repo HEAD: %v", err)
	}
	headRefName := headRef.Name().String()

	// 3. Test push via helper
	inputCmds := fmt.Sprintf("capabilities\nlist\npush %s:refs/heads/main\n\nquit\n", headRefName)
	inBuf := bytes.NewBufferString(inputCmds)
	outBuf := new(bytes.Buffer)

	h, err := helper.NewHelper("origin", "oci://"+ociURL, inBuf, outBuf)
	if err != nil {
		t.Fatalf("Failed to create helper: %v", err)
	}

	ctx := context.Background()
	if err := h.Run(ctx); err != nil {
		t.Fatalf("Helper.Run failed: %v", err)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, "fetch") || !strings.Contains(outStr, "push") {
		t.Errorf("Capabilities missing, got output:\n%s", outStr)
	}
	if !strings.Contains(outStr, "ok refs/heads/main") {
		t.Errorf("Push response missing 'ok refs/heads/main', got output:\n%s", outStr)
	}

	// 4. Test list via helper
	inBufList := bytes.NewBufferString("list\n\nquit\n")
	outBufList := new(bytes.Buffer)
	hList, err := helper.NewHelper("origin", "oci://"+ociURL, inBufList, outBufList)
	if err != nil {
		t.Fatalf("Failed to create helper for list: %v", err)
	}
	if err := hList.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for list failed: %v", err)
	}

	listOut := outBufList.String()
	expectedRefLine := commitHash.String() + " refs/heads/main"
	if !strings.Contains(listOut, expectedRefLine) {
		t.Errorf("List output missing %q, got:\n%s", expectedRefLine, listOut)
	}

	// 5. Test fetch into a new clean repo
	dstDir := t.TempDir()

	dstRepo, err := gogit.PlainInit(dstDir, false)
	if err != nil {
		t.Fatalf("Failed to init dst repo: %v", err)
	}

	t.Setenv("GIT_DIR", filepath.Join(dstDir, ".git"))

	fetchCmds := "fetch " + commitHash.String() + " refs/heads/main\n\nquit\n"
	inBufFetch := bytes.NewBufferString(fetchCmds)
	outBufFetch := new(bytes.Buffer)
	hFetch, err := helper.NewHelper("origin", "oci://"+ociURL, inBufFetch, outBufFetch)
	if err != nil {
		t.Fatalf("Failed to create helper for fetch: %v", err)
	}

	if err := hFetch.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for fetch failed: %v", err)
	}

	// Verify commit exists in dstRepo
	commitObj, err := dstRepo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("Failed to get fetched commit from dst repo: %v", err)
	}
	if commitObj.Message != "Initial commit\n" && commitObj.Message != "Initial commit" {
		t.Errorf("Unexpected commit message: %q", commitObj.Message)
	}
	// 6. Test pushing a Git tag (refs/tags/v1.0.0)
	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))
	tagCmds := fmt.Sprintf("push %s:refs/tags/v1.0.0\n\nquit\n", headRefName)
	inBufTag := bytes.NewBufferString(tagCmds)
	outBufTag := new(bytes.Buffer)
	hTag, err := helper.NewHelper("origin", "oci://"+ociURL, inBufTag, outBufTag)
	if err != nil {
		t.Fatalf("Failed to create helper for tag push: %v", err)
	}
	if err := hTag.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for tag push failed: %v", err)
	}
	if !strings.Contains(outBufTag.String(), "ok refs/tags/v1.0.0") {
		t.Errorf("Tag push missing 'ok refs/tags/v1.0.0', got:\n%s", outBufTag.String())
	}

	// Verify listing includes the tag ref
	inBufListTag := bytes.NewBufferString("list\n\nquit\n")
	outBufListTag := new(bytes.Buffer)
	hListTag, err := helper.NewHelper("origin", "oci://"+ociURL, inBufListTag, outBufListTag)
	if err != nil {
		t.Fatalf("Failed to create helper for tag list: %v", err)
	}
	if err := hListTag.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for tag list failed: %v", err)
	}
	expectedTagLine := commitHash.String() + " refs/tags/v1.0.0"
	if !strings.Contains(outBufListTag.String(), expectedTagLine) {
		t.Errorf("List output missing tag ref %q, got:\n%s", expectedTagLine, outBufListTag.String())
	}

	// 7. A ref name made entirely of characters that are illegal at the start of
	// an OCI tag is now representable: the encoding escapes them rather than
	// mangling them, so this push succeeds where it used to be rejected.
	awkwardCmds := fmt.Sprintf("push %s:refs/heads/---\n\nquit\n", headRefName)
	inBufAwkward := bytes.NewBufferString(awkwardCmds)
	outBufAwkward := new(bytes.Buffer)
	hAwkward, err := helper.NewHelper("origin", "oci://"+ociURL, inBufAwkward, outBufAwkward)
	if err != nil {
		t.Fatalf("Failed to create helper for awkward push: %v", err)
	}
	if err := hAwkward.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for awkward push failed: %v", err)
	}
	if !strings.Contains(outBufAwkward.String(), "ok refs/heads/---") {
		t.Errorf("Awkward push output missing 'ok refs/heads/---', got:\n%s", outBufAwkward.String())
	}

	// 8. Test deletion push (e.g. push :refs/heads/feature)
	// First push refs/heads/feature so there is a ref to delete
	pushFeatureCmds := fmt.Sprintf("push %s:refs/heads/feature\n\nquit\n", headRefName)
	inBufFeature := bytes.NewBufferString(pushFeatureCmds)
	outBufFeature := new(bytes.Buffer)
	hFeature, err := helper.NewHelper("origin", "oci://"+ociURL, inBufFeature, outBufFeature)
	if err != nil {
		t.Fatalf("Failed to create helper for feature push: %v", err)
	}
	if err := hFeature.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for feature push failed: %v", err)
	}
	if !strings.Contains(outBufFeature.String(), "ok refs/heads/feature") {
		t.Fatalf("Feature push missing 'ok refs/heads/feature', got:\n%s", outBufFeature.String())
	}

	// Now delete refs/heads/feature
	deleteCmds := "push :refs/heads/feature\n\nquit\n"
	inBufDelete := bytes.NewBufferString(deleteCmds)
	outBufDelete := new(bytes.Buffer)
	hDelete, err := helper.NewHelper("origin", "oci://"+ociURL, inBufDelete, outBufDelete)
	if err != nil {
		t.Fatalf("Failed to create helper for delete push: %v", err)
	}
	if err := hDelete.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for delete push failed: %v", err)
	}
	if !strings.Contains(outBufDelete.String(), "ok refs/heads/feature") {
		t.Errorf("Delete push output missing 'ok refs/heads/feature', got:\n%s", outBufDelete.String())
	}

	// Verify listing no longer includes refs/heads/feature
	inBufListDel := bytes.NewBufferString("list\n\nquit\n")
	outBufListDel := new(bytes.Buffer)
	hListDel, err := helper.NewHelper("origin", "oci://"+ociURL, inBufListDel, outBufListDel)
	if err != nil {
		t.Fatalf("Failed to create helper for list after delete: %v", err)
	}
	if err := hListDel.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for list after delete failed: %v", err)
	}
	if strings.Contains(outBufListDel.String(), "refs/heads/feature") {
		t.Errorf("List output still contains deleted ref refs/heads/feature, got:\n%s", outBufListDel.String())
	}

	// 9. Test non-fast-forward push rejection and force override
	// Create a second commit so we have two distinct commits
	testFile2 := filepath.Join(srcDir, "CHANGELOG.md")
	if err := os.WriteFile(testFile2, []byte("# Changelog\n"), 0644); err != nil {
		t.Fatalf("Failed to write second test file: %v", err)
	}
	_, err = w.Add("CHANGELOG.md")
	if err != nil {
		t.Fatalf("Failed to add second file: %v", err)
	}
	_, err = w.Commit("Second commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create second commit: %v", err)
	}

	// Push initial commit (commit1) to refs/heads/develop using the initial HEAD ref
	// First we need to create a local branch 'develop' pointing at commit1
	developRef := plumbing.NewHashReference("refs/heads/develop", commitHash)
	if err := repo.Storer.SetReference(developRef); err != nil {
		t.Fatalf("Failed to create develop branch: %v", err)
	}

	pushCmds2 := "list\npush refs/heads/develop:refs/heads/develop\n\nquit\n"
	inBuf2 := bytes.NewBufferString(pushCmds2)
	outBuf2 := new(bytes.Buffer)
	h2, err := helper.NewHelper("origin", "oci://"+ociURL, inBuf2, outBuf2)
	if err != nil {
		t.Fatalf("Failed to create helper for develop push: %v", err)
	}
	if err := h2.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for develop push failed: %v", err)
	}
	if !strings.Contains(outBuf2.String(), "ok refs/heads/develop") {
		t.Errorf("Develop push missing 'ok refs/heads/develop', got:\n%s", outBuf2.String())
	}

	// Now advance develop branch to HEAD (commit2) and push (fast-forward, should succeed)
	headHash, err := repo.ResolveRevision(plumbing.Revision("HEAD"))
	if err != nil {
		t.Fatalf("Failed to resolve HEAD: %v", err)
	}
	developRef2 := plumbing.NewHashReference("refs/heads/develop", *headHash)
	if err := repo.Storer.SetReference(developRef2); err != nil {
		t.Fatalf("Failed to update develop branch: %v", err)
	}

	pushCmds3 := "list\npush refs/heads/develop:refs/heads/develop\n\nquit\n"
	inBuf3 := bytes.NewBufferString(pushCmds3)
	outBuf3 := new(bytes.Buffer)
	h3, err := helper.NewHelper("origin", "oci://"+ociURL, inBuf3, outBuf3)
	if err != nil {
		t.Fatalf("Failed to create helper for fast-forward push: %v", err)
	}
	if err := h3.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for fast-forward push failed: %v", err)
	}
	if !strings.Contains(outBuf3.String(), "ok refs/heads/develop") {
		t.Errorf("Fast-forward push missing 'ok refs/heads/develop', got:\n%s", outBuf3.String())
	}

	// Reset develop branch back to commit1 and try non-force push (non-fast-forward, should be rejected)
	developRef3 := plumbing.NewHashReference("refs/heads/develop", commitHash)
	if err := repo.Storer.SetReference(developRef3); err != nil {
		t.Fatalf("Failed to reset develop branch: %v", err)
	}

	pushCmdsNff := "list\npush refs/heads/develop:refs/heads/develop\n\nquit\n"
	inBufNff := bytes.NewBufferString(pushCmdsNff)
	outBufNff := new(bytes.Buffer)
	hNff, err := helper.NewHelper("origin", "oci://"+ociURL, inBufNff, outBufNff)
	if err != nil {
		t.Fatalf("Failed to create helper for non-fast-forward push: %v", err)
	}
	if err := hNff.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for non-fast-forward push failed: %v", err)
	}
	if !strings.Contains(outBufNff.String(), "error refs/heads/develop non-fast-forward") {
		t.Errorf("Non-fast-forward push should be rejected, got:\n%s", outBufNff.String())
	}

	// Force push commit1 back to refs/heads/develop (should succeed with '+')
	pushCmdsForce := "list\npush +refs/heads/develop:refs/heads/develop\n\nquit\n"
	inBufForce := bytes.NewBufferString(pushCmdsForce)
	outBufForce := new(bytes.Buffer)
	hForce, err := helper.NewHelper("origin", "oci://"+ociURL, inBufForce, outBufForce)
	if err != nil {
		t.Fatalf("Failed to create helper for force push: %v", err)
	}
	if err := hForce.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for force push failed: %v", err)
	}
	if !strings.Contains(outBufForce.String(), "ok refs/heads/develop") {
		t.Errorf("Force push missing 'ok refs/heads/develop', got:\n%s", outBufForce.String())
	}

	// 10. Test push without preceding list call (remoteRefs should be auto-populated)
	// Reset develop back to commit1 again locally
	developRef4 := plumbing.NewHashReference("refs/heads/develop", commitHash)
	if err := repo.Storer.SetReference(developRef4); err != nil {
		t.Fatalf("Failed to reset develop branch: %v", err)
	}

	// First advance remote develop to commit2 using force push so remote has commit2
	developRefAdvance := plumbing.NewHashReference("refs/heads/develop", *headHash)
	if err := repo.Storer.SetReference(developRefAdvance); err != nil {
		t.Fatalf("Failed to set develop to headHash: %v", err)
	}
	pushCmdsAdvance := "list\npush +refs/heads/develop:refs/heads/develop\n\nquit\n"
	inBufAdvance := bytes.NewBufferString(pushCmdsAdvance)
	outBufAdvance := new(bytes.Buffer)
	hAdvance, err := helper.NewHelper("origin", "oci://"+ociURL, inBufAdvance, outBufAdvance)
	if err != nil {
		t.Fatalf("Failed to create helper for advance push: %v", err)
	}
	if err := hAdvance.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for advance push failed: %v", err)
	}

	// Reset local develop back to commitHash
	if err := repo.Storer.SetReference(developRef4); err != nil {
		t.Fatalf("Failed to reset develop branch: %v", err)
	}

	// Issue push WITHOUT preceding list command (push directly without 'list')
	pushNoListCmds := "push refs/heads/develop:refs/heads/develop\n\nquit\n"
	inBufNoList := bytes.NewBufferString(pushNoListCmds)
	outBufNoList := new(bytes.Buffer)
	hNoList, err := helper.NewHelper("origin", "oci://"+ociURL, inBufNoList, outBufNoList)
	if err != nil {
		t.Fatalf("Failed to create helper for no-list push: %v", err)
	}
	if err := hNoList.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for no-list push failed: %v", err)
	}
	if !strings.Contains(outBufNoList.String(), "error refs/heads/develop non-fast-forward") {
		t.Errorf("Push without list command should still enforce non-fast-forward protection, got:\n%s", outBufNoList.String())
	}
	// 11. Test option atomic true and atomic push batch handling
	atomicCmds := fmt.Sprintf("capabilities\noption atomic true\nlist\npush %s:refs/heads/atomic1\npush refs/heads/nonexistent:refs/heads/atomic2\n\nquit\n", headRefName)
	inBufAtomic := bytes.NewBufferString(atomicCmds)
	outBufAtomic := new(bytes.Buffer)
	hAtomic, err := helper.NewHelper("origin", "oci://"+ociURL, inBufAtomic, outBufAtomic)
	if err != nil {
		t.Fatalf("Failed to create helper for atomic push: %v", err)
	}
	if err := hAtomic.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for atomic push failed: %v", err)
	}
	outAtomic := outBufAtomic.String()
	if !strings.Contains(outAtomic, "option") {
		t.Errorf("Capabilities output missing 'option', got:\n%s", outAtomic)
	}
	if !strings.Contains(outAtomic, "ok\n") {
		t.Errorf("Option atomic true response missing 'ok', got:\n%s", outAtomic)
	}
	if !strings.Contains(outAtomic, "error refs/heads/atomic2") || !strings.Contains(outAtomic, "error refs/heads/atomic1") {
		t.Errorf("Atomic push batch with one failing ref should fail all refs in batch, got:\n%s", outAtomic)
	}

	// Successful atomic batch
	atomicValidCmds := fmt.Sprintf("option atomic true\nlist\npush %s:refs/heads/atomic1\npush %s:refs/heads/atomic2\n\nquit\n", headRefName, headRefName)
	inBufAtomicValid := bytes.NewBufferString(atomicValidCmds)
	outBufAtomicValid := new(bytes.Buffer)
	hAtomicValid, err := helper.NewHelper("origin", "oci://"+ociURL, inBufAtomicValid, outBufAtomicValid)
	if err != nil {
		t.Fatalf("Failed to create helper for valid atomic push: %v", err)
	}
	if err := hAtomicValid.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for valid atomic push failed: %v", err)
	}
	outAtomicValid := outBufAtomicValid.String()
	if !strings.Contains(outAtomicValid, "ok refs/heads/atomic1") || !strings.Contains(outAtomicValid, "ok refs/heads/atomic2") {
		t.Errorf("Valid atomic push batch missing expected ok responses, got:\n%s", outAtomicValid)
	}
	// 12. Test option dry-run true (push validation without remote mutation)
	dryRunCmds := fmt.Sprintf("capabilities\noption dry-run true\nlist\npush %s:refs/heads/dryrun-branch\n\nquit\n", headRefName)
	inBufDry := bytes.NewBufferString(dryRunCmds)
	outBufDry := new(bytes.Buffer)
	hDry, err := helper.NewHelper("origin", "oci://"+ociURL, inBufDry, outBufDry)
	if err != nil {
		t.Fatalf("Failed to create helper for dry-run push: %v", err)
	}
	if err := hDry.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for dry-run push failed: %v", err)
	}
	outDry := outBufDry.String()
	if !strings.Contains(outDry, "option") {
		t.Errorf("Capabilities missing 'option', got:\n%s", outDry)
	}
	if !strings.Contains(outDry, "ok refs/heads/dryrun-branch") {
		t.Errorf("Dry-run push missing 'ok refs/heads/dryrun-branch', got:\n%s", outDry)
	}

	// Verify that dry-run push did NOT update remote refs
	listCheckCmds := "list\n\nquit\n"
	inBufListCheck := bytes.NewBufferString(listCheckCmds)
	outBufListCheck := new(bytes.Buffer)
	hListCheck, err := helper.NewHelper("origin", "oci://"+ociURL, inBufListCheck, outBufListCheck)
	if err != nil {
		t.Fatalf("Failed to create helper for post-dryrun list check: %v", err)
	}
	if err := hListCheck.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for post-dryrun list check failed: %v", err)
	}
	if strings.Contains(outBufListCheck.String(), "refs/heads/dryrun-branch") {
		t.Errorf("Dry-run push should NOT create remote ref refs/heads/dryrun-branch, but found it in list:\n%s", outBufListCheck.String())
	}
	// 13. Test option depth <n> for shallow fetch (depth 1)
	shallowDir := t.TempDir()

	_, err = gogit.PlainInit(shallowDir, false)
	if err != nil {
		t.Fatalf("Failed to init shallow repo: %v", err)
	}

	t.Setenv("GIT_DIR", filepath.Join(shallowDir, ".git"))
	depthCmds := fmt.Sprintf("option depth 1\nfetch %s refs/heads/develop\n\nquit\n", *headHash)
	inBufDepth := bytes.NewBufferString(depthCmds)
	outBufDepth := new(bytes.Buffer)
	hDepth, err := helper.NewHelper("origin", "oci://"+ociURL, inBufDepth, outBufDepth)
	if err != nil {
		t.Fatalf("Failed to create helper for shallow fetch: %v", err)
	}
	if err := hDepth.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for shallow fetch failed: %v", err)
	}
	if !strings.Contains(outBufDepth.String(), "ok\n") {
		t.Errorf("Option depth response missing 'ok', got:\n%s", outBufDepth.String())
	}

	// Verify shallow repository only imported 1 commit
	revCountCmd := exec.CommandContext(t.Context(), "git", "--git-dir="+filepath.Join(shallowDir, ".git"), "rev-list", "--count", headHash.String())
	revCountOut, err := revCountCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to count commits in shallow repo: %v\nOutput: %s", err, string(revCountOut))
	}
	if countStr := strings.TrimSpace(string(revCountOut)); countStr != "1" {
		t.Errorf("Expected shallow fetch with depth 1 to import 1 commit, got count %s", countStr)
	}
	// 14. Test option cas <ref>:<expected-sha> (force-with-lease protection)
	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))
	// 14a. CAS mismatch: wrong expected SHA should be rejected with stale info error
	casFailCmds := fmt.Sprintf("capabilities\noption cas refs/heads/main:0000000000000000000000000000000000000000\nlist\npush %s:refs/heads/main\n\nquit\n", headRefName)
	inBufCasFail := bytes.NewBufferString(casFailCmds)
	outBufCasFail := new(bytes.Buffer)
	hCasFail, err := helper.NewHelper("origin", "oci://"+ociURL, inBufCasFail, outBufCasFail)
	if err != nil {
		t.Fatalf("Failed to create helper for failing CAS push: %v", err)
	}
	if err := hCasFail.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for failing CAS push failed: %v", err)
	}
	outCasFail := outBufCasFail.String()
	if !strings.Contains(outCasFail, "error refs/heads/main stale info") {
		t.Errorf("CAS push with wrong expected SHA should return stale info error, got:\n%s", outCasFail)
	}

	// 14b. CAS match: correct expected SHA should succeed
	casPassCmds := fmt.Sprintf("option cas refs/heads/main:%s\nlist\npush %s:refs/heads/main\n\nquit\n", commitHash, headRefName)
	inBufCasPass := bytes.NewBufferString(casPassCmds)
	outBufCasPass := new(bytes.Buffer)
	hCasPass, err := helper.NewHelper("origin", "oci://"+ociURL, inBufCasPass, outBufCasPass)
	if err != nil {
		t.Fatalf("Failed to create helper for passing CAS push: %v", err)
	}
	if err := hCasPass.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for passing CAS push failed: %v", err)
	}
	outCasPass := outBufCasPass.String()
	if !strings.Contains(outCasPass, "ok refs/heads/main") {
		t.Errorf("CAS push with correct expected SHA should succeed, got:\n%s", outCasPass)
	}

	// 15. Test option verbosity <n>
	// 15a. Verbosity 0 (quiet): stderr progress output should be suppressed
	quietCmds := fmt.Sprintf("option verbosity 0\nfetch %s refs/heads/develop\n\nquit\n", *headHash)
	inBufQuiet := bytes.NewBufferString(quietCmds)
	outBufQuiet := new(bytes.Buffer)
	hQuiet, err := helper.NewHelper("origin", "oci://"+ociURL, inBufQuiet, outBufQuiet)
	if err != nil {
		t.Fatalf("Failed to create helper for quiet fetch: %v", err)
	}
	if err := hQuiet.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for quiet fetch failed: %v", err)
	}
	if !strings.Contains(outBufQuiet.String(), "ok\n") {
		t.Errorf("Option verbosity 0 response missing 'ok', got:\n%s", outBufQuiet.String())
	}

	// 15b. Verbosity 2 (verbose): stderr output should contain extra [verbose] details
	verboseCmds := fmt.Sprintf("option verbosity 2\nfetch %s refs/heads/develop\n\nquit\n", *headHash)
	inBufVerbose := bytes.NewBufferString(verboseCmds)
	outBufVerbose := new(bytes.Buffer)
	hVerbose, err := helper.NewHelper("origin", "oci://"+ociURL, inBufVerbose, outBufVerbose)
	if err != nil {
		t.Fatalf("Failed to create helper for verbose fetch: %v", err)
	}
	if err := hVerbose.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for verbose fetch failed: %v", err)
	}
	if !strings.Contains(outBufVerbose.String(), "ok\n") {
		t.Errorf("Option verbosity 2 response missing 'ok', got:\n%s", outBufVerbose.String())
	}
	// 16. Test option progress true|false
	// 16a. option progress true
	progressTrueCmds := fmt.Sprintf("option progress true\nfetch %s refs/heads/develop\n\nquit\n", *headHash)
	inBufProgTrue := bytes.NewBufferString(progressTrueCmds)
	outBufProgTrue := new(bytes.Buffer)
	hProgTrue, err := helper.NewHelper("origin", "oci://"+ociURL, inBufProgTrue, outBufProgTrue)
	if err != nil {
		t.Fatalf("Failed to create helper for progress true: %v", err)
	}
	if err := hProgTrue.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for progress true failed: %v", err)
	}
	if !strings.Contains(outBufProgTrue.String(), "ok\n") {
		t.Errorf("Option progress true response missing 'ok', got:\n%s", outBufProgTrue.String())
	}

	// 16b. option progress false
	progressFalseCmds := fmt.Sprintf("option progress false\nfetch %s refs/heads/develop\n\nquit\n", *headHash)
	inBufProgFalse := bytes.NewBufferString(progressFalseCmds)
	outBufProgFalse := new(bytes.Buffer)
	hProgFalse, err := helper.NewHelper("origin", "oci://"+ociURL, inBufProgFalse, outBufProgFalse)
	if err != nil {
		t.Fatalf("Failed to create helper for progress false: %v", err)
	}
	if err := hProgFalse.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for progress false failed: %v", err)
	}
	if !strings.Contains(outBufProgFalse.String(), "ok\n") {
		t.Errorf("Option progress false response missing 'ok', got:\n%s", outBufProgFalse.String())
	}

	// 17. Test Git LFS pointer scanning, upload, and object retrieval
	lfsContent := []byte("Large binary LFS payload data for testing")
	// The OID must be the real SHA-256 of the payload: the fetch path verifies
	// downloaded content against it before storing.
	lfsSum := sha256.Sum256(lfsContent)
	lfsOID := hex.EncodeToString(lfsSum[:])
	lfsPtrText := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", lfsOID, len(lfsContent))

	// Write LFS pointer to repo file
	lfsFilePath := filepath.Join(srcDir, "large_file.bin")
	if err := os.WriteFile(lfsFilePath, []byte(lfsPtrText), 0644); err != nil {
		t.Fatalf("Failed to write LFS pointer file: %v", err)
	}

	_, err = w.Add("large_file.bin")
	if err != nil {
		t.Fatalf("Failed to add LFS pointer file: %v", err)
	}

	lfsCommitHash, err := w.Commit("Add LFS pointer file", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit LFS pointer: %v", err)
	}

	// Store raw LFS payload in srcDir/.git/lfs/objects/...
	lfsObjDir := filepath.Join(srcDir, ".git", "lfs", "objects", lfsOID[0:2], lfsOID[2:4])
	if err := os.MkdirAll(lfsObjDir, 0755); err != nil {
		t.Fatalf("Failed to create src LFS object dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lfsObjDir, lfsOID), lfsContent, 0644); err != nil {
		t.Fatalf("Failed to write src LFS payload: %v", err)
	}

	// Create local branch 'lfs-branch' pointing at lfsCommitHash
	lfsBranchRef := plumbing.NewHashReference("refs/heads/lfs-branch", lfsCommitHash)
	if err := repo.Storer.SetReference(lfsBranchRef); err != nil {
		t.Fatalf("Failed to set lfs-branch ref: %v", err)
	}

	// Push lfs-branch to OCI
	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))
	lfsPushCmds := "push refs/heads/lfs-branch:refs/heads/lfs-branch\n\nquit\n"
	inBufLFSPush := bytes.NewBufferString(lfsPushCmds)
	outBufLFSPush := new(bytes.Buffer)
	hLFSPush, err := helper.NewHelper("origin", "oci://"+ociURL, inBufLFSPush, outBufLFSPush)
	if err != nil {
		t.Fatalf("Failed to create helper for LFS push: %v", err)
	}
	if err := hLFSPush.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for LFS push failed: %v", err)
	}
	if !strings.Contains(outBufLFSPush.String(), "ok refs/heads/lfs-branch") {
		t.Errorf("LFS push missing 'ok refs/heads/lfs-branch', got:\n%s", outBufLFSPush.String())
	}

	// Fetch lfs-branch into dstDir and verify LFS object is stored in dstDir/.git/lfs/objects/...
	t.Setenv("GIT_DIR", filepath.Join(dstDir, ".git"))
	lfsFetchCmds := fmt.Sprintf("fetch %s refs/heads/lfs-branch\n\nquit\n", lfsCommitHash)
	inBufLFSFetch := bytes.NewBufferString(lfsFetchCmds)
	outBufLFSFetch := new(bytes.Buffer)
	hLFSFetch, err := helper.NewHelper("origin", "oci://"+ociURL, inBufLFSFetch, outBufLFSFetch)
	if err != nil {
		t.Fatalf("Failed to create helper for LFS fetch: %v", err)
	}
	if err := hLFSFetch.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for LFS fetch failed: %v", err)
	}

	dstLFSPath := filepath.Join(dstDir, ".git", "lfs", "objects", lfsOID[0:2], lfsOID[2:4], lfsOID)
	dstLFSContent, err := os.ReadFile(dstLFSPath)
	if err != nil {
		t.Fatalf("Failed to read fetched LFS payload at %s: %v", dstLFSPath, err)
	}
	if !bytes.Equal(dstLFSContent, lfsContent) {
		t.Errorf("Fetched LFS content mismatch, got %q, expected %q", string(dstLFSContent), string(lfsContent))
	}

	// 18. Test option followtags true: automatically discovers reachable local tags and pushes them
	followTagRef := plumbing.NewHashReference("refs/tags/v2.0.0", lfsCommitHash)
	if err := repo.Storer.SetReference(followTagRef); err != nil {
		t.Fatalf("Failed to create local tag refs/tags/v2.0.0: %v", err)
	}

	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))
	followTagCmds := "option followtags true\npush refs/heads/lfs-branch:refs/heads/lfs-branch\n\nquit\n"
	inBufFollowTag := bytes.NewBufferString(followTagCmds)
	outBufFollowTag := new(bytes.Buffer)
	hFollowTag, err := helper.NewHelper("origin", "oci://"+ociURL, inBufFollowTag, outBufFollowTag)
	if err != nil {
		t.Fatalf("Failed to create helper for followtags push: %v", err)
	}
	if err := hFollowTag.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for followtags push failed: %v", err)
	}

	if !strings.Contains(outBufFollowTag.String(), "ok refs/tags/v2.0.0") {
		t.Errorf("Followtags push output missing 'ok refs/tags/v2.0.0', got:\n%s", outBufFollowTag.String())
	}

	// 19. Test parallel concurrent multi-LFS blob layer pushing
	for i := range 3 {
		payload := []byte(fmt.Sprintf("Payload %d", i))
		sum := sha256.Sum256(payload)
		oid := hex.EncodeToString(sum[:])

		lfsText := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(payload))
		_ = os.WriteFile(filepath.Join(srcDir, fmt.Sprintf("multi_%d.bin", i)), []byte(lfsText), 0644)
		_, _ = w.Add(fmt.Sprintf("multi_%d.bin", i))

		objDir := filepath.Join(srcDir, ".git", "lfs", "objects", oid[0:2], oid[2:4])
		_ = os.MkdirAll(objDir, 0755)
		_ = os.WriteFile(filepath.Join(objDir, oid), payload, 0644)
	}

	multiCommitHash, err := w.Commit("Add multiple LFS files for parallel push", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit multi LFS files: %v", err)
	}

	multiBranchRef := plumbing.NewHashReference("refs/heads/multi-lfs-branch", multiCommitHash)
	_ = repo.Storer.SetReference(multiBranchRef)

	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))
	multiPushCmds := "push refs/heads/multi-lfs-branch:refs/heads/multi-lfs-branch\n\nquit\n"
	inBufMultiPush := bytes.NewBufferString(multiPushCmds)
	outBufMultiPush := new(bytes.Buffer)
	hMultiPush, err := helper.NewHelper("origin", "oci://"+ociURL, inBufMultiPush, outBufMultiPush)
	if err != nil {
		t.Fatalf("Failed to create helper for multi-LFS push: %v", err)
	}
	if err := hMultiPush.Run(ctx); err != nil {
		t.Fatalf("Helper.Run for multi-LFS push failed: %v", err)
	}
	if !strings.Contains(outBufMultiPush.String(), "ok refs/heads/multi-lfs-branch") {
		t.Errorf("Multi-LFS parallel push output missing 'ok', got:\n%s", outBufMultiPush.String())
	}
}

func TestPushCertOption(t *testing.T) {
	cmdInput := "capabilities\noption pushcert true\noption pushcert if-asked\noption pushcert false\noption pushcert invalid\nquit\n"
	inBuf := bytes.NewBufferString(cmdInput)
	outBuf := new(bytes.Buffer)

	h, err := helper.NewHelper("origin", "oci://localhost:5000/repo", inBuf, outBuf)
	if err != nil {
		t.Fatalf("Failed to create helper: %v", err)
	}

	ctx := context.Background()
	if err := h.Run(ctx); err != nil {
		t.Fatalf("Helper.Run failed: %v", err)
	}

	// "pushcert" is not a gitremote-helpers(7) capability, so it is no longer
	// advertised. The option itself is still parsed and accepted.
	out := outBuf.String()
	if !strings.Contains(out, "ok\nok\nok\nunsupported") {
		t.Errorf("Unexpected pushcert option response, got:\n%s", out)
	}
}

// TestCapabilitiesAdvertisesOnlyRealNames pins the capability list to names
// that gitremote-helpers(7) actually defines. Advertising option names, or
// prefixing them with "+" (the mandatory marker is "*"), told git nothing and
// implied support that does not exist.
func TestCapabilitiesAdvertisesOnlyRealNames(t *testing.T) {
	inBuf := bytes.NewBufferString("capabilities\nquit\n")
	outBuf := new(bytes.Buffer)

	h, err := helper.NewHelper("origin", "oci://localhost:5000/repo", inBuf, outBuf)
	if err != nil {
		t.Fatalf("Failed to create helper: %v", err)
	}
	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("Helper.Run failed: %v", err)
	}

	var got []string
	for _, line := range strings.Split(outBuf.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got = append(got, line)
		}
	}

	// Every name here must be one gitremote-helpers(7) defines. "object-format"
	// is: it lets git ask for the remote's hash algorithm, without which it will
	// not talk to a SHA-256 repository. "stateless-connect" is too, and it is
	// advertised even when protocol v2 is switched off: the command answers
	// "fallback" in that case, which is the documented way to decline, and the
	// alternative — advertising it conditionally — would mean the capability
	// list depended on configuration git has no way to have supplied yet.
	want := []string{"fetch", "push", "option", "object-format", "stateless-connect"}
	if len(got) != len(want) {
		t.Fatalf("capabilities = %q, want exactly %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("capability %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOptionFilterAndDepth(t *testing.T) {
	// "deepen" is not an option gitremote-helpers(7) defines -- the shallow
	// options are `depth` and the `deepen-*` family -- so accepting it
	// claimed support for something git never sends.
	cmdInput := "capabilities\noption filter blob:none\noption filter blob:limit=100\noption depth 2\noption deepen 2\nquit\n"
	inBuf := bytes.NewBufferString(cmdInput)
	outBuf := new(bytes.Buffer)

	h, err := helper.NewHelper("origin", "oci://localhost:5000/repo", inBuf, outBuf)
	if err != nil {
		t.Fatalf("Failed to create helper: %v", err)
	}

	ctx := context.Background()
	if err := h.Run(ctx); err != nil {
		t.Fatalf("Helper.Run failed: %v", err)
	}

	// "filter" and "depth" are option names, not capabilities, so they are not
	// advertised. Both options are accepted; the made-up one is not.
	out := outBuf.String()
	if !strings.Contains(out, "ok\nok\nok\nunsupported\n") {
		t.Errorf("Unexpected filter/depth option response, got:\n%s", out)
	}
}

// TestRefPrefixFiltersListOutput exercises the filtering itself rather than
// merely grepping the capabilities banner, which is what the previous version
// of this test did - it never invoked the filter at all.
func TestRefPrefixFiltersListOutput(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	branch := currentBranch(t, srcDir)
	seed := "push " + branch + ":refs/heads/keep\npush " + branch + ":refs/tags/drop\n\n"
	if out, err := runHelper(t, registry, seed); err != nil {
		t.Fatalf("seeding push failed: %v (%s)", err, out)
	}

	out, err := runHelper(t, registry, "ref-prefix refs/heads/\nlist\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}

	if !strings.Contains(out, "refs/heads/keep") {
		t.Errorf("filtered list should include refs/heads/keep, got:\n%s", out)
	}
	if strings.Contains(out, "refs/tags/drop") {
		t.Errorf("filtered list should exclude refs/tags/drop, got:\n%s", out)
	}
}

// TestRefPrefixSurvivesRepeatedList: the prefix filter used to be cleared after
// the first list, so a second list in the same session came back unfiltered.
func TestRefPrefixSurvivesRepeatedList(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	branch := currentBranch(t, srcDir)
	seed := "push " + branch + ":refs/heads/keep\npush " + branch + ":refs/tags/drop\n\n"
	if out, err := runHelper(t, registry, seed); err != nil {
		t.Fatalf("seeding push failed: %v (%s)", err, out)
	}

	out, err := runHelper(t, registry, "ref-prefix refs/heads/\nlist\nlist\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}

	if strings.Contains(out, "refs/tags/drop") {
		t.Errorf("the second list should still be filtered, got:\n%s", out)
	}
}
