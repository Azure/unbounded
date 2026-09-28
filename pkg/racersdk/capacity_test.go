// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEffectiveCapacityStats(t *testing.T) {
	cache, err := ParseCacheName("capacity")
	require.NoError(t, err)
	c, err := NewClient(ClientConfig{Cache: cache})
	require.NoError(t, err)

	defer c.Close()

	s := c.Stats()
	require.Equal(t, 64, s.BulkLimit)
	require.Equal(t, 4, s.MetadataLimit)
	require.Equal(t, 4, s.SmallObjectLimit)
	require.Equal(t, 128, s.BulkQueueLimit)
	require.Equal(t, 16, s.MetadataQueueLimit)
	require.Equal(t, 128, s.SmallObjectQueueLimit)
}
