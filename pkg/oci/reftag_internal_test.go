package oci

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// decodeRefTag is the inverse of EncodeRefTag for tags that were not truncated.
// It returns an error for tags that are not valid output of EncodeRefTag, and
// for truncated tags, whose original ref name is not recoverable.
//
// It lives with the tests because nothing in production decodes a tag: the ref
// name is always read from the manifest's annotation, never recovered from the
// tag. The decoder exists to prove the encoding is reversible, and therefore
// injective, which is the property the tests below pin.
func decodeRefTag(tag string) (string, error) {
	switch {
	case tag == "":
		return "", fmt.Errorf("empty tag")
	case strings.HasPrefix(tag, nsTruncated):
		return "", fmt.Errorf("tag %q was truncated; the original ref name is not recoverable", tag)
	case strings.HasPrefix(tag, nsTag):
		name, err := unescapeTag(strings.TrimPrefix(tag, nsTag))
		if err != nil {
			return "", err
		}
		return "refs/tags/" + name, nil
	case strings.HasPrefix(tag, nsRef):
		name, err := unescapeTag(strings.TrimPrefix(tag, nsRef))
		if err != nil {
			return "", err
		}
		return "refs/" + name, nil
	case strings.HasPrefix(tag, nsOther):
		return unescapeTag(strings.TrimPrefix(tag, nsOther))
	default:
		name, err := unescapeTag(tag)
		if err != nil {
			return "", err
		}
		return "refs/heads/" + name, nil
	}
}

// unescapeTag reverses escapeTag.
func unescapeTag(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '_' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '_' {
			b.WriteByte('_')
			i += 2
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated escape sequence at offset %d in %q", i, s)
		}
		decoded, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("invalid escape sequence at offset %d in %q: %w", i, s, err)
		}
		b.WriteByte(decoded[0])
		i += 3
	}
	return b.String(), nil
}

// ociTagPattern is the tag grammar from the OCI distribution specification.
// The external test file keeps its own copy; these are different packages.
var ociTagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

func TestEncodeRefTagRoundTrips(t *testing.T) {
	refs := []string{
		"refs/heads/main",
		"refs/heads/feature/login",
		"refs/heads/my_branch",
		"refs/heads/-dash",
		"refs/heads/.dot",
		"refs/tags/v1.0.0",
		"refs/tags/nested/tag",
		"refs/notes/commits",
		"refs/heads/ünïcödé",
		"HEAD",
	}

	for _, ref := range refs {
		tag := EncodeRefTag(ref)
		got, err := decodeRefTag(tag)
		if err != nil {
			t.Errorf("decodeRefTag(%q) from ref %q: %v", tag, ref, err)
			continue
		}
		if got != ref {
			t.Errorf("round trip of %q via %q gave %q", ref, tag, got)
		}
	}
}

// FuzzEncodeRefTag asserts the two properties the encoding exists to provide:
// every output is a legal OCI tag, and the mapping is reversible - which
// implies it is injective, so two refs can never share a manifest.
func FuzzEncodeRefTag(f *testing.F) {
	seeds := []string{
		"refs/heads/main",
		"refs/heads/feature/login",
		"refs/tags/v1.0.0",
		"refs/heads/_underscore",
		"refs/heads/-dash",
		"refs/heads/..",
		"refs/heads/ünïcödé",
		"refs/heads/with space",
		"HEAD",
		"refs/",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, ref string) {
		tag := EncodeRefTag(ref)
		if ref == "" {
			if tag != "" {
				t.Fatalf("EncodeRefTag(%q) = %q, want an empty tag", ref, tag)
			}
			return
		}
		if tag == "" {
			// Only refs with no representable short name may encode to "".
			return
		}

		if !ociTagPattern.MatchString(tag) {
			t.Fatalf("EncodeRefTag(%q) = %q, which is not a valid OCI tag", ref, tag)
		}

		got, err := decodeRefTag(tag)
		if err != nil {
			// Only a truncated tag may refuse to decode, and truncation is
			// lossy by construction: the digest suffix keeps distinct refs
			// distinct without preserving the name.
			if !strings.HasPrefix(tag, "_h_") {
				t.Fatalf("decodeRefTag(%q) from ref %q failed: %v", tag, ref, err)
			}
			return
		}

		// Anything that decodes must decode back to exactly the ref it came
		// from. A truncated tag reaching this point would mean a long ref and
		// some short ref share a tag, which is the collision this encoding
		// exists to prevent.
		if got != ref {
			t.Fatalf("round trip of %q via %q gave %q", ref, tag, got)
		}
	})
}

// TestTruncatedTagsCannotCollideWithShortRefs is the regression test for a
// collision the fuzzer found in the first version of this encoding.
//
// A truncated tag ends in "-<digest>", which is also perfectly ordinary
// content. Without a reserved marker, decoding a truncated tag yielded a
// plausible short ref name that re-encoded to the *same* tag — so a long ref and
// that short ref shared one manifest, which is exactly what this encoding exists
// to prevent.
func TestTruncatedTagsCannotCollideWithShortRefs(t *testing.T) {
	longRef := "refs/heads/" + strings.Repeat("segment/", 40) + "tip"
	longTag := EncodeRefTag(longRef)

	if !strings.HasPrefix(longTag, "_h_") {
		t.Fatalf("expected a truncated tag to be marked, got %q", longTag)
	}
	if _, err := decodeRefTag(longTag); err == nil {
		t.Errorf("a truncated tag must not claim to decode: %q", longTag)
	}

	// The short ref spelled exactly like the truncated tag's payload must get a
	// different tag.
	shortRef := "refs/heads/" + strings.TrimPrefix(longTag, "_h_")
	if shortTag := EncodeRefTag(shortRef); shortTag == longTag {
		t.Errorf("long ref %q and short ref %q both encode to %q", longRef, shortRef, longTag)
	}
}

// TestLockTagFitsForRefsThatFillTheTagLimit pins the lock tag's length budget.
//
// A ref whose encoding is 125-128 bytes is a legal ref tag, but the lock tag
// prefixes it, and "_lock_" plus 128 bytes is not a legal tag at all: the
// registry refused it, so the ref could not be locked and therefore could not
// be pushed. The lock tag has to be truncated at a budget that leaves room for
// the prefix, by the same injective scheme, so that distinct refs still get
// distinct locks.
func TestLockTagFitsForRefsThatFillTheTagLimit(t *testing.T) {
	seen := map[string]string{}
	for _, n := range []int{125, 126, 127, 128} {
		// "refs/heads/" + n bytes of name encodes to n bytes, since the
		// prefix is dropped and the name is already legal tag content.
		for _, variant := range []string{"a", "b"} {
			ref := "refs/heads/" + strings.Repeat(variant, n)
			encoded := EncodeRefTag(ref)
			if len(encoded) < 125 || len(encoded) > maxTagLength {
				t.Fatalf("test setup: %q encodes to %d bytes, want 125..128", ref, len(encoded))
			}
			if strings.HasPrefix(encoded, nsTruncated) {
				t.Fatalf("test setup: the ref tag for %q should not itself be truncated", ref)
			}

			tag := LockTag(ref)
			if len(tag) > maxTagLength {
				t.Errorf("LockTag(%q) is %d bytes, over the %d-byte tag limit", ref, len(tag), maxTagLength)
			}
			if !ociTagPattern.MatchString(tag) {
				t.Errorf("LockTag(%q) = %q is not a legal OCI tag", ref, tag)
			}
			if !strings.HasPrefix(tag, LockTagPrefix+nsTruncated) {
				t.Errorf("LockTag(%q) = %q should be marked as truncated", ref, tag)
			}
			if other, dup := seen[tag]; dup {
				t.Errorf("refs %q and %q share the lock tag %q", other, ref, tag)
			}
			seen[tag] = ref
		}
	}

	// A ref that fits the smaller budget keeps the tag it always had, so
	// nothing changes for the refs that could be locked before.
	short := "refs/heads/feature/login"
	if got, want := LockTag(short), LockTagPrefix+EncodeRefTag(short); got != want {
		t.Errorf("LockTag(%q) = %q, want %q", short, got, want)
	}
}
