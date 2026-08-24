// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// Output formats relctl can produce.
const (
	// OutputText is the human-facing default.
	OutputText = "text"
	// OutputJSON is for scripting.
	OutputJSON = "json"
	// OutputGitHub emits key=value lines for $GITHUB_OUTPUT.
	//
	// Exists so the release workflows can call relctl in place of the shell
	// they used to, without their downstream steps changing at all.
	OutputGitHub = "github"
)

// client builds a GitHub client lazily.
//
// Deliberately not built at the root: version resolution is pure git and has to
// keep working with no credential, in a clone or in a workflow that was never
// granted one. Building it up front would make `relctl next` fail for want of a
// token it never uses.
func (o *Options) client(ctx context.Context) (*gh.Client, error) {
	return gh.New(ctx, gh.Options{Repo: o.Repo, BaseURL: o.BaseURL})
}

// repo binds the resolver to the local clone.
func (o *Options) repo(ctx context.Context) *version.GitRepo {
	return version.NewGitRepo(ctx, o.RepoPath)
}

// validateOutput rejects an unknown format before any work is done.
func (o *Options) validateOutput() error {
	switch o.Output {
	case OutputText, OutputJSON, OutputGitHub:
		return nil
	default:
		return fmt.Errorf("unknown output format %q; want %s, %s or %s",
			o.Output, OutputText, OutputJSON, OutputGitHub)
	}
}

// writeJSON emits a value as indented JSON.
func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	return encoder.Encode(value)
}

// table writes aligned columns.
func table(w io.Writer, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				if _, err := fmt.Fprint(tw, "\t"); err != nil {
					return err
				}
			}

			if _, err := fmt.Fprint(tw, cell); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintln(tw); err != nil {
			return err
		}
	}

	return tw.Flush()
}
