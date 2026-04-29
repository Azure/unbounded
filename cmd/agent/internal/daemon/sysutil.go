// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// writeFile writes content to filename atomically, creating parent directories
// as needed.
func writeFile(filename string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o750); err != nil {
		return err
	}

	return renameio.WriteFile(filename, content, perm)
}
