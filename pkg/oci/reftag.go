package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Ref names and OCI tags do not have the same alphabet. An OCI tag must match
// [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}, while a git ref name may contain "/" and
// many other characters, and the same short name may exist under both
// refs/heads/ and refs/tags/.
//
// EncodeRefTag maps a ref name to a tag *injectively*, so that distinct refs
// can never end up sharing - and therefore overwriting - one manifest. The one
// qualified exception is the truncated form for over-long refs (nsTruncated
// below, FORMAT.md §3.1), which is collision-resistant rather than injective by
// construction. The previous scheme replaced every illegal character with "-"
// and dropped the refs/heads/ and refs/tags/ prefixes, which silently collided:
//
//	refs/heads/feature/foo  and  refs/heads/feature-foo   -> "feature-foo"
//	refs/heads/v1           and  refs/tags/v1             -> "v1"
//
// The encoding keeps the common case readable: a branch whose name is already a
// legal tag encodes to itself, so refs/heads/main is still the tag "main".
//
// Grammar:
//
//	refs/heads/<name>  ->            escape(<name>)
//	refs/tags/<name>   ->  "_t_"  +  escape(<name>)
//	refs/<rest>        ->  "_r_"  +  escape(<rest>)
//	<anything else>    ->  "_x_"  +  escape(<name>)
//
// escape() passes [a-zA-Z0-9.-] through, doubles "_", and renders every other
// byte as "_" followed by two lowercase hex digits. Because an encoded branch
// name escapes a leading "_" to "__", it can never be mistaken for one of the
// "_t_" / "_r_" / "_x_" namespace markers.
const (
	nsTag   = "_t_"
	nsRef   = "_r_"
	nsOther = "_x_"
	// nsTruncated marks a tag whose ref name did not fit and was shortened.
	//
	// The truncated form is lossy and is the one place the mapping is not
	// injective by construction: two distinct long refs receive distinct tags
	// unless the first 32 bits of their SHA-256 digests collide, which is
	// collision-resistant -- unique in practice -- rather than guaranteed
	// (FORMAT.md §3.1).
	//
	// The marker must be reserved rather than reusing the plain form: a
	// truncated tag ends in "-<digest>", which is also perfectly ordinary
	// content, so without a marker a truncated long ref and a genuinely short
	// ref spelled the same way as the truncation would produce the identical
	// tag - a collision by construction, not by chance. escape() can never
	// emit "_h", because a lone "_" is always followed by "_" or two hex
	// digits and "h" is not hex, so this prefix is unambiguous.
	nsTruncated = "_h_"

	// maxTagLength is the OCI limit: one leading character plus 127 more.
	maxTagLength = 128
	// truncationSuffixLength is "-" plus 8 hex digits of the ref name digest.
	truncationSuffixLength = 9
)

// EncodeRefTag returns the OCI tag that stores the manifest for refName.
// It returns "" if refName is empty.
func EncodeRefTag(refName string) string {
	return encodeRefTagWithin(refName, maxTagLength)
}

// encodeRefTagWithin is EncodeRefTag with the length budget as a parameter.
//
// The budget exists for the one caller that cannot use the whole tag: a lock
// tag is the encoded ref behind LockTagPrefix, and a ref encoded to the full
// 128 bytes leaves no room for the prefix, so the lock tag exceeded the limit
// and the registry refused it — which made any ref with a long enough name
// impossible to push, since the push takes the lock first. Truncating at a
// smaller budget keeps the same scheme -- injective below the budget,
// collision-resistant above it -- just applied earlier.
func encodeRefTagWithin(refName string, budget int) string {
	if refName == "" {
		return ""
	}

	var encoded string
	switch {
	case strings.HasPrefix(refName, "refs/heads/"):
		encoded = escapeTag(strings.TrimPrefix(refName, "refs/heads/"))
	case strings.HasPrefix(refName, "refs/tags/"):
		encoded = nsTag + escapeTag(strings.TrimPrefix(refName, "refs/tags/"))
	case strings.HasPrefix(refName, "refs/"):
		encoded = nsRef + escapeTag(strings.TrimPrefix(refName, "refs/"))
	default:
		encoded = nsOther + escapeTag(refName)
	}

	if encoded == "" {
		// A branch whose name is empty is not a valid ref; treat it as
		// unrepresentable rather than emitting an empty tag.
		return ""
	}

	if len(encoded) <= budget {
		return encoded
	}

	// Too long for a tag. Mark it truncated, keep a readable prefix, and append
	// a digest of the full ref name so distinct long refs stay distinct in
	// practice; see nsTruncated for why this is not a guarantee.
	keep := truncateEscaped(encoded, budget-len(nsTruncated)-truncationSuffixLength)
	sum := sha256.Sum256([]byte(refName))
	return nsTruncated + keep + "-" + hex.EncodeToString(sum[:])[:8]
}

// escapeTag renders an arbitrary string using only characters legal in an OCI
// tag, reversibly.
func escapeTag(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		case c == '_':
			b.WriteString("__")
		default:
			b.WriteByte('_')
			b.WriteString(hex.EncodeToString([]byte{c}))
		}
	}

	out := b.String()
	// The first character of a tag must be [a-zA-Z0-9_]; "." and "-" are legal
	// only from the second position onwards.
	if out != "" && (out[0] == '.' || out[0] == '-') {
		return "_" + hex.EncodeToString([]byte{out[0]}) + out[1:]
	}
	return out
}

// truncateEscaped shortens an escaped string to at most limit bytes without
// cutting an escape sequence in half.
func truncateEscaped(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	// Walk escape sequences from the start so we always stop on a boundary.
	end := 0
	for end < limit {
		next := end + 1
		if s[end] == '_' {
			if end+1 < len(s) && s[end+1] == '_' {
				next = end + 2
			} else {
				next = end + 3
			}
		}
		if next > limit {
			break
		}
		end = next
	}

	out := strings.TrimRight(s[:end], "-.")
	if out == "" {
		// Everything was trimmed; the digest suffix alone identifies the ref,
		// but a tag may not start with "-", so give it a stable prefix.
		return "ref"
	}
	return out
}
