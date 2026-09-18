package oci_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// The docker config tests cover the `auths` entries. These cover the other
// half of the credential store: native credential helpers, which the README
// promises and which nothing exercised. A helper is a `docker-credential-<name>`
// executable on PATH that is asked for the server address on stdin and answers
// with JSON; `credHelpers` names one per registry host, `credsStore` names one
// for every host.

// fakeCredentialHelper installs a `docker-credential-<name>` on PATH.
//
// The script logs each server address it is asked about to a file, so a test
// can prove the helper was consulted and for which host, and then behaves as
// script says: printing a credential, or failing the way a real helper does.
func fakeCredentialHelper(t *testing.T, binDir, name, script string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake credential helper is a shell script")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH to run the fake credential helper")
	}

	log = filepath.Join(t.TempDir(), name+".log")
	body := "#!/bin/sh\n" +
		"read -r server\n" +
		"printf '%s\\n' \"$server\" >> " + log + "\n" +
		script + "\n"
	path := filepath.Join(binDir, "docker-credential-"+name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake credential helper: %v", err)
	}
	return log
}

// answering is the script body of a helper that returns bob's credential
// with the given secret.
func answering(pass string) string {
	return `printf '{"ServerURL":"%s","Username":"bob","Secret":"` + pass + `"}\n' "$server"`
}

// writeDockerConfig points DOCKER_CONFIG at a config.json with the given body.
func writeDockerConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal docker config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

// helperEnv clears every other credential source and prepends binDir to PATH.
func helperEnv(t *testing.T) (binDir string) {
	t.Helper()
	clearCredentialEnv(t)
	binDir = t.TempDir()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir
}

// askedFor reads the hosts a fake helper was consulted about.
func askedFor(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read helper log: %v", err)
	}
	return strings.Fields(string(raw))
}

// TestAuthCredHelperForTheRegistryHostIsInvoked: a `credHelpers` entry keyed
// by the registry host names the helper that answers for it.
func TestAuthCredHelperForTheRegistryHostIsInvoked(t *testing.T) {
	binDir := helperEnv(t)
	reg := newAuthRegistry(t, "bob", "hunter2")
	host := strings.TrimPrefix(reg.server.URL, "http://")

	log := fakeCredentialHelper(t, binDir, "perhost", answering("hunter2"))
	writeDockerConfig(t, map[string]any{
		"credHelpers": map[string]any{host: "perhost"},
	})

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
		t.Fatalf("the credential helper's answer was not used: %v", err)
	}

	if _, authorized := reg.counts(); authorized == 0 {
		t.Error("no request reached the API authenticated with the helper's credential")
	}
	hosts := askedFor(t, log)
	if len(hosts) == 0 {
		t.Fatal("the credential helper was never run")
	}
	for _, h := range hosts {
		if h != host {
			t.Errorf("the helper was asked about %q, want the registry host %q", h, host)
		}
	}
}

// TestAuthCredsStoreIsInvokedForEveryHost: `credsStore` is the default helper
// when no per-host entry matches.
func TestAuthCredsStoreIsInvokedForEveryHost(t *testing.T) {
	binDir := helperEnv(t)
	reg := newAuthRegistry(t, "bob", "hunter2")
	host := strings.TrimPrefix(reg.server.URL, "http://")

	log := fakeCredentialHelper(t, binDir, "global", answering("hunter2"))
	writeDockerConfig(t, map[string]any{
		"credsStore": "global",
	})

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
		t.Fatalf("the credsStore helper's answer was not used: %v", err)
	}
	if hosts := askedFor(t, log); len(hosts) == 0 || hosts[0] != host {
		t.Errorf("credsStore helper was asked about %v, want %q", hosts, host)
	}
}

// TestAuthCredHelperPrecedence pins the order docker itself uses: a per-host
// `credHelpers` entry beats `credsStore`, and either beats a plain `auths`
// entry for the same host, which is not consulted at all once a helper is
// configured.
func TestAuthCredHelperPrecedence(t *testing.T) {
	t.Run("credHelpers over credsStore", func(t *testing.T) {
		binDir := helperEnv(t)
		reg := newAuthRegistry(t, "bob", "hunter2")
		host := strings.TrimPrefix(reg.server.URL, "http://")

		goodLog := fakeCredentialHelper(t, binDir, "good", answering("hunter2"))
		badLog := fakeCredentialHelper(t, binDir, "bad", answering("wrong"))
		writeDockerConfig(t, map[string]any{
			"credHelpers": map[string]any{host: "good"},
			"credsStore":  "bad",
		})

		client, err := oci.NewClient(reg.repoRef(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
			t.Fatalf("the per-host helper should have answered: %v", err)
		}
		if len(askedFor(t, goodLog)) == 0 {
			t.Error("the per-host helper was not consulted")
		}
		if asked := askedFor(t, badLog); len(asked) != 0 {
			t.Errorf("credsStore helper was consulted (%v) although a per-host helper is configured", asked)
		}
	})

	t.Run("credHelpers over auths", func(t *testing.T) {
		binDir := helperEnv(t)
		reg := newAuthRegistry(t, "bob", "hunter2")
		host := strings.TrimPrefix(reg.server.URL, "http://")

		fakeCredentialHelper(t, binDir, "good", answering("hunter2"))
		writeDockerConfig(t, map[string]any{
			"auths": map[string]any{
				host: map[string]any{"auth": base64.StdEncoding.EncodeToString([]byte("bob:wrong"))},
			},
			"credHelpers": map[string]any{host: "good"},
		})

		client, err := oci.NewClient(reg.repoRef(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
			t.Fatalf("the helper should win over the auths entry: %v", err)
		}
	})

	t.Run("credsStore over auths", func(t *testing.T) {
		binDir := helperEnv(t)
		reg := newAuthRegistry(t, "bob", "hunter2")
		host := strings.TrimPrefix(reg.server.URL, "http://")

		fakeCredentialHelper(t, binDir, "global", answering("hunter2"))
		writeDockerConfig(t, map[string]any{
			"auths": map[string]any{
				host: map[string]any{"auth": base64.StdEncoding.EncodeToString([]byte("bob:wrong"))},
			},
			"credsStore": "global",
		})

		client, err := oci.NewClient(reg.repoRef(), true)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if _, err := client.EnumerateTagRefs(context.Background()); err != nil {
			t.Fatalf("credsStore should win over the auths entry: %v", err)
		}
	})
}

// TestAuthCredHelperWithNoCredentialIsAnonymous: a helper that reports
// "credentials not found in native keychain" is the documented way of saying
// it has nothing for this host, and that is anonymous access, not an error.
func TestAuthCredHelperWithNoCredentialIsAnonymous(t *testing.T) {
	binDir := helperEnv(t)
	reg := newAuthRegistry(t, "bob", "hunter2")
	host := strings.TrimPrefix(reg.server.URL, "http://")

	log := fakeCredentialHelper(t, binDir, "empty",
		`printf 'credentials not found in native keychain\n'; exit 1`)
	writeDockerConfig(t, map[string]any{
		"credHelpers": map[string]any{host: "empty"},
	})

	client, err := oci.NewClient(reg.repoRef(), true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.EnumerateTagRefs(context.Background())
	if err == nil {
		t.Fatal("the registry demands a credential; an anonymous request should have been rejected")
	}
	if !oci.IsAuthError(err) {
		t.Fatalf("expected an authentication failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "anonymously") {
		t.Errorf("the request went out anonymously and the error should say so, got: %v", err)
	}
	if len(askedFor(t, log)) == 0 {
		t.Error("the credential helper was never run")
	}
}

// TestAuthCredHelperFailureIsSurfaced: a helper that is configured and then
// breaks -- exits with an error, or prints something that is not a credential
// -- must be reported as what it is. The user configured a helper; telling
// them the request "was made anonymously" and to run `docker login` sends them
// to fix the wrong thing.
func TestAuthCredHelperFailureIsSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"exits non-zero", `printf 'keychain is locked\n' >&2; exit 1`},
		{"prints malformed JSON", `printf 'not a credential\n'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := helperEnv(t)
			reg := newAuthRegistry(t, "bob", "hunter2")
			host := strings.TrimPrefix(reg.server.URL, "http://")

			log := fakeCredentialHelper(t, binDir, "broken", tc.script)
			writeDockerConfig(t, map[string]any{
				"credHelpers": map[string]any{host: "broken"},
			})

			client, err := oci.NewClient(reg.repoRef(), true)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = client.EnumerateTagRefs(context.Background())
			if err == nil {
				t.Fatal("a broken credential helper should not produce a successful anonymous session")
			}
			if len(askedFor(t, log)) == 0 {
				t.Fatal("the credential helper was never run")
			}
			if strings.Contains(err.Error(), "anonymously") {
				t.Errorf("the failure was reported as an anonymous request, hiding the broken helper: %v", err)
			}
			if !strings.Contains(err.Error(), "docker-credential-broken") && !strings.Contains(err.Error(), "credential helper") {
				t.Errorf("the error should name the credential helper that failed, got: %v", err)
			}
		})
	}
}
