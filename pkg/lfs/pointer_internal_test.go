package lfs

import (
	"fmt"
	"strings"
	"testing"
)

// formatPointer renders the canonical LFS pointer text. Production code only
// ever parses pointers, so the serialiser lives with the test that round-trips
// them.
func (p *Pointer) formatPointer() string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", p.Oid, p.Size)
}

// FuzzParsePointer feeds arbitrary bytes to the LFS pointer parser.
//
// Pointer text comes out of a git tree, so it is attacker-influenced by anyone
// who can get a commit into the repository. The parser must never panic, and
// anything it accepts must be usable: a returned OID has to be a valid object
// id, because callers turn it straight into a filesystem path.
func FuzzParsePointer(f *testing.F) {
	// The exact line ParsePointer requires. Every seed below said
	// "git-github.com" — the real spec URL is "git-lfs.github.com" — so all of
	// them were rejected on the first line and the body of this target never
	// ran on any of them. The interesting seeds are the malformed ones, and
	// they were being thrown away before reaching the code that rejects them.
	// Naming it once is what stops that happening again.
	const version = "version https://git-lfs.github.com/spec/v1\n"

	f.Add([]byte(version + "oid sha256:" + strings.Repeat("ab", 32) + "\nsize 1234\n"))
	f.Add([]byte(version))
	f.Add([]byte(version + "oid sha256:\nsize 0\n"))
	f.Add([]byte(version + "oid sha256:zz\nsize -1\n"))
	f.Add([]byte(version + "oid sha256:../../etc/passwd\nsize 5\n"))
	f.Add([]byte(version + "size 99999999999999999999\n"))
	f.Add([]byte(version + "oid sha256:" + strings.Repeat("AB", 32) + "\nsize 0\n"))
	f.Add([]byte(""))
	f.Add([]byte("version https://git-github.com/spec/v1\noid sha256:" +
		strings.Repeat("ab", 32) + "\nsize 1\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		ptr := ParsePointer(data)
		if ptr == nil {
			return
		}

		// Whatever the parser accepts, the OID must survive validation -
		// otherwise the push path would build a path from it and fail late,
		// or worse, escape the object directory.
		if _, err := ValidateOID(ptr.Oid); err != nil {
			t.Fatalf("ParsePointer accepted an OID that ValidateOID rejects: %q (%v)", ptr.Oid, err)
		}
		if ptr.Size < 0 {
			t.Fatalf("ParsePointer returned a negative size %d", ptr.Size)
		}

		// A pointer we accept must round-trip through its canonical form.
		reparsed := ParsePointer([]byte(ptr.formatPointer()))
		if reparsed == nil {
			t.Fatalf("canonical form of %+v does not re-parse: %q", ptr, ptr.formatPointer())
		}
		if reparsed.Oid != ptr.Oid || reparsed.Size != ptr.Size {
			t.Fatalf("round trip changed the pointer: %+v -> %+v", ptr, reparsed)
		}
	})
}
