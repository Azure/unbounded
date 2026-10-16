// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheConfig(t *testing.T) {
	c, err := NewClient(ClientConfig{Cache: "blobs"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	require.Equal(t, "/run/racer/blobs/client/socket", c.path)

	_, err = (OriginConfig{Cache: "blobs"}).limits()
	require.NoError(t, err)
}

func TestInvalidCacheNameErrors(t *testing.T) {
	for _, name := range []string{"", "../bad", "Upper", "a..b", "-bad", "bad-", "under_score", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		t.Run(name, func(t *testing.T) {
			_, err := NewClient(ClientConfig{Cache: name})
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")

			err = ServeOrigin(t.Context(), OriginConfig{Cache: name}, nil)
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")

			_, err = (OriginConfig{Cache: name}).limits()
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")
		})
	}
}
