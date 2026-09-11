// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStageBarrierRejectsMissingOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, SyncFilesystems(dir, dir))
	require.Error(t, SyncFilesystems(filepath.Join(dir, "missing")))
	file := filepath.Join(dir, "output")
	require.NoError(t, os.WriteFile(file, []byte("stage output"), 0o600))
	require.NoError(t, SyncFilesystems(file, dir))
}
