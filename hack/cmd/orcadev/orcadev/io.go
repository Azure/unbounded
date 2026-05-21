// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"fmt"
	"io"
	"os"
)

// fprintf is a fmt.Fprintf wrapper that intentionally discards the
// returned error. Used by the human-readable output paths of every
// subcommand. Write failures to stdout / stderr in a CLI tool are
// not actionable: the process is about to exit anyway, and a
// SIGPIPE on a closed pipe (e.g. `orcadev bench | head`) is the
// expected steady-state. The wrapper exists so call sites stay
// readable without sprinkling `_, _ = fmt.Fprintf(...)` at every
// printout.
func fprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // see fprintf doc comment
}

// fprintln is the Fprintln companion to fprintf.
func fprintln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...) //nolint:errcheck // see fprintf doc comment
}

// printOut writes to stdout via fprintf; convenience shorthand for
// the most common case.
func printOut(format string, args ...any) { fprintf(os.Stdout, format, args...) }
