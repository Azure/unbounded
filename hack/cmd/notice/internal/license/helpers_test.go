// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package license

import (
	"os"
	"path/filepath"
)

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
