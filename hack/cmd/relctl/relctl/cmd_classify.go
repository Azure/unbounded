// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/hack/cmd/relctl/relctl/version"
)

// classifyResult is what happens to a release once it has been built.
type classifyResult struct {
	Tag string `json:"tag"`
	// FromMain says whether this soaks on unbounded-stable.
	FromMain bool `json:"fromMain"`
	// Latest says whether the GitHub release is marked Latest.
	Latest bool `json:"latest"`
	// Reasoning is the human-facing explanation, in order.
	Reasoning []string `json:"reasoning,omitempty"`
}

func classifyCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "classify <tag>",
		Short: "Say whether a release soaks, and whether it is Latest",
		Long: `Say whether a release soaks, and whether it is Latest.

Two questions, and they are not the same question.

  from_main  Whether this soaks on unbounded-stable. That cluster soaks main,
             and only main: a release cut from a release-X.Y branch would
             replace whatever main last deployed. Deliberately a question about
             PROVENANCE rather than version ordering, because the cluster can be
             running a candidate newer than the newest final, and an ordering
             test would then say deploy when the honest answer is that this
             release does not belong there at all.

  latest     Whether the GitHub release is marked Latest, which is what
             releases/latest/download resolves to and therefore the install
             command in README.md and every guide.

Answered against the local clone, so it needs a checkout of the DEFAULT BRANCH
with full history and tags, and no GitHub credential. release-upgrade calls this
to turn the answers into job conditions; run it by hand to see what it would
decide for a tag before cutting one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClassify(cmd, opts, args[0])
		},
	}

	return cmd
}

func runClassify(cmd *cobra.Command, opts *Options, tag string) error {
	if err := opts.validateOutput(); err != nil {
		return err
	}

	repo := opts.repo(cmd.Context())

	if err := requireDefaultBranch(cmd, repo); err != nil {
		return err
	}

	result, err := version.Classify(repo, tag)
	if err != nil {
		return err
	}

	answer := classifyResult{
		Tag:       tag,
		FromMain:  result.FromMain,
		Latest:    result.Latest,
		Reasoning: result.Report,
	}

	out := cmd.OutOrStdout()

	switch opts.Output {
	case OutputJSON:
		return writeJSON(out, answer)

	case OutputGitHub:
		// The same two keys classify-release.sh emitted, so the workflow steps
		// reading them do not change. The reasoning goes to stderr, where it
		// annotates the run without landing in $GITHUB_OUTPUT.
		for _, line := range answer.Reasoning {
			fmt.Fprintln(cmd.ErrOrStderr(), line) //nolint:errcheck // diagnostic
		}

		_, err := fmt.Fprintf(out, "from_main=%t\nlatest=%t\n", answer.FromMain, answer.Latest)

		return err

	default:
		return writeClassifyText(out, answer)
	}
}

func writeClassifyText(out io.Writer, answer classifyResult) error {
	if err := table(out, [][]string{
		{"Tag:", answer.Tag},
		{"Soaks:", yesNo(answer.FromMain)},
		{"Latest:", yesNo(answer.Latest)},
	}); err != nil {
		return err
	}

	for _, line := range answer.Reasoning {
		if _, err := fmt.Fprintf(out, "\n%s\n", line); err != nil {
			return err
		}
	}

	return nil
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}

	return "no"
}

// defaultBranch is the branch classify's answers are relative to.
//
// A constant rather than a lookup: classify is deliberately pure git and must
// work with no GitHub credential, which is what lets release-upgrade call it
// from a checkout. If this repository's default branch ever changes, this is
// the one place to change.
const defaultBranch = "main"

// requireDefaultBranch refuses to classify from the wrong checkout.
//
// Classify asks whether a tag is reachable from HEAD, so its answers are only
// meaningful when HEAD is the trunk. Run from release-0.3, a v0.3.1 tag IS
// reachable, so FromMain comes back true for a release that was not cut from
// main, and Latest is computed against that branch's trunk instead of main's.
// Both wrong, and neither can fail: there is nothing to be inconsistent with.
//
// A refusal rather than a warning, which is where this differs from next.
// next takes an explicit --branch, so a mismatch means policy and history
// disagree and it says which one it used; and its answer is still guarded,
// because a version computed from the wrong history either falls outside the
// requested series or collides with an existing tag, and it refuses. classify
// has neither a flag nor a guard. There is no reading of "reachable from HEAD"
// that is merely different rather than false, and --output json and github put
// the answer on stdout where a warning on stderr would never be seen.
//
// A detached HEAD warns instead. CurrentBranch reports empty rather than
// guessing, and refusing on "cannot tell" would break a legitimate checkout of
// a commit for the sake of a case we cannot actually detect.
func requireDefaultBranch(cmd *cobra.Command, repo *version.GitRepo) error {
	current, err := repo.CurrentBranch()
	if err != nil {
		return err
	}

	switch current {
	case defaultBranch:
		return nil

	case "":
		warn(cmd.ErrOrStderr(),
			"no branch is checked out, so it cannot be confirmed that this is %s; "+
				"the answers are only correct from there", defaultBranch)

		return nil

	default:
		return fmt.Errorf(
			"classify needs a %s checkout and %s is checked out: a tag cut from %s "+
				"is reachable from here, so from_main and latest would both be wrong; "+
				"check %s out, or pass --repo-path pointing at a %s clone",
			defaultBranch, current, current, defaultBranch, defaultBranch)
	}
}
