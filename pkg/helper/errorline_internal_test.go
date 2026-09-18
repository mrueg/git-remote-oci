package helper

import (
	"errors"
	"strings"
	"testing"
)

// The reason on an `error <ref> <why>` line is very often a registry's own
// words, and stdout is line-delimited with nothing to hide behind: a newline
// in the reason ends the line early and starts another, which git parses as a
// response to a ref that was never pushed.

func TestErrorLineStaysOneLine(t *testing.T) {
	registryErr := errors.New("unexpected status 500\r\nok refs/heads/injected\ndetail:\ttab\x00nul")

	for name, line := range map[string]string{
		"failReport": failReport("refs/heads/main", "%v", registryErr).line,
		"errorLine":  errorLine("refs/heads/main", "failed: %v", registryErr),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.ContainsAny(line, "\r\n\t\x00") {
				t.Errorf("control characters reached the protocol line: %q", line)
			}
			if !strings.HasPrefix(line, "error refs/heads/main ") {
				t.Errorf("line = %q, want it to start with the error marker and the ref", line)
			}
			// The words survive; only the line structure is taken away.
			for _, word := range []string{"unexpected status 500", "injected", "detail:", "nul"} {
				if !strings.Contains(line, word) {
					t.Errorf("line %q lost %q from the message", line, word)
				}
			}
		})
	}
}

func TestSanitiseProtocolTextLeavesOrdinaryTextAlone(t *testing.T) {
	const msg = "non-fast-forward update rejected (use '+' to force): remote is abc123 — ünïcödé ok"
	if got := sanitiseProtocolText(msg); got != msg {
		t.Errorf("sanitiseProtocolText altered printable text:\n got %q\nwant %q", got, msg)
	}
}
