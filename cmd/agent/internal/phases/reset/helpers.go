package reset

import (
	"os"
)

// removeFileIfExists removes a file if it exists, ignoring errors.
func removeFileIfExists(path string) {
	os.Remove(path) //nolint:errcheck // Best-effort cleanup; file may not exist.
}

// removeAllIfExists removes a path and all children if it exists.
func removeAllIfExists(path string) error {
	return os.RemoveAll(path)
}
