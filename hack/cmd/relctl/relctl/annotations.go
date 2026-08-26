// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"fmt"
	"io"
	"os"
)

// Actions workflow command prefixes.
//
// The shell this replaces wrote `::warning::` and `::error::` directly, which
// is what turns a line into an annotation in the run summary. A library
// returning a Warnings slice should not embed them, so they are added back
// here, at the point of rendering, when running under Actions.
//
// Getting this wrong is silent: "cutting vX.Y.Z while <train> is still in
// flight; that train will be stranded" simply stops appearing where a human
// would have seen it, and nothing fails.
const (
	actionsWarning = "::warning::"
)

// underActions reports whether output is being read by GitHub Actions.
//
// GITHUB_ACTIONS is set to "true" by the runner for every step. Checked rather
// than assumed from the output format, because a workflow may legitimately want
// text output and still wants its annotations.
func underActions() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true"
}

// warn renders a warning, as an annotation when Actions is reading.
func warn(w io.Writer, format string, args ...any) {
	prefix := "warning: "
	if underActions() {
		prefix = actionsWarning
	}

	//nolint:errcheck // a warning is not worth failing a release over
	fmt.Fprintf(w, "%s%s\n", prefix, fmt.Sprintf(format, args...))
}
