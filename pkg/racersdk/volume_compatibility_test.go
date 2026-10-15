// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVolumeConfigCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name, cache, volume string
		valid               bool
	}{
		{"cache", "blobs", "", true},
		{"volume", "", "blobs", true},
		{"matching", "blobs", "blobs", true},
		{"conflicting", "blobs", "other", false},
		{"empty", "", "", false},
		{"invalid volume", "", "../blobs", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewClient(ClientConfig{Cache: tt.cache, Volume: tt.volume})

			_, originErr := (OriginConfig{Cache: tt.cache, Volume: tt.volume}).limits()
			if !tt.valid {
				require.ErrorIs(t, err, ErrInvalidRequest)
				require.ErrorIs(t, originErr, ErrInvalidRequest)

				return
			}

			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, c.Close()) })
			require.NoError(t, originErr)
			require.Equal(t, "/run/racer/blobs/client/socket", c.path)
		})
	}
}

func TestInvalidVolumeAndCacheNameErrors(t *testing.T) {
	for _, name := range []string{"../bad", "Upper", "a..b", "-bad", "bad-", "under_score", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		t.Run(name, func(t *testing.T) {
			_, err := NewClient(ClientConfig{Volume: name})
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: volume: invalid request: invalid volume name")

			err = ServeOrigin(t.Context(), OriginConfig{Volume: name}, nil)
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: volume: invalid request: invalid volume name")

			_, err = (OriginConfig{Volume: name}).limits()
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: volume: invalid request: invalid volume name")

			_, err = NewClient(ClientConfig{Cache: name})
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")

			err = ServeOrigin(t.Context(), OriginConfig{Cache: name}, nil)
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")
		})
	}

	_, err := NewClient(ClientConfig{})
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")

	err = ServeOrigin(t.Context(), OriginConfig{}, nil)
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.EqualError(t, err, "racersdk: cache: invalid request: invalid cache name")
}
