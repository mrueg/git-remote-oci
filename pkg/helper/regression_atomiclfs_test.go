package helper_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

// commitLFSPointer commits a pointer file and stores the object it points at in
// the repository's local LFS store, which is the state git-lfs leaves behind
// after a smudge.
func commitLFSPointer(t *testing.T, dir, name string, payload []byte) {
	t.Helper()

	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
		oid, len(payload))

	gitDir := filepath.Join(dir, ".git")
	objPath := lfs.GetLFSObjectPath(gitDir, oid)
	if objPath == "" {
		t.Fatalf("GetLFSObjectPath returned empty for %s", oid)
	}
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("mkdir for LFS object: %v", err)
	}
	if err := os.WriteFile(objPath, payload, 0o644); err != nil {
		t.Fatalf("write LFS object: %v", err)
	}

	commitFile(t, dir, name, pointer, "add "+name)
}

// failBlobUpload makes the registry reject exactly the upload carrying payload,
// leaving every other blob to the normal path.
func failBlobUpload(payload []byte) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/blobs/uploads/") {
			return false
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return false
		}
		if bytes.Equal(body, payload) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		// Not ours: hand the body back so the default handler can store it.
		r.Body = io.NopCloser(bytes.NewReader(body))
		return false
	}
}

// TestAtomicPushFailsWhenAnLFSUploadFails is the regression test for a silent
// data loss on the --atomic path.
//
// That path discarded the scan error, a failure to open the local object, the
// upload error, and finally the worker group's result. A failed LFS upload
// therefore published the ref anyway, and the loss surfaced much later as a
// dangling pointer in somebody's checkout. The ordinary push path had always
// treated a failed upload as fatal; the two had simply drifted.
func TestAtomicPushFailsWhenAnLFSUploadFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"atomic", "list for-push\noption atomic true\npush refs/heads/main:refs/heads/main\n\n"},
		{"ordinary", "list for-push\npush refs/heads/main:refs/heads/main\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newMockRegistry()
			ts := reg.Server()
			defer ts.Close()

			registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
			t.Setenv("OCI_INSECURE", "1")

			src := newWorkRepo(t)
			t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

			payload := []byte("large binary payload that must reach the registry")
			commitLFSPointer(t, src, "big.bin", payload)

			reg.mu.Lock()
			reg.intercept = failBlobUpload(payload)
			reg.mu.Unlock()

			out, err := runHelper(t, registry, tc.script)

			// The helper reports per-ref problems in the protocol stream rather
			// than by returning, so a failure is either an error or an
			// "error <ref> ..." line — but never a bare "ok".
			reportedOK := strings.Contains(out, "ok refs/heads/main")
			if err == nil && reportedOK {
				t.Fatalf("the push reported success even though the LFS upload failed\noutput: %q", out)
			}
			if !strings.Contains(out, "error") && err == nil {
				t.Fatalf("no failure was reported at all\noutput: %q", out)
			}
		})
	}
}

// TestAtomicPushUploadsLFSObjects pins the positive case, so the fix above
// cannot be satisfied by simply never uploading anything.
func TestAtomicPushUploadsLFSObjects(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	payload := []byte("large binary payload that must reach the registry")
	commitLFSPointer(t, src, "big.bin", payload)

	script := "list for-push\noption atomic true\npush refs/heads/main:refs/heads/main\n\n"
	if out, err := runHelper(t, registry, script); err != nil {
		t.Fatalf("atomic push failed: %v (output %q)", err, out)
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, blob := range reg.blobs {
		if bytes.Equal(blob, payload) {
			return
		}
	}
	t.Error("the atomic push did not upload the LFS object")
}
