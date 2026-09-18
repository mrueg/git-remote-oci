package oci_test

import (
	"bytes"
	"context"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// pushCommitImage publishes a small in-memory packfile.
//
// This was oci.PushCommitImage: exported production API whose only callers were
// these tests. Keeping it as a test helper takes it out of the package's public
// surface without changing what the tests do, since it is only a buffered
// PushCommitStream.
func pushCommitImage(ctx context.Context, client *oci.Client, commitSHA, refName, refTag string, data []byte) error {
	return client.PushCommitStream(ctx, oci.CommitPush{
		CommitSHA: commitSHA,
		RefName:   refName,
		RefTag:    refTag,
	}, bytes.NewReader(data), int64(len(data)))
}

// pushRefIndex publishes a plain ref -> commit mapping.
//
// This was oci.PushRefIndex, production API whose only callers were these
// tests; the helper never had a use for an index without metadata.
func pushRefIndex(ctx context.Context, client *oci.Client, refs map[string]string) error {
	rich := make(map[string]oci.RefEntry, len(refs))
	for refName, sha := range refs {
		rich[refName] = oci.RefEntry{SHA: sha}
	}
	return client.PushRichRefIndex(ctx, rich, nil)
}
