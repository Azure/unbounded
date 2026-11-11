// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLayoutRemoveOfTheActiveVersionIsErrVersionActive(t *testing.T) {
	t.Parallel()

	l := Layout{Base: filepath.Join(t.TempDir(), "node")}
	_, err := l.Ensure("v1")
	require.NoError(t, err)
	_, err = l.Flip("v1")
	require.NoError(t, err)

	err = l.Remove("v1")
	require.ErrorIs(t, err, ErrVersionActive)
	require.ErrorContains(t, err, `"v1"`)

	_, err = l.Ensure("v2")
	require.NoError(t, err)
	require.NoError(t, l.Remove("v2"), "an inactive version is removable")
}
