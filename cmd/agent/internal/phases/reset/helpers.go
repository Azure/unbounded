package reset

import (
	"context"
	"os"
	"os/exec"
)

// removeFileIfExists removes a file if it exists, ignoring errors.
func removeFileIfExists(path string) {
	_ = os.Remove(path)
}

// removeAllIfExists removes a path and all children if it exists.
func removeAllIfExists(path string) error {
	return os.RemoveAll(path)
}

// swapon returns a command factory for swapon.
func swapon() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "swapon")
	}
}
