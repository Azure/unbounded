// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// writeFile writes content to filename atomically, creating parent directories
// as needed. The temp file is created in the same directory as the destination to
// preserve the correct SELinux label (avoiding the user_tmp_t label from /tmp).
func writeFile(filename string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return err
	}

	return renameio.WriteFile(filename, content, perm, renameio.WithTempDir(filepath.Dir(filename)))
}
