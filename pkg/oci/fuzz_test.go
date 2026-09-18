package oci

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// validOCITag is the tag grammar from the OCI distribution specification.
// Declared here because the external test package has its own copy and the two

// FuzzRefEntryUnmarshalJSON exercises the custom unmarshaller behind the _refs
// index, which parses JSON fetched from a registry.
//
// The entry is a JSON object. Parsing must never panic on registry-supplied
// input, and a value that does parse must survive a round trip.
func FuzzRefEntryUnmarshalJSON(f *testing.F) {
	f.Add(`{"sha":"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d"}`)
	f.Add(`"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d"`)
	f.Add(`{"sha":"x","author":"A <a@b>","timestamp":1,"message":"m"}`)
	f.Add(`{"sha":123}`)
	f.Add(`{"timestamp":"not-a-number"}`)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, data string) {
		var entry RefEntry
		if err := json.Unmarshal([]byte(data), &entry); err != nil {
			return
		}

		// Anything that parsed must re-marshal and parse back identically.
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("a parsed RefEntry failed to marshal: %+v (%v)", entry, err)
		}
		var again RefEntry
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("re-parsing %q failed: %v", encoded, err)
		}
		if again != entry {
			t.Fatalf("round trip changed the entry: %+v -> %+v", entry, again)
		}
	})
}

// FuzzParseRetryAfter feeds arbitrary Retry-After header values to the parser.
//
// The header is registry-controlled. A negative or absurd delay would either
// busy-loop the retry logic or stall it indefinitely, so the result must always
// be a sane non-negative duration.
func FuzzParseRetryAfter(f *testing.F) {
	f.Add("1")
	f.Add("0")
	f.Add("-5")
	f.Add("99999999999999999999")
	f.Add("Wed, 21 Oct 2015 07:28:00 GMT")
	f.Add("not a date")
	f.Add("")

	f.Fuzz(func(t *testing.T, header string) {
		d := parseRetryAfter(header)
		if d < 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, which is negative", header, d)
		}

		// The parser deliberately reports whatever the header said; bounding it
		// is the backoff calculation's job. Assert that guarantee here, because
		// it is what actually protects the caller: a registry answering
		// "Retry-After: 87000" must not stall a push for a day. A Retry-After
		// is honoured past the backoff's own cap, on its own ceiling.
		rt := newRetryTransport(nil)
		for attempt := range 4 {
			backoff := rt.calculateBackoff(attempt, d)
			if backoff < 0 {
				t.Fatalf("calculateBackoff(%d, %v) = %v, which is negative", attempt, d, backoff)
			}
			if backoff > max(rt.maxInterval, maxRetryAfter) {
				t.Fatalf("calculateBackoff(%d, %v) = %v, over the %v ceiling", attempt, d, backoff, maxRetryAfter)
			}
		}
	})
}

// FuzzDecompressStream feeds arbitrary bytes to the layer decompressor.
//
// Layer content and its media type both come from the registry. The
// decompressor must never panic, and raw input must pass through untouched.
func FuzzDecompressStream(f *testing.F) {
	gz, _, _ := compressBytes([]byte("packfile"), "gzip")
	zs, _, _ := compressBytes([]byte("packfile"), "zstd")
	f.Add(gz, MediaTypeGitPackfileGzip)
	f.Add(zs, MediaTypeGitPackfileZstd)
	f.Add([]byte("PACK raw bytes"), MediaTypeGitPackfile)
	f.Add([]byte{0x1f, 0x8b, 0x00}, MediaTypeGitPackfileGzip)
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}, MediaTypeGitPackfileZstd)
	f.Add([]byte{}, "")

	f.Fuzz(func(t *testing.T, data []byte, mediaType string) {
		rc, err := DecompressStream(io.NopCloser(bytes.NewReader(data)), mediaType)
		if err != nil {
			return
		}
		// Bounded: the stream is consumed by git in production and never
		// buffered, so the only thing to check here is that reading it does
		// not misbehave, not how much it expands to.
		out, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return
		}
		// Uncompressed input must be passed through untouched.
		if mediaType == MediaTypeGitPackfile && !bytes.Equal(out, data) {
			t.Fatalf("uncompressed input was altered")
		}
	})
}
