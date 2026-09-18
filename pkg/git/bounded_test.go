package git

import (
	"strings"
	"testing"
)

// boundedBuffer is what a subprocess's stderr is written into: enough of it to
// quote in an error, and never a short write, however much git says.
func TestBoundedBufferKeepsAPrefixAndNeverShortWrites(t *testing.T) {
	var b boundedBuffer

	n, err := b.Write([]byte("  first\n"))
	if err != nil || n != 8 {
		t.Fatalf("Write = %d, %v; want 8, nil", n, err)
	}

	big := strings.Repeat("x", 2*boundedBufferLimit)
	n, err = b.Write([]byte(big))
	if err != nil || n != len(big) {
		t.Fatalf("Write of %d bytes = %d, %v; a short write would make exec report a broken pipe", len(big), n, err)
	}
	if got := len(b.buf.String()); got != boundedBufferLimit {
		t.Errorf("buffer holds %d bytes, want exactly the limit %d", got, boundedBufferLimit)
	}
	if !strings.HasPrefix(b.String(), "first") {
		t.Errorf("String() = %q..., want the trimmed prefix of what was written", b.String()[:16])
	}

	// Once full, further writes are still accepted and still dropped.
	if n, err := b.Write([]byte("more")); err != nil || n != 4 {
		t.Errorf("Write after the limit = %d, %v; want 4, nil", n, err)
	}
}
