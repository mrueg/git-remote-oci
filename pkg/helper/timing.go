package helper

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Where the time went.
//
// A clone that feels slow is slow somewhere specific, and the candidates are
// not close together: resolving the pack graph is round trips, transferring
// packfiles is bandwidth, and index-pack is local CPU. Guessing between them
// from a wall-clock total is how an afternoon gets spent optimising the wrong
// one.
//
// `GIT_REMOTE_OCI_TIMING=1` prints a breakdown to stderr when the helper exits.
// Off otherwise, and the instrumentation costs a mutex and two time.Now calls
// per phase even when it is on, so it is left in the hot paths rather than
// compiled around.

// timingEnv turns the breakdown on.
const timingEnv = "GIT_REMOTE_OCI_TIMING"

// phaseTimer accumulates how long was spent in each named phase.
//
// Phases run concurrently -- twelve workers fetching packfiles are twelve
// overlapping intervals -- so the totals are time *spent*, summed across
// workers, and add up to more than the wall clock. That is the useful number
// for "which phase is the work in"; the wall clock is reported beside it so the
// two are not confused.
type phaseTimer struct {
	enabled bool
	start   time.Time

	mu    sync.Mutex
	order []string
	total map[string]time.Duration
	count map[string]int
}

// newPhaseTimer returns a timer that is enabled only if the environment asks.
func newPhaseTimer(getenv func(string) string) *phaseTimer {
	if getenv == nil {
		getenv = os.Getenv
	}
	value := strings.TrimSpace(getenv(timingEnv))
	return &phaseTimer{
		enabled: value != "" && value != "0" && !strings.EqualFold(value, "false"),
		start:   time.Now(),
		total:   map[string]time.Duration{},
		count:   map[string]int{},
	}
}

// phase starts timing and returns the function that stops it.
//
// The returned function is safe to call from any goroutine and safe to call
// when timing is off, which is what lets call sites be unconditional:
//
//	defer h.timer.phase("fetch packfiles")()
func (t *phaseTimer) phase(name string) func() {
	if t == nil || !t.enabled {
		return func() {}
	}
	started := time.Now()
	return func() {
		elapsed := time.Since(started)
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, seen := t.total[name]; !seen {
			t.order = append(t.order, name)
		}
		t.total[name] += elapsed
		t.count[name]++
	}
}

// report writes the breakdown, slowest phase first.
//
// The writer is stderr, a diagnostic channel: a write that fails has nowhere
// better to be reported, so the errors are dropped.
func (t *phaseTimer) report(w io.Writer) {
	if t == nil || !t.enabled {
		return
	}
	wall := time.Since(t.start)

	t.mu.Lock()
	names := append([]string(nil), t.order...)
	totals := make(map[string]time.Duration, len(t.total))
	counts := make(map[string]int, len(t.count))
	for _, name := range names {
		totals[name] = t.total[name]
		counts[name] = t.count[name]
	}
	t.mu.Unlock()

	if len(names) == 0 {
		_, _ = fmt.Fprintf(w, "git-remote-oci: [timing] %s wall, no phases recorded\n", round(wall))
		return
	}

	// Slowest first: the question this output answers is "what should I look
	// at", and the answer is the top line.
	sort.SliceStable(names, func(i, j int) bool { return totals[names[i]] > totals[names[j]] })

	width := 0
	for _, name := range names {
		if len(name) > width {
			width = len(name)
		}
	}

	_, _ = fmt.Fprintf(w, "git-remote-oci: [timing] %s wall\n", round(wall))
	for _, name := range names {
		_, _ = fmt.Fprintf(w, "git-remote-oci: [timing]   %-*s  %9s  (%d call%s)\n",
			width, name, round(totals[name]), counts[name], plural(counts[name]))
	}
	// Said explicitly, because a reader who adds the column up and gets more
	// than the wall clock will otherwise assume the numbers are wrong.
	_, _ = fmt.Fprint(w, "git-remote-oci: [timing]   phases overlap across workers, so these sum to more than the wall time\n")
}

// round trims a duration to something readable. Nanosecond precision on a
// network operation is noise dressed as data.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
