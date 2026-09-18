package oci

import (
	"bytes"
	"io"
	"testing"
)

// compressBytes runs data through CompressStream, which is the only
// compressor production code has: the whole-buffer variants were removed once
// nothing but these tests called them.
func compressBytes(data []byte, mode string) ([]byte, string, error) {
	var buf bytes.Buffer
	cw, mediaType, err := CompressStream(&buf, mode)
	if err != nil {
		return nil, "", err
	}
	if _, err := cw.Write(data); err != nil {
		return nil, "", err
	}
	if err := cw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mediaType, nil
}

// compressForTest is compressBytes for a test that cannot proceed without it.
func compressForTest(t *testing.T, data []byte, mode string) ([]byte, string) {
	t.Helper()
	out, mediaType, err := compressBytes(data, mode)
	if err != nil {
		t.Fatalf("compress (%s): %v", mode, err)
	}
	return out, mediaType
}

func TestPackfileCompression(t *testing.T) {
	rawPayload := []byte("hello-git-packfile-data-compression-test-payload-1234567890")

	modes := []string{"gzip", "zstd", "none"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			compressed, mediaType := compressForTest(t, rawPayload, mode)

			rc, err := DecompressStream(io.NopCloser(bytes.NewReader(compressed)), mediaType)
			if err != nil {
				t.Fatalf("DecompressStream failed for mode %s: %v", mode, err)
			}
			decompressed, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read failed for mode %s: %v", mode, err)
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("close failed for mode %s: %v", mode, err)
			}

			if string(decompressed) != string(rawPayload) {
				t.Errorf("Mismatch in decompressed payload: expected %q, got %q", rawPayload, decompressed)
			}
		})
	}
}
