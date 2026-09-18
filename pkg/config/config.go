// Package config reads tunables from git's own configuration.
//
// Everything this package exposes previously existed as a compile-time
// constant: worker pool sizes, lock TTLs, the compression algorithm. Those
// defaults are reasonable on a fast link to a nearby registry and wrong
// somewhere else, and a user with a slow connection had no way to say so short
// of rebuilding.
//
// git config is the right home for them rather than more environment
// variables. It is where a git user already looks, it is per-repository
// without exporting anything, and it can be set per-remote — pushing to a
// local registry and a hosted one from the same clone are genuinely different
// situations.
//
// Two scopes are read, most specific first:
//
//	remote.<name>.oci<key>   applies to one remote
//	ociremote.<key>          applies to the repository (or globally)
//
// The environment still wins over both, so an existing OCI_COMPRESSION keeps
// working and a one-off override does not require editing config.
//
// This package is a leaf: it imports nothing internal, which is what lets the
// registry client and the protocol layer both depend on it.
package config

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Keys, without their section. Lower case throughout: git lowercases the
// section and variable of every name it reports, so a user writing
// `ociRemote.pushLockTTL` reaches the same entry as `ociremote.pushlockttl`.
const (
	KeyCompression     = "compression"
	KeyConcurrency     = "concurrency"
	KeyBlobConcurrency = "blobconcurrency"
	KeyPushLockTTL     = "pushlockttl"
	KeyIndexLockTTL    = "indexlockttl"
	KeyLFSIndexLockTTL = "lfsindexlockttl"
	// KeyShallowSnapshot enables publishing a self-contained snapshot of each
	// ref tip, which is what makes `git clone --depth 1` cheap. Off by
	// default: it costs a full copy of the tip on every push.
	KeyShallowSnapshot = "shallowsnapshot"
	// KeyProtocolV2 serves git's wire protocol version 2 over the remote-helper
	// stateless-connect capability. On by default; set it false to fall back to
	// the simple fetch/push commands, which cannot express a partial clone.
	KeyProtocolV2 = "protocolv2"
	// KeyChunkSize is how much of a blob is sent per request once an upload is
	// large enough to be worth resuming. 0 sends every blob whole.
	KeyChunkSize = "chunksize"
	// KeyCompactAfter is how many published commits a repository may
	// accumulate before a push compacts it. 0 never compacts automatically.
	KeyCompactAfter = "compactafter"
)

// Config is a snapshot of git's configuration, resolved for one remote.
//
// The zero value is usable and returns every default, which is what a caller
// running outside a repository gets.
type Config struct {
	values map[string]string
	remote string
}

// Load reads git's configuration once.
//
// remote may be empty, in which case only the repository-wide scope is
// consulted. Any failure — git absent, not a repository, a malformed file —
// yields a Config that returns defaults rather than an error: a tunable that
// cannot be read is not a reason to refuse to push.
func Load(remote string) *Config {
	// The remote name is kept as given: it is a subsection, and git keeps
	// those case-sensitive. See normaliseName.
	c := &Config{values: map[string]string{}, remote: remote}

	// -z separates entries with NUL and the name from the value with a
	// newline, so a value containing either character cannot be misparsed.
	//
	// context.Background: Load has no context to offer -- it is called at the
	// entry points, before anything cancellable exists, and never fails, so
	// there is nothing a cancellation would change.
	out, err := exec.CommandContext(context.Background(), "git", "config", "--list", "-z").Output()
	if err != nil {
		return c
	}
	for _, entry := range strings.Split(string(out), "\x00") {
		name, value, found := strings.Cut(entry, "\n")
		if !found {
			// A name with no newline is a valueless boolean, e.g. `[x] y`.
			// Git spells that "true".
			if entry != "" {
				c.values[normaliseName(entry)] = "true"
			}
			continue
		}
		c.values[normaliseName(name)] = value
	}
	return c
}

// normaliseName lowercases the section and the variable of a config name and
// leaves the subsection alone.
//
// That is git's own rule: `remote.Origin.url` and `remote.origin.url` are two
// different remotes, and `git config --list` reports the subsection exactly as
// it was written. Lowercasing the whole name folded the two together, so a
// remote whose name had a capital in it read another remote's settings -- or
// its own under a name git would never have handed over.
func normaliseName(name string) string {
	first := strings.IndexByte(name, '.')
	last := strings.LastIndexByte(name, '.')
	if first < 0 || first == last {
		return strings.ToLower(name)
	}
	return strings.ToLower(name[:first]) + name[first:last] + strings.ToLower(name[last:])
}

// lookup resolves a key, preferring the per-remote scope.
func (c *Config) lookup(key string) (string, bool) {
	if c == nil || c.values == nil {
		return "", false
	}
	if c.remote != "" {
		if v, ok := c.values["remote."+c.remote+".oci"+key]; ok && v != "" {
			return v, true
		}
	}
	v, ok := c.values["ociremote."+key]
	return v, ok && v != ""
}

// String returns a configured string, or def.
func (c *Config) String(key, def string) string {
	if v, ok := c.lookup(key); ok {
		return v
	}
	return def
}

// Int returns a configured positive integer, or def.
//
// A value that is not a positive integer is ignored rather than reported. A
// typo in a tunable must not stop a push: the cost of the default is a slower
// transfer, the cost of failing is a user unable to work.
func (c *Config) Int(key string, def int) int {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// Count returns a configured non-negative integer, or def.
//
// It differs from Int in accepting zero. Int is for sizes and pool widths,
// where zero is a typo; Count is for thresholds where zero is a real setting
// -- `compactAfter = 0` is how automatic compaction is switched off, and Int
// read that as "use the default", which switched it on.
func (c *Config) Count(key string, def int) int {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// Bool returns a configured boolean, or def.
//
// The accepted spellings are git's own: true/false, yes/no, on/off, 1/0, and a
// valueless key, which git reports as "true". Anything else falls back to def
// rather than failing, for the same reason Int does.
func (c *Config) Bool(key string, def bool) bool {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	default:
		return def
	}
}

// Duration returns a configured duration, or def.
//
// The value is a Go duration: "30s", "10m", "1h30m". A bare integer is read as
// seconds, because that is what someone writing a git config value is likely
// to mean. Anything unparseable, or not positive, falls back to def.
func (c *Config) Duration(key string, def time.Duration) time.Duration {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Bytes reads a size, accepting the `k`/`m`/`g` suffixes git itself uses.
//
// A value that does not parse falls back to the default rather than failing.
// These keys tune transfers; a typo in one should not stop a push, and the
// alternative -- refusing to run because a size was misspelt -- is worse than
// running with the default.
func (c *Config) Bytes(key string, def int64) int64 {
	raw, ok := c.lookup(key)
	if !ok {
		return def
	}
	size, err := ParseByteSize(raw)
	if err != nil {
		return def
	}
	return size
}

// ParseByteSize parses a size written the way git writes them: a decimal count
// with an optional `k`, `m` or `g` suffix, in either case.
func ParseByteSize(spec string) (int64, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, fmt.Errorf("empty size")
	}

	multiplier := int64(1)
	digits := spec
	switch unit := spec[len(spec)-1]; unit {
	case 'k', 'K':
		multiplier, digits = 1<<10, spec[:len(spec)-1]
	case 'm', 'M':
		multiplier, digits = 1<<20, spec[:len(spec)-1]
	case 'g', 'G':
		multiplier, digits = 1<<30, spec[:len(spec)-1]
	}

	value, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", spec)
	}
	if value < 0 {
		return 0, fmt.Errorf("%q is negative", spec)
	}
	if multiplier > 1 && value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%q overflows", spec)
	}
	return value * multiplier, nil
}
