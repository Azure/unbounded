// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsNotEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), nil, 0o600))

	err := os.Remove(dir)
	require.Error(t, err)
	assert.True(t, IsNotEmpty(err), "removing a directory with entries")
	assert.True(t, IsNotEmpty(fmt.Errorf("wrapped: %w", err)))

	assert.False(t, IsNotEmpty(nil))
	assert.False(t, IsNotEmpty(os.Remove(filepath.Join(dir, "absent"))))
	assert.False(t, IsNotEmpty(fmt.Errorf("directory not empty")), "text alone is not the error")
}
