package helper

import (
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

// TestCommonDirPathsRefuseToGuessOutsideARepository: with GIT_DIR unset and no
// .git anywhere above the working directory, the shallow file and the LFS
// object cache have no home. Writing them under ./.git used to succeed
// quietly and leave a fetch that reported success while git could not see
// what it had written. Git itself always sets GIT_DIR before running a remote
// helper, so this guards the caller that is not git.
func TestCommonDirPathsRefuseToGuessOutsideARepository(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	t.Setenv("GIT_WORK_TREE", "")
	t.Chdir(t.TempDir())

	const want = "failed to locate the git repository"

	h := &Helper{}

	err := h.markShallowBoundary("0123456789012345678901234567890123456789")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("markShallowBoundary outside a repository: got %v, want an error containing %q", err, want)
	}

	// One LFS layer with a well-formed OID gets past validation and reaches the
	// path lookup; nothing further is attempted because the error comes first.
	oid := strings.Repeat("ab", 32)
	manifest := &ocispec.Manifest{
		Layers: []ocispec.Descriptor{{
			MediaType:   lfs.MediaTypeGitLFSBlob,
			Annotations: map[string]string{lfs.AnnotationLFSOID: oid},
		}},
	}
	err = h.downloadLFSObjects(t.Context(), "0123456789012345678901234567890123456789", manifest, "")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("downloadLFSObjects outside a repository: got %v, want an error containing %q", err, want)
	}
}
