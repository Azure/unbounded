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

	result, err := version.Classify(opts.repo(cmd.Context()), tag)
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
