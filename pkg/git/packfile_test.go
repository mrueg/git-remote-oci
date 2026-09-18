package git

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestThinPackfileDeduplication(t *testing.T) {
	tempDir := t.TempDir()

	goRepo, err := gogit.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	wt, err := goRepo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	// 1. Initial commit with baseline files
	filePath1 := filepath.Join(tempDir, "file1.txt")
	content1 := bytes.Repeat([]byte("Base content line for baseline thin packfile test.\n"), 500)
	if err := os.WriteFile(filePath1, content1, 0644); err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}
	if _, err := wt.Add("file1.txt"); err != nil {
		t.Fatalf("Failed to git add: %v", err)
	}
	commit1Hash, err := wt.Commit("Initial commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit 1: %v", err)
	}

	// 2. Incremental commit appending small modification
	filePath2 := filepath.Join(tempDir, "file1.txt")
	content2 := append(bytes.Clone(content1), []byte("Incremental change added to file1.\n")...)
	if err := os.WriteFile(filePath2, content2, 0644); err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}
	if _, err := wt.Add("file1.txt"); err != nil {
		t.Fatalf("Failed to git add: %v", err)
	}
	commit2Hash, err := wt.Commit("Incremental commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit 2: %v", err)
	}

	t.Setenv("GIT_DIR", filepath.Join(tempDir, ".git"))
	repo, err := OpenRepository()
	if err != nil {
		t.Fatalf("Failed to open repository: %v", err)
	}

	// Full packfile (without haveHashes)
	fullPack, err := createPackfile(repo, commit2Hash, nil)
	if err != nil {
		t.Fatalf("Failed to create full packfile: %v", err)
	}

	// Thin packfile (with haveHashes containing commit1Hash)
	thinPack, err := createPackfile(repo, commit2Hash, []plumbing.Hash{commit1Hash})
	if err != nil {
		t.Fatalf("Failed to create thin packfile: %v", err)
	}

	t.Logf("Full packfile size: %d bytes, Thin packfile size: %d bytes (Deduplication Ratio: %.2f%%)",
		len(fullPack), len(thinPack), (1.0-float64(len(thinPack))/float64(len(fullPack)))*100)

	if len(thinPack) >= len(fullPack) {
		t.Errorf("Expected thin packfile (%d bytes) to be smaller than full packfile (%d bytes)", len(thinPack), len(fullPack))
	}

	// Verify thin packfile can be unpacked/indexed by ImportPackfile
	destDir := t.TempDir()

	destGoRepo, err := gogit.PlainInit(destDir, false)
	if err != nil {
		t.Fatalf("Failed to init dest repo: %v", err)
	}

	t.Setenv("GIT_DIR", filepath.Join(destDir, ".git"))
	destRepo, err := OpenRepository()
	if err != nil {
		t.Fatalf("Failed to open dest repo: %v", err)
	}

	// Import base packfile first
	if _, err := destRepo.ImportPackfile(bytes.NewReader(fullPack)); err != nil {
		t.Fatalf("Failed to import full base packfile: %v", err)
	}

	// Import thin packfile (which references commit1 objects)
	if _, err := destRepo.ImportPackfile(bytes.NewReader(thinPack)); err != nil {
		t.Fatalf("Failed to import thin packfile: %v", err)
	}

	// Verify commit2 is accessible in dest repo
	_, err = destGoRepo.CommitObject(commit2Hash)
	if err != nil {
		t.Errorf("Failed to retrieve commit 2 after thin packfile import: %v", err)
	}
}

// createPackfile buffers CreatePackfileTo, which is what the removed
// Repository.CreatePackfile did. It lived in the package's public API and had
// no caller outside these tests.
func createPackfile(r *Repository, want plumbing.Hash, haves []plumbing.Hash) ([]byte, error) {
	var buf bytes.Buffer
	if err := r.CreatePackfileTo(&buf, want, haves); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
