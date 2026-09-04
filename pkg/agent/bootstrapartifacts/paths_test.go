// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrapartifacts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerImageArchivePath(t *testing.T) {
	t.Parallel()

	got := ContainerImageArchivePath("amd64", "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9")
	require.Equal(t, "container-images/amd64/mcr.microsoft.com_oss_v2_kubernetes_pause_3.9-a68ffa05fa78.tar", got)
}
