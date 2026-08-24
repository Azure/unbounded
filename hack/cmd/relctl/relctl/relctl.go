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
//
// The work happens in run so that deferred cleanup actually runs: os.Exit skips
// defers, so calling it here directly would leave the signal handler
// uninstalled on every failing invocation.
func Run() {
	os.Exit(run())
}

func run() int {
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

		return 1
	}

	return 0
}

// Options carry the flags every command shares.
type Options struct {
	// Repo is the owner/name slug commands talk to.
	Repo string
	// RepoPath is the local clone version resolution reads.
	//
	// Exists because release-prepare runs relctl built from main against a
	// RELEASE BRANCH's history: the branch supplies the history, main supplies
	// the tooling that computes against it. Getting this wrong resolves the
	// wrong series and is the sort of thing that only shows up at release time.
	RepoPath string
	// Output is text, json or github.
	Output string
	// Token supplies the GitHub credential. Nil means the ordinary lookup:
	// GITHUB_TOKEN, then an existing gh login.
	//
	// A seam rather than a flag, so tests need no ambient credential. Without
	// it the command tests passed on a machine with gh logged in and failed in
	// CI, which is the environment dependency they were written to avoid.
	Token gh.TokenSource
	// BaseURL points the GitHub client at another API root.
	//
	// Exists so command-level tests can aim status, preflight and watch at an
	// httptest server. Without it those three could only be exercised against
	// the real API, which is why they arrived untested while next, the one
	// command needing no client, did not.
	BaseURL string
}

// Root builds the command tree.
func Root() *cobra.Command {
	return newRoot(&Options{Repo: gh.DefaultRepo, Output: OutputText})
}

// newRoot builds the tree around supplied options, so tests can inject a
// credential and an API root without a flag surface for either.
func newRoot(opts *Options) *cobra.Command {
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
		// NoArgs alone does nothing on a root with no RunE: cobra skips
		// argument validation entirely for a command it cannot run, so a
		// mistyped subcommand fell through to help and exited zero. A typo in a
		// release command must not look like success.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().StringVar(&opts.Repo, "repo", opts.Repo, "Repository, as OWNER/NAME")
	cmd.PersistentFlags().StringVar(&opts.RepoPath, "repo-path", "",
		"Local clone to resolve versions against (default: the working directory)")
	cmd.PersistentFlags().StringVarP(&opts.Output, "output", "o", opts.Output,
		"Output format: text, json or github")

	// Hidden: this is a test seam, not a supported way to point relctl at a
	// GitHub Enterprise instance. Nothing else here is written for one.
	cmd.PersistentFlags().StringVar(&opts.BaseURL, "base-url", opts.BaseURL, "GitHub API root (testing)")

	if err := cmd.PersistentFlags().MarkHidden("base-url"); err != nil {
		panic(err)
	}

	cmd.AddCommand(
		statusCommand(opts),
		nextCommand(opts),
		classifyCommand(opts),
		preflightCommand(opts),
		watchCommand(opts),
		cutCommand(opts),
		rcCommand(opts),
		promoteCommand(opts),
		branchCommand(opts),
		soakCommand(opts),
		publishCommand(opts),
	)

	return cmd
}
