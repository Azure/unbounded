// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ociartifact

import (
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/ociutil"
)

func TestIsIndexMediaType(t *testing.T) {
	t.Parallel()

	require.True(t, isIndexMediaType(ocispec.MediaTypeImageIndex))
	require.True(t, isIndexMediaType(ociutil.DockerMediaTypeManifestList))
	require.False(t, isIndexMediaType(ocispec.MediaTypeImageManifest))
}
