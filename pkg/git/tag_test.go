package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestAnnotatedTagPreservation(t *testing.T) {
	tempDir := t.TempDir()

	goRepo, err := gogit.PlainInit(tempDir, false)
	if err != nil {
		t.Fatalf("Failed to init git repo: %v", err)
	}

	wt, err := goRepo.Worktree()
	if err != nil {
		t.Fatalf("Failed to get worktree: %v", err)
	}

	dummyFile := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(dummyFile, []byte("# Test Repo"), 0644); err != nil {
		t.Fatalf("Failed to write dummy file: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("Failed to add file: %v", err)
	}

	commitHash, err := wt.Commit("Initial commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test Author",
			Email: "author@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Failed to commit: %v", err)
	}

	repo := &Repository{
		repo:   goRepo,
		storer: goRepo.Storer,
	}

	// 1. Create an annotated tag directly in git repo
	_, err = goRepo.CreateTag("v1.0.0", commitHash, &gogit.CreateTagOptions{
		Tagger: &object.Signature{
			Name:  "Release Bot",
			Email: "bot@example.com",
			When:  time.Now(),
		},
		Message: "Release v1.0.0 Production Build",
	})
	if err != nil {
		t.Fatalf("Failed to create annotated tag: %v", err)
	}

	// 2. Test GetAnnotatedTagInfo
	tagInfo, err := repo.GetAnnotatedTagInfo("refs/tags/v1.0.0")
	if err != nil {
		t.Fatalf("GetAnnotatedTagInfo failed: %v", err)
	}
	if tagInfo == nil {
		t.Fatalf("Expected tagInfo to be non-nil for annotated tag")
	}

	if tagInfo.TargetHash != commitHash.String() {
		t.Errorf("Expected target hash %s, got %s", commitHash.String(), tagInfo.TargetHash)
	}
	if tagInfo.Message != "Release v1.0.0 Production Build" {
		t.Errorf("Expected tag message %q, got %q", "Release v1.0.0 Production Build", tagInfo.Message)
	}
}
