// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import (
	"os"
	"path/filepath"
)

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
