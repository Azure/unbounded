// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
