package reset

import (
	"os"
)

// removeFileIfExists removes a file if it exists, ignoring errors.
func removeFileIfExists(path string) {
	_ = os.Remove(path)
}

// removeAllIfExists removes a path and all children if it exists.
func removeAllIfExists(path string) error {
	return os.RemoveAll(path)
}
