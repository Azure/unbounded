// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"fmt"
	"io"
	"os"
	"sync"
)

const logPrefix = "unbounded-metal"

// Logger writes init messages with the same prefix as the old shell script.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

func NewLogger(out io.Writer) *Logger {
	return &Logger{out: out}
}

func NewKernelLogger() *Logger {
	f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return NewLogger(os.Stderr)
	}

	return NewLogger(f)
}

func (l *Logger) Printf(format string, args ...any) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	fmt.Fprintf(l.out, "%s: %s\n", logPrefix, fmt.Sprintf(format, args...)) //nolint:errcheck // Best-effort initrd logging.
}
