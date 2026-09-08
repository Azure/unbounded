// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSystemdSwapUnits(t *testing.T) {
	t.Parallel()

	out := `dev-zram0.swap loaded active active Compressed Swap on /dev/zram0
dev-disk-by\x2duuid-123.swap loaded inactive dead /dev/disk/by-uuid/123
not-a-swap.service loaded active active ignored
dev-zram0.swap loaded active active duplicate`

	require.Equal(t, []string{
		`dev-disk-by\x2duuid-123.swap`,
		"dev-zram0.swap",
	}, parseSystemdSwapUnits(out))
}

func TestZramSetupUnitsForSwapUnits(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"systemd-zram-setup@zram0.service",
	}, zramSetupUnitsForSwapUnits([]string{
		`dev-disk-by\x2dlabel-zram0.swap`,
		"dev-zram0.swap",
	}))
}

func TestSystemdSwapUnitsToMask(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"dev-zram0.swap",
	}, systemdSwapUnitsToMask([]string{
		`dev-disk-by\x2dlabel-zram0.swap`,
		`dev-disk-by\x2duuid-123.swap`,
		"dev-zram0.swap",
	}))
}
