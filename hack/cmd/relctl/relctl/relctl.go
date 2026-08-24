// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package relctl drives and observes the release process from a terminal.
//
// RELEASING.md says of checking whether main is releasable: "There is no single
// dashboard." That is what this exists to be. The `gh` invocations it documents
// remain correct and remain the fallback when this tool is wrong, absent, or
// asked to do something it does not cover.
package relctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
)

// Run executes the root command and exits non-zero on failure.
func Run() {
	// Cancelled on the first signal; the second is left to the default handler
	// so a wedged poll loop can still be killed. Commands that dispatch a
	// workflow honour it between the confirmation and the request, which is the
	// window where interrupting actually helps.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := Root().ExecuteContext(ctx); err != nil {
		// Reported here rather than by cobra, and on stderr, so a --output=json
		// consumer never has an error interleaved into its stdout.
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}

		os.Exit(1)
	}
}

// Options carry the flags every command shares.
type Options struct {
	Repo string
}

// Root builds the command tree.
func Root() *cobra.Command {
	opts := &Options{Repo: gh.DefaultRepo}

	cmd := &cobra.Command{
		Use:   "relctl",
		Short: "Drive and observe unbounded releases",
		Long: `Drive and observe unbounded releases.

Version resolution runs against the local clone, so 'next' and the train view
need an up-to-date checkout and work with no GitHub credential. Everything that
reads or dispatches workflows needs one, taken from GITHUB_TOKEN or from an
existing 'gh' login.`,
		// Usage on a runtime failure is noise; errors are printed by Run.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&opts.Repo, "repo", opts.Repo, "Repository, as OWNER/NAME")

	return cmd
}
