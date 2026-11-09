// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolationtest

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Golden compares got with testdata/<name> in the calling package's directory.
// Running the package's tests with -update rewrites the file first.
func Golden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}
