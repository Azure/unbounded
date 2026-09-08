// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// dispatchRunTimeout is how long to look for the run a dispatch created.
//
// Short on purpose. The dispatch endpoint returns no run ID, so finding the run
// means polling, and a queued run can take longer than any tolerable wait. Ten
// seconds catches the common case; past that the command says where to look
// rather than holding the terminal. Overridden in tests, which have no runner.
//
//nolint:gochecknoglobals // a knob for tests, not configuration
var dispatchRunTimeout = 10 * time.Second

// dispatch is one workflow dispatch, described before it is sent.
//
// Every command here prints the workflow, the ref and every input, then asks.
// The dispatch endpoint returns no run ID and does not report an input a
// workflow ignores, so what is on screen before the confirmation is the only
// chance to see what is actually being sent.
type dispatch struct {
	// Workflow is the file name to dispatch.
	Workflow string
	// Ref is the branch the workflow runs from.
	Ref string
	// Inputs are the workflow_dispatch inputs.
	Inputs map[string]any
	// Summary is what this will do, in a line.
	Summary string
	// Confirm is a phrase the operator must type exactly.
	//
	// Empty means --yes is enough. Non-empty is for the break-glass paths,
	// where the point is that it should not be possible to do this by reflex.
	Confirm string
	// Warnings are shown before the prompt.
	Warnings []string
}

// run confirms and sends a dispatch.
func (d dispatch) run(ctx context.Context, cmd *cobra.Command, opts *Options, yes, dryRun bool) error {
	out := cmd.OutOrStdout()

	if err := d.describe(out); err != nil {
		return err
	}

	if dryRun {
		_, err := fmt.Fprintln(out, "\nDry run. Nothing was dispatched.")

		return err
	}

	// Before the prompt, not after. A missing credential is worth finding out
	// about before typing a break-glass phrase, not instead of the dispatch
	// that was supposed to follow it.
	client, err := opts.client(ctx)
	if err != nil {
		return err
	}

	if err := d.confirm(cmd, yes); err != nil {
		return err
	}

	// Recorded before the dispatch, so a run created by it cannot be older.
	sentAt := time.Now()

	if err := client.Dispatch(ctx, d.Workflow, d.Ref, d.Inputs); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "\nDispatched %s on %s.\n", d.Workflow, d.Ref); err != nil {
		return err
	}

	run, err := client.FindDispatched(ctx, d.Workflow, sentAt, dispatchRunTimeout)
	if err != nil {
		return err
	}

	if run == nil {
		_, err := fmt.Fprintf(out,
			"The run has not appeared yet; it is probably queued.\n  gh run list --repo %s --workflow %s\n",
			client.Repo(), d.Workflow)

		return err
	}

	// Described as the newest matching run rather than asserted to be ours: the
	// dispatch endpoint returns no run ID, so two dispatches close together are
	// indistinguishable.
	_, err = fmt.Fprintf(out, "Newest matching run: %s\n", run.URL)

	return err
}

func (d dispatch) describe(out io.Writer) error {
	rows := [][]string{
		{"Workflow:", d.Workflow},
		{"Ref:", d.Ref},
	}

	keys := make([]string, 0, len(d.Inputs))
	for key := range d.Inputs {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		rows = append(rows, []string{"  " + key + ":", fmt.Sprint(d.Inputs[key])})
	}

	if d.Summary != "" {
		if _, err := fmt.Fprintf(out, "%s\n\n", d.Summary); err != nil {
			return err
		}
	}

	if err := table(out, rows); err != nil {
		return err
	}

	for _, warning := range d.Warnings {
		warn(out, "%s", warning)
	}

	return nil
}

// confirm asks, unless --yes covers it.
func (d dispatch) confirm(cmd *cobra.Command, yes bool) error {
	out := cmd.OutOrStdout()

	// A typed phrase is not satisfiable by --yes. The break-glass paths use it
	// precisely so that a script cannot take them by default, and so that doing
	// one requires reading what it says.
	if d.Confirm != "" {
		// Refused rather than ignored. --yes not applying is the entire point,
		// but a script that passes it everywhere would otherwise sit on a
		// prompt it cannot answer and report an EOF, which says nothing about
		// why. Fails closed either way; this one says what to do instead.
		if yes {
			return fmt.Errorf("--yes does not apply here: type %q to continue", d.Confirm)
		}

		if _, err := fmt.Fprintf(out, "\nType %q to continue: ", d.Confirm); err != nil {
			return err
		}

		reader := bufio.NewReader(cmd.InOrStdin())

		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("aborted: %w", err)
		}

		if strings.TrimSpace(line) != d.Confirm {
			return fmt.Errorf("aborted")
		}

		return nil
	}

	if yes {
		return nil
	}

	if _, err := fmt.Fprint(out, "\nContinue? [y/N] "); err != nil {
		return err
	}

	reader := bufio.NewReader(cmd.InOrStdin())

	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("aborted: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted")
	}
}

// addDispatchFlags registers the flags every dispatch command shares.
func addDispatchFlags(cmd *cobra.Command, yes, dryRun *bool) {
	cmd.Flags().BoolVar(yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().BoolVar(dryRun, "dry-run", false, "Print what would be dispatched and stop")
}

// preview resolves what a prepare dispatch will cut, for the confirmation.
//
// Shown before confirming because the workflow's own dry_run costs a dispatch
// and a minute, and the answer is computable here instantly from the same code
// release-prepare runs. It is a preview rather than a guess.
//
// A failure is not fatal. The local clone may be stale or absent while the
// workflow resolves against its own checkout perfectly well, so this reports
// what it could not do rather than refusing to dispatch.
func preview(cmd *cobra.Command, opts *Options, branch string, req version.Request, major bool) (string, []string) {
	policy, err := version.ForBranch(branch, major)
	if err != nil {
		return "", []string{"could not preview locally: " + err.Error()}
	}

	req.Bump = policy.Bump
	req.Series = policy.Series

	resolved, err := version.Resolve(opts.repo(cmd.Context()), req)
	if err != nil {
		return "", []string{"could not preview locally: " + err.Error()}
	}

	return resolved.Tag, resolved.Warnings
}
