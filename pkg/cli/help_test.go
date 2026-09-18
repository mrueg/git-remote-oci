package cli_test

import (
	"strings"
	"testing"
)

// TestSubcommandHelpFlagIsNotAnError: `git-remote-oci gc -h` prints the usage
// and is done. The flag package reports that as flag.ErrHelp so that parsing
// stops, and passing it up as-is printed "flag: help requested" after the
// usage text and exited 1 -- a failure, for a user who asked how to use it.
func TestSubcommandHelpFlagIsNotAnError(t *testing.T) {
	for _, args := range [][]string{
		{"gc", "-h"},
		{"gc", "--help"},
		{"fsck", "-h"},
		{"break-lock", "-h"},
		{"lfs-lock", "-h"},
		{"lfs-locks", "-h"},
		{"lfs-unlock", "-h"},
		{"set-head", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, stderr, err := runCLI(t, args...)
			if err != nil {
				t.Fatalf("%v returned %v; asking for help is not a failure", args, err)
			}
			if !strings.Contains(stderr, "usage: git-remote-oci "+args[0]) {
				t.Errorf("no usage text for %s on stderr:\n%s", args[0], stderr)
			}
			if stdout != "" {
				t.Errorf("help wrote to stdout: %q", stdout)
			}
		})
	}
}

// TestSubcommandFlagErrorsAreStillErrors keeps the exemption narrow: a flag
// that does not exist is a mistake, not a request for help.
func TestSubcommandFlagErrorsAreStillErrors(t *testing.T) {
	_, _, err := runCLI(t, "gc", "--no-such-flag", "oci://localhost:1/x/y")
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
