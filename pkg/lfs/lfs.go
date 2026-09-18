package lfs

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MediaTypeGitLFSBlob is the custom OCI layer media type for Git LFS payload blobs
const MediaTypeGitLFSBlob = "application/vnd.git.lfs.v1+blob"

// AnnotationLFSOID is the OCI layer annotation carrying the LFS pointer's
// object id; AnnotationLFSSize carries the object's size in bytes.
const AnnotationLFSOID = "org.git.lfs.oid"
const AnnotationLFSSize = "org.git.lfs.size"

// Pointer represents a Git LFS pointer file
type Pointer struct {
	Oid  string // SHA256 hex string without "sha256:" prefix
	Size int64  // File size in bytes
}

// ParsePointer parses content bytes into an LFS Pointer, returning nil if the
// content is not a well-formed LFS pointer.
//
// The object id is validated here rather than only at the point of use. Pointer
// text arrives from a git tree, so anyone who can land a commit controls it,
// and callers turn the id straight into a filesystem path. Rejecting a
// malformed id at the parser means the file is simply treated as ordinary
// content, which is the safe reading.
func ParsePointer(content []byte) *Pointer {
	if !bytes.HasPrefix(content, []byte("version https://git-lfs.github.com/spec/v1")) {
		return nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(content))
	var ptr Pointer
	sawSize := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "oid sha256:") {
			ptr.Oid = strings.TrimPrefix(line, "oid sha256:")
		} else if strings.HasPrefix(line, "size ") {
			if s, err := strconv.ParseInt(strings.TrimPrefix(line, "size "), 10, 64); err == nil && s >= 0 {
				ptr.Size = s
				sawSize = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil
	}

	// A zero-byte object is legitimate, so presence of the size field is what
	// matters, not a non-zero value.
	if !sawSize {
		return nil
	}
	clean, err := ValidateOID(ptr.Oid)
	if err != nil {
		return nil
	}
	ptr.Oid = clean
	return &ptr
}

// oidLength is the hex length of a Git LFS object id (SHA-256).
const oidLength = 64

// ValidateOID checks that oid is a well-formed Git LFS object id and returns it
// with any "sha256:" prefix removed.
//
// LFS object ids arrive from registry-supplied layer annotations, i.e. from an
// untrusted source, and are used to build filesystem paths. Anything that is not
// exactly 64 lowercase-or-uppercase hex characters is rejected, which also rules
// out separators and "..".
func ValidateOID(oid string) (string, error) {
	cleanOID := strings.TrimPrefix(oid, "sha256:")
	if len(cleanOID) != oidLength {
		return "", fmt.Errorf("invalid LFS OID %q: expected %d hex characters, got %d", oid, oidLength, len(cleanOID))
	}
	for _, r := range cleanOID {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return "", fmt.Errorf("invalid LFS OID %q: contains non-hex character %q", oid, r)
		}
	}
	return strings.ToLower(cleanOID), nil
}

// GetLFSObjectPath returns local storage path inside .git/lfs/objects/xx/yy/xxyy...
// It returns "" if oid is not a valid LFS object id.
func GetLFSObjectPath(gitDir, oid string) string {
	cleanOID, err := ValidateOID(oid)
	if err != nil {
		return ""
	}
	return filepath.Join(gitDir, "lfs", "objects", cleanOID[0:2], cleanOID[2:4], cleanOID)
}

// StoreLFSObject writes payload into local .git/lfs/objects/xx/yy/xxyy...
//
// The payload is hashed while it is written and the result is compared against
// oid. A mismatch means the registry served content that is not the object we
// asked for, so the temporary file is discarded rather than published under a
// name that claims to describe it.
func StoreLFSObject(gitDir, oid string, r io.Reader) error {
	cleanOID, err := ValidateOID(oid)
	if err != nil {
		return err
	}

	dirPath := filepath.Join(gitDir, "lfs", "objects", cleanOID[0:2], cleanOID[2:4])
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return err
	}
	filePath := filepath.Join(dirPath, cleanOID)

	// A unique temporary name per call: the same object can legitimately be
	// downloaded concurrently by several fetch workers, and a shared
	// "<oid>.tmp" would let them interleave writes and race on the rename.
	f, err := os.CreateTemp(dirPath, cleanOID+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), r); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if got := hex.EncodeToString(hasher.Sum(nil)); got != cleanOID {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("LFS object content does not match its OID: expected %s, got %s", cleanOID, got)
	}

	return os.Rename(tmpPath, filePath)
}
