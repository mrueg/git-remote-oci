package helper_test

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// A push holds its ref lock for ociremote.pushLockTTL. A large upload can
// outlive that without anyone saying so: the lock simply stops being there, a
// second client takes the ref legitimately, and the pusher learns of it at
// release at best. The helper knows both the TTL and when it took the lock, so
// it warns at 80% of the TTL and again when the TTL lapses, if the push is
// still running at those points -- and says nothing for a push that finishes
// in time.
//
// The helper runs as git's subprocess here so that its stderr is what git
// forwards to the user, which is the only place these lines matter.
func TestPushWarnsWhenItApproachesAndOutlivesTheLockTTL(t *testing.T) {
	url, reg := v2setupRegistry(t)

	const (
		nearlyLine = "has held its lock for"
		lapsedLine = "lapsed after"
	)

	// The delay lives in the registry rather than the helper: the commit
	// manifest write of a push stalls long enough for both timers to fire
	// while the lock is held. That write comes after the packfile upload and
	// before the release, so it is inside the watched window; the lock's own
	// blob and manifest writes, which come before it, are left alone -- a
	// stall there would expire the lock before its read-back and fail the
	// acquisition rather than exercise the warning. commitManifestPath is the
	// pack-chain tests' matcher for that write.
	var (
		mu    sync.Mutex
		delay time.Duration
		once  sync.Once
	)
	reg.Observe(func(method, path string) {
		mu.Lock()
		d := delay
		mu.Unlock()
		if d == 0 || method != "PUT" || !commitManifestPath.MatchString(path) {
			return
		}
		once.Do(func() { time.Sleep(d) })
	})

	t.Run("a push that outlives its TTL is warned twice", func(t *testing.T) {
		mu.Lock()
		delay = 1500 * time.Millisecond
		mu.Unlock()

		src := newWorkRepo(t)
		git(t, src, "config", "ociremote.pushLockTTL", "1s")
		out, err := v2run(t, src, nil, "push", url, "main:refs/heads/slow")
		if err != nil {
			t.Fatalf("push failed: %v\n%s", err, out)
		}
		for _, want := range []string{nearlyLine, lapsedLine} {
			if n := strings.Count(out, want); n != 1 {
				t.Errorf("expected exactly one line containing %q, got %d:\n%s", want, n, out)
			}
		}
		if !strings.Contains(out, "ociremote.pushLockTTL") {
			t.Errorf("the warning should name the setting to raise:\n%s", out)
		}
	})

	t.Run("a push that finishes in time hears nothing", func(t *testing.T) {
		mu.Lock()
		delay = 0
		mu.Unlock()

		src := newWorkRepo(t)
		git(t, src, "config", "ociremote.pushLockTTL", "5s")
		out, err := v2run(t, src, nil, "push", url, "main:refs/heads/fast")
		if err != nil {
			t.Fatalf("push failed: %v\n%s", err, out)
		}
		for _, unwanted := range []string{nearlyLine, lapsedLine} {
			if strings.Contains(out, unwanted) {
				t.Errorf("a push well inside its TTL must not warn, got:\n%s", out)
			}
		}
	})
}
