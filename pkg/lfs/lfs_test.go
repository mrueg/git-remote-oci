package lfs_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

func TestLFSPointerParsing(t *testing.T) {
	validPointerText := []byte("version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f553fd865fc99d0d21057f8646b9a89680327f29a008\nsize 12345678\n")

	ptr := lfs.ParsePointer(validPointerText)
	if ptr == nil {
		t.Fatalf("ParsePointer returned nil for valid LFS pointer")
	}
	if ptr.Oid != "4d7a214614ab2935c943f553fd865fc99d0d21057f8646b9a89680327f29a008" {
		t.Errorf("Unexpected OID: %s", ptr.Oid)
	}
	if ptr.Size != 12345678 {
		t.Errorf("Unexpected size: %d", ptr.Size)
	}

	invalidText := []byte("This is not an LFS pointer file.\n")
	if lfs.ParsePointer(invalidText) != nil {
		t.Errorf("ParsePointer should return nil for non-LFS pointer content")
	}
}

func TestLFSStorage(t *testing.T) {
	tempGitDir := t.TempDir()

	payload := []byte("Hello, Git LFS OCI payload!")
	// The OID must be the SHA-256 of the payload; StoreLFSObject verifies it.
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	if err := lfs.StoreLFSObject(tempGitDir, oid, bytes.NewReader(payload)); err != nil {
		t.Fatalf("StoreLFSObject failed: %v", err)
	}

	path := lfs.GetLFSObjectPath(tempGitDir, oid)
	if path == "" {
		t.Fatalf("GetLFSObjectPath returned empty")
	}

	readPayload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read stored LFS object: %v", err)
	}

	if !bytes.Equal(readPayload, payload) {
		t.Errorf("Stored payload mismatch, got %q, expected %q", string(readPayload), string(payload))
	}
}

// TestValidateOIDRejectsMalformed pins the rule that LFS object ids, which
// arrive from registry-supplied annotations, must be exactly 64 hex characters.
// Anything else could escape the object directory once joined into a path.
func TestValidateOIDRejectsMalformed(t *testing.T) {
	valid := strings.Repeat("ab", 32)

	tests := []struct {
		name    string
		oid     string
		wantErr bool
	}{
		{"valid", valid, false},
		{"valid with sha256 prefix", "sha256:" + valid, false},
		{"uppercase is normalised", strings.ToUpper(valid), false},
		{"empty", "", true},
		{"too short", "abcd", true},
		{"too long", valid + "ab", true},
		{"parent traversal", "../../../../etc/passwd", true},
		{"padded traversal", strings.Repeat("a", 58) + "/../..", true},
		{"path separator", strings.Repeat("a", 32) + "/" + strings.Repeat("b", 31), true},
		{"non-hex", strings.Repeat("z", 64), true},
		{"null byte", strings.Repeat("a", 63) + "\x00", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lfs.ValidateOID(tt.oid)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateOID(%q) = %q, want an error", tt.oid, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateOID(%q) returned unexpected error: %v", tt.oid, err)
			}
			if got != strings.ToLower(strings.TrimPrefix(tt.oid, "sha256:")) {
				t.Errorf("ValidateOID(%q) = %q", tt.oid, got)
			}
		})
	}
}

// TestStoreLFSObjectRejectsTraversal verifies that a hostile OID cannot cause a
// write outside the LFS object directory.
func TestStoreLFSObjectRejectsTraversal(t *testing.T) {
	gitDir := t.TempDir()
	outside := filepath.Join(gitDir, "pwned")

	err := lfs.StoreLFSObject(gitDir, "../../pwned", bytes.NewReader([]byte("payload")))
	if err == nil {
		t.Fatal("StoreLFSObject accepted a traversing OID")
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatalf("StoreLFSObject wrote outside the object directory: %s exists", outside)
	}
}

// TestStoreLFSObjectRejectsContentMismatch verifies that content which does not
// hash to its advertised OID is discarded rather than stored as authentic.
func TestStoreLFSObjectRejectsContentMismatch(t *testing.T) {
	gitDir := t.TempDir()

	announced := sha256.Sum256([]byte("what we asked for"))
	oid := hex.EncodeToString(announced[:])

	err := lfs.StoreLFSObject(gitDir, oid, bytes.NewReader([]byte("what we actually got")))
	if err == nil {
		t.Fatal("StoreLFSObject accepted content that does not match its OID")
	}

	path := lfs.GetLFSObjectPath(gitDir, oid)
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("mismatched content was published at %s", path)
	}

	// No temporary files should be left behind either.
	dir := filepath.Dir(path)
	entries, readErr := os.ReadDir(dir)
	if readErr == nil && len(entries) != 0 {
		t.Errorf("expected no leftover files in %s, found %d", dir, len(entries))
	}
}
