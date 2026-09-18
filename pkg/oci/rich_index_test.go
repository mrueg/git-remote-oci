package oci_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

func TestRefEntryUnmarshalJSON(t *testing.T) {
	// A bare string was the shape an earlier layout used. It is no longer
	// accepted: the entry is an object, and anything else is a repository this
	// build does not read.
	var legacy oci.RefEntry
	if err := json.Unmarshal([]byte(`"f0d5b61268be377529d6aa5585bd30226aab8d03"`), &legacy); err == nil {
		t.Error("a bare SHA string should no longer unmarshal into a RefEntry")
	}

	// The object form
	richJSON := []byte(`{
		"sha": "f0d5b61268be377529d6aa5585bd30226aab8d03",
		"author": "Alice <alice@example.com>",
		"timestamp": 1700000000,
		"message": "Initial commit"
	}`)
	var entry2 oci.RefEntry
	if err := json.Unmarshal(richJSON, &entry2); err != nil {
		t.Fatalf("Failed to unmarshal rich JSON: %v", err)
	}
	expected := oci.RefEntry{
		SHA:       "f0d5b61268be377529d6aa5585bd30226aab8d03",
		Author:    "Alice <alice@example.com>",
		Timestamp: 1700000000,
		Message:   "Initial commit",
	}
	if !reflect.DeepEqual(entry2, expected) {
		t.Errorf("Expected %+v, got %+v", expected, entry2)
	}
}

func TestRichRefIndexPushFetch(t *testing.T) {
	mock := registrytest.New()
	server := mock.Serve(t)

	serverURL := strings.TrimPrefix(server.URL, "http://")
	client, err := oci.NewClient(serverURL+"/test-org/test-repo", true)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	richRefs := map[string]oci.RefEntry{
		"refs/heads/main": {
			SHA:       "1111111111111111111111111111111111111111",
			Author:    "Bob <bob@example.com>",
			Timestamp: 1700000100,
			Message:   "Add main feature",
		},
		"refs/tags/v1.0.0": {
			SHA:       "2222222222222222222222222222222222222222",
			Author:    "Alice <alice@example.com>",
			Timestamp: 1700000200,
			Message:   "Release v1.0.0",
		},
	}

	if err := client.PushRichRefIndex(ctx, richRefs, nil); err != nil {
		t.Fatalf("PushRichRefIndex failed: %v", err)
	}

	// Fetch via FetchRichRefIndex
	fetchedRich, err := client.FetchRichRefIndex(ctx)
	if err != nil {
		t.Fatalf("FetchRichRefIndex failed: %v", err)
	}

	if !reflect.DeepEqual(fetchedRich, richRefs) {
		t.Errorf("FetchRichRefIndex mismatch:\nExpected: %+v\nGot:      %+v", richRefs, fetchedRich)
	}

	// FetchRefIndex is the SHA-only view over the same index.
	fetchedSimple, err := client.FetchRefIndex(ctx)
	if err != nil {
		t.Fatalf("FetchRefIndex failed: %v", err)
	}

	expectedSimple := map[string]string{
		"refs/heads/main":  "1111111111111111111111111111111111111111",
		"refs/tags/v1.0.0": "2222222222222222222222222222222222222222",
	}

	if !reflect.DeepEqual(fetchedSimple, expectedSimple) {
		t.Errorf("FetchRefIndex mismatch:\nExpected: %+v\nGot:      %+v", expectedSimple, fetchedSimple)
	}
}
