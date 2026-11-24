// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"encoding/json"
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

// printErr writes to stderr via fprintf; convenience shorthand.
func printErr(format string, args ...any) { fprintf(os.Stderr, format, args...) }

// emitJSONResult is the shared output dispatcher used by bench and
// scenario. It writes a human-readable summary to stdout via
// writeHuman when output == "text", encodes v as indented JSON when
// output == "json", and (independently) writes v as indented JSON
// to jsonOutPath when non-empty. Unknown output values are tolerated
// silently so the caller's flag validation owns that contract.
func emitJSONResult[T any](v T, output, jsonOutPath string, writeHuman func(io.Writer, T)) error {
	switch output {
	case "text":
		writeHuman(os.Stdout, v)
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
	}

	if jsonOutPath != "" {
		f, err := os.Create(jsonOutPath)
		if err != nil {
			return fmt.Errorf("create --json-out: %w", err)
		}

		defer f.Close() //nolint:errcheck // best-effort; encode error below is the meaningful one

		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")

		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("write --json-out: %w", err)
		}
	}

	return nil
}
