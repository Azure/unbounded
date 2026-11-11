// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package relctl

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/gh"
	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

func draftsCommand(opts *Options) *cobra.Command {
	group := &cobra.Command{
		Use:   "drafts",
		Short: "Clean up candidate drafts",
		Long: `Clean up candidate drafts.

Candidates are never published, so each leaves a draft release. These remove the
ones that have stopped being useful. Only DRAFTS of CANDIDATES are ever touched:
a published release, and a final's draft, are refused or left alone. Tags stay
either way, because images, signatures and the Go module proxy may refer to them.

release-upgrade runs these as a candidate's soak finishes, when a build fails,
and when a final publishes. Drafts that predate candidates are left for a
maintainer to clear deliberately.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	group.AddCommand(draftsPruneCommand(opts), draftsDiscardCommand(opts))

	return group
}

func draftsPruneCommand(opts *Options) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "prune <tag>",
		Short: "Delete the drafts a finished candidate or a published final supersedes",
		Long: `Delete the drafts a finished candidate or a published final supersedes.

For a candidate, vX.Y.Z-rc.N, whose soak has just finished: deletes the drafts of
OLDER candidates of vX.Y.Z, except the newest of them that passed, when rc.N
itself did not pass. Newer candidates are left alone, since they may still be
building.

  Kept: rc.N, so its soak can be re-run once whatever failed is fixed; and the
  newest candidate that passed, since that is what promote would ship.

For a final, vX.Y.Z, that has just published: deletes every remaining draft of
its candidates.

Nothing outside that one version is touched.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrune(cmd.Context(), cmd.OutOrStdout(), opts, args[0], dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Say what would be deleted and stop")

	return cmd
}

func runPrune(ctx context.Context, out io.Writer, opts *Options, tag string, dryRun bool) error {
	client, err := opts.client(ctx)
	if err != nil {
		return err
	}

	drafts, err := client.Drafts(ctx)
	if err != nil {
		return err
	}

	passed := func(release gh.Release) (bool, error) {
		commit, err := client.CommitOf(ctx, release.Tag)
		if err != nil {
			return false, err
		}

		status, err := client.CandidateStatus(ctx, commit)
		if err != nil {
			return false, err
		}

		return status.Passed(), nil
	}

	remove, keep, err := planPrune(tag, drafts, passed)
	if err != nil {
		return err
	}

	for _, note := range keep {
		if _, err := fmt.Fprintf(out, "kept %s\n", note); err != nil {
			return err
		}
	}

	if len(remove) == 0 {
		_, err := fmt.Fprintf(out, "Nothing to prune for %s.\n", tag)

		return err
	}

	for _, release := range remove {
		if dryRun {
			if _, err := fmt.Fprintf(out, "would delete the draft of %s\n", release.Tag); err != nil {
				return err
			}

			continue
		}

		if err := client.DeleteRelease(ctx, release); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(out, "deleted the draft of %s\n", release.Tag); err != nil {
			return err
		}
	}

	return nil
}

// planPrune decides which drafts a finished candidate or a published final
// supersedes.
//
// Pure apart from passed, which says whether a candidate's soak passed, so the
// rules can be tested without an API. It is consulted lazily, newest first, and
// stops as soon as it has the answer.
func planPrune(
	tag string,
	drafts []gh.Release,
	passed func(gh.Release) (bool, error),
) (remove []gh.Release, keep []string, err error) {
	if version.IsFinal(tag) {
		for _, draft := range drafts {
			if !draft.Draft {
				continue
			}

			if core, _, ok := version.ParseCandidate(draft.Tag); ok && core == tag {
				remove = append(remove, draft)
			}
		}

		return remove, nil, nil
	}

	core, number, ok := version.ParseCandidate(tag)
	if !ok {
		return nil, nil, fmt.Errorf("%s is neither a final nor a candidate; nothing to prune for it", tag)
	}

	type older struct {
		release gh.Release
		number  int
	}

	var olders []older

	for _, draft := range drafts {
		if !draft.Draft {
			continue
		}

		draftCore, draftNumber, ok := version.ParseCandidate(draft.Tag)
		if !ok || draftCore != core || draftNumber >= number {
			continue
		}

		olders = append(olders, older{release: draft, number: draftNumber})
	}

	if len(olders) == 0 {
		return nil, nil, nil
	}

	slices.SortFunc(olders, func(a, b older) int { return cmp.Compare(b.number, a.number) })

	// When this candidate passed, it is the newest that did, and nothing older
	// is worth keeping. Otherwise keep the newest older one that passed, since
	// promote would ship it.
	self := gh.Release{Tag: tag}

	selfPassed, err := passed(self)
	if err != nil {
		return nil, nil, err
	}

	keepIndex := -1

	if !selfPassed {
		for i, o := range olders {
			ok, err := passed(o.release)
			if err != nil {
				return nil, nil, err
			}

			if ok {
				keepIndex = i

				keep = append(keep, o.release.Tag+" (the newest candidate that passed)")

				break
			}
		}
	}

	for i, o := range olders {
		if i != keepIndex {
			remove = append(remove, o.release)
		}
	}

	return remove, keep, nil
}

func draftsDiscardCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discard <candidate>",
		Short: "Delete the draft of a candidate whose build failed",
		Long: `Delete the draft of a candidate whose build failed.

A failed candidate build is not retried; the next candidate is the retry. Its
draft holds a partial set of assets that nothing will ever finish, so it goes.
The tag stays.

Refuses anything that is not a candidate's draft. A missing release is not an
error: a build can fail before the draft is opened.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tag := args[0]
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if _, _, ok := version.ParseCandidate(tag); !ok {
				return fmt.Errorf("%s is not a candidate; only a candidate's draft is discarded", tag)
			}

			client, err := opts.client(ctx)
			if err != nil {
				return err
			}

			release, err := client.Release(ctx, tag)
			if err != nil {
				return err
			}

			if release == nil {
				_, err := fmt.Fprintf(out, "No release exists for %s; nothing to discard.\n", tag)

				return err
			}

			if !release.Draft {
				return fmt.Errorf("%s is published; refusing to delete a published release", tag)
			}

			if err := client.DeleteRelease(ctx, *release); err != nil {
				return err
			}

			_, err = fmt.Fprintf(out, "deleted the draft of %s\n", tag)

			return err
		},
	}

	return cmd
}
