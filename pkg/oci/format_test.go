package oci_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// TestRefIndexDeclaresFormatVersion pins that every repository this build
// writes says what it is.
func TestRefIndexDeclaresFormatVersion(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	if err := client.PushRichRefIndex(context.Background(), map[string]oci.RefEntry{
		"refs/heads/main": {SHA: "1111111111111111111111111111111111111111"},
	}, nil); err != nil {
		t.Fatalf("PushRichRefIndex: %v", err)
	}

	raw := reg.RawManifest(t, oci.TagRefIndex)

	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal _refs manifest: %v", err)
	}
	if got := m.Annotations["io.git-remote-oci.format-version"]; got != oci.FormatVersion {
		t.Errorf("_refs declares format version %q, want %q", got, oci.FormatVersion)
	}
}

// TestUnsupportedFormatVersionIsRefused pins that a repository written in a
// layout this build does not implement is a hard stop.
//
// There is no compatibility path any more, and silently reading a layout whose
// meaning has changed is how a fetch ends up quietly missing objects. Refusing
// with an explanation is the whole point of recording the version.
func TestUnsupportedFormatVersionIsRefused(t *testing.T) {
	// Derived rather than hardcoded, so a version bump does not silently turn
	// one of these into the current version and stop testing anything.
	unsupported := []string{"0", "99", "not-a-number", ""}
	for _, v := range unsupported {
		if v == oci.FormatVersion {
			t.Fatalf("test data %q is the current format version; pick another", v)
		}
	}
	for _, version := range unsupported {
		name := version
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			reg := registrytest.New()
			ts := reg.Serve(t)

			client := registrytest.Client(t, ts)
			ctx := context.Background()
			if err := client.PushRichRefIndex(ctx, map[string]oci.RefEntry{
				"refs/heads/main": {SHA: "1111111111111111111111111111111111111111"},
			}, nil); err != nil {
				t.Fatalf("PushRichRefIndex: %v", err)
			}

			// Rewrite the declared version, and drop _index so the fallback
			// cannot mask the rejection.
			var m map[string]any
			_ = json.Unmarshal(reg.RawManifest(t, oci.TagRefIndex), &m)
			annotations, _ := m["annotations"].(map[string]any)
			if version == "" {
				delete(annotations, "io.git-remote-oci.format-version")
			} else {
				annotations["io.git-remote-oci.format-version"] = version
			}
			updated, _ := json.Marshal(m)
			reg.PutManifest(oci.TagRefIndex, updated)
			reg.DropManifest(oci.TagOCIIndex)

			client.ClearManifestCache()
			_, err := client.FetchRichRefIndex(ctx)
			if err == nil {
				t.Fatal("a repository in an unknown format was read instead of refused")
			}
			if !errors.Is(err, oci.ErrUnsupportedFormat) {
				t.Errorf("error should be ErrUnsupportedFormat, got: %v", err)
			}
			// The useful content is both versions: what the repository claims
			// and what this build implements. Without them the reader is left
			// guessing which side is out of step.
			if !strings.Contains(err.Error(), oci.FormatVersion) {
				t.Errorf("error should name the version this build implements, got: %v", err)
			}
			want := version
			if want == "" {
				want = "none"
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name the version the repository declares (%s), got: %v", want, err)
			}
		})
	}
}

// TestParsePackBasesRequiresTheAnnotation pins that the packfile dependency
// list is mandatory.
//
// Treating an absent annotation as "self-contained" is precisely the guess that
// produced clones missing objects, so absent, empty and malformed are all
// errors rather than an empty list.
func TestParsePackBasesRequiresTheAnnotation(t *testing.T) {
	base := "a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d"

	tests := []struct {
		name        string
		annotations map[string]string
		wantErr     bool
		wantBases   []string
	}{
		{name: "absent", annotations: map[string]string{}, wantErr: true},
		{name: "empty", annotations: map[string]string{"io.git-remote-oci.pack-bases": ""}, wantErr: true},
		{name: "whitespace", annotations: map[string]string{"io.git-remote-oci.pack-bases": "   "}, wantErr: true},
		{name: "malformed", annotations: map[string]string{"io.git-remote-oci.pack-bases": "not-a-sha"}, wantErr: true},
		{name: "none", annotations: map[string]string{"io.git-remote-oci.pack-bases": "none"}},
		{name: "one", annotations: map[string]string{"io.git-remote-oci.pack-bases": base}, wantBases: []string{base}},
		{name: "two", annotations: map[string]string{"io.git-remote-oci.pack-bases": base + "," + base}, wantBases: []string{base, base}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bases, err := oci.ParsePackBases(tt.annotations)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got bases %v", bases)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(bases) != len(tt.wantBases) {
				t.Fatalf("got %v, want %v", bases, tt.wantBases)
			}
			for i := range bases {
				if bases[i] != tt.wantBases[i] {
					t.Fatalf("got %v, want %v", bases, tt.wantBases)
				}
			}
		})
	}
}

// TestRefManifestHasExactlyOneTag pins that a ref lives under one name only.
//
// An earlier layout mangled ref names into tags lossily and readers had to try
// both schemes. The encoding is injective now, so there is one tag to write and
// one to read, and nothing has to guess.
func TestRefManifestHasExactlyOneTag(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	const (
		refName   = "refs/heads/feature/login"
		commitSHA = "5555555555555555555555555555555555555555"
	)
	if err := pushCommitImage(ctx, client, commitSHA, refName, true, []byte("PACK")); err != nil {
		t.Fatalf("PushCommitImage: %v", err)
	}

	var refTags []string
	for _, tag := range reg.Tags() {
		if tag == commitSHA || strings.HasPrefix(tag, "_") && (tag == oci.TagRefIndex || tag == oci.TagOCIIndex) {
			continue
		}
		if strings.HasPrefix(tag, oci.LockTagPrefix) {
			continue
		}
		refTags = append(refTags, tag)
	}

	if len(refTags) != 1 {
		t.Fatalf("expected exactly one ref tag, got %v", refTags)
	}
	if want := oci.EncodeRefTag(refName); refTags[0] != want {
		t.Errorf("ref tag is %q, want %q", refTags[0], want)
	}
}
