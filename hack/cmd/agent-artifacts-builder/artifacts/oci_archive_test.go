// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArchiveOCIImageRejectsDigestReference(t *testing.T) {
	t.Parallel()

	digest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	err := ArchiveOCIImage(
		context.Background(),
		"ghcr.io/azure/agent-ubuntu2404@"+digest,
		filepath.Join(t.TempDir(), "rootfs.oci.tar.gz"),
	)
	require.ErrorContains(t, err, "must use a tag")
}
