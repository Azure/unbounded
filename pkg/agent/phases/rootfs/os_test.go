// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureOS_APTSourcesUsesMirrorOverride(t *testing.T) {
	t.Parallel()

	task := &configureOS{
		goalState: &goalstates.RootFS{
			HostArch: "amd64",
			PackageSources: &goalstates.PackageSources{
				APT: &goalstates.APTPackageSource{
					MirrorURL: "http://mirror.internal/ubuntu/",
				},
			},
		},
	}

	got, err := task.aptSources()
	require.NoError(t, err)
	require.Equal(t, `deb http://mirror.internal/ubuntu noble main restricted universe multiverse
deb http://mirror.internal/ubuntu noble-updates main restricted universe multiverse
deb http://mirror.internal/ubuntu noble-backports main restricted universe multiverse
deb http://mirror.internal/ubuntu noble-security main restricted universe multiverse
`, string(got))
}

func TestConfigureOS_APTSourcesRejectsUnsupportedArchWithoutOverride(t *testing.T) {
	t.Parallel()

	task := &configureOS{goalState: &goalstates.RootFS{HostArch: "ppc64le"}}

	_, err := task.aptSources()
	require.ErrorContains(t, err, "unsupported architecture")
}
