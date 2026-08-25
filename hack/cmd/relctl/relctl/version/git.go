// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GitRepo answers resolution questions from a real clone.
type GitRepo struct {
	ctx context.Context
	dir string
}

// NewGitRepo binds a resolver to a working tree. An empty dir uses the process
// working directory.
func NewGitRepo(ctx context.Context, dir string) *GitRepo {
	return &GitRepo{ctx: ctx, dir: dir}
}

// Dir returns the working tree this reads from.
func (g *GitRepo) Dir() string { return g.dir }

func (g *GitRepo) run(args ...string) (string, error) {
	cmd := exec.CommandContext(g.ctx, "git", args...) //nolint:gosec // fixed binary, args built here
	cmd.Dir = g.dir

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

// lines splits command output, dropping the empty trailing element.
func lines(out string) []string {
	if out == "" {
		return nil
	}

	return strings.Split(out, "\n")
}

// ReachableTags lists tags matching a glob that are ancestors of HEAD.
//
// A tag whose commit later leaves the branch's history stops being seen. That
// fails safe, since the resolver would recompute a version whose tag already
// exists and refuse it, and it is the correct reading anyway: a tag on a commit
// that is no longer on the branch is not part of its history.
//
// Errors are returned, not swallowed. The shell this replaces ended the same
// query with `|| true`, which looks like it is absorbing "no tags match" - but
// `git tag --merged HEAD --list <glob>` exits 0 with empty output when nothing
// matches, and 128 only for an unborn HEAD, a missing repository or a canceled
// context. So there is no legitimate absence to absorb, and swallowing here
// would turn a real failure into "no tags", which is a floor of v0.0.0 that the
// resolver would then compute a version from.
func (g *GitRepo) ReachableTags(pattern string) ([]string, error) {
	out, err := g.run("tag", "--merged", "HEAD", "--list", pattern)
	if err != nil {
		return nil, err
	}

	return lines(out), nil
}

// AllTags lists tags matching a glob anywhere in the repository.
//
// Errors are returned for the same reason as ReachableTags, and it matters more
// here: this feeds the Latest decision, where an empty result means "nothing
// outranks this release". `git tag --list <glob>` exits 0 even in a repository
// with no commits at all, so an error from it is always a real failure.
func (g *GitRepo) AllTags(pattern string) ([]string, error) {
	out, err := g.run("tag", "--list", pattern)
	if err != nil {
		return nil, err
	}

	return lines(out), nil
}

// TagExists reports whether a tag exists anywhere in the repository.
//
// rev-parse exits non-zero for a tag that does not exist, which is the answer
// rather than a failure, so the distinction is made on the message: anything
// that is not a resolution failure is reported as an error. Reporting a genuine
// git failure as "the tag does not exist" would let validate fall through its
// duplicate-tag guard.
func (g *GitRepo) TagExists(tag string) (bool, error) {
	_, err := g.run("rev-parse", "-q", "--verify", "refs/tags/"+tag)
	if err == nil {
		return true, nil
	}

	// `rev-parse -q` is silent about an unknown ref and exits 1. A real failure
	// exits 128: not a repository, a broken object store, a canceled context.
	// errors.As walks the chain, so run()'s wrapping is transparent here.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}

	return false, err
}

// Head returns the commit HEAD points at.
func (g *GitRepo) Head() (string, error) {
	return g.run("rev-parse", "HEAD")
}

// CommitOf resolves a tag to its commit.
func (g *GitRepo) CommitOf(tag string) (string, error) {
	return g.run("rev-list", "-n1", tag)
}

// IsAncestor reports whether commit is an ancestor of HEAD.
//
// `merge-base --is-ancestor` documents the same contract as `rev-parse -q
// --verify` above: "exit with status 0 if true, or with status 1 if not.
// Errors are signaled by a non-zero status that is not 1." So exit 1 is an
// answer and anything else is a failure to answer.
//
// The distinction is not cosmetic, because the two callers fail in opposite
// directions. In the resolver a swallowed error becomes "not an ancestor",
// which refuses to tag and is safe. In Classify it becomes FromMain=false,
// and release-upgrade publishes a release whose from_main is not 'true' with
// deploy, Orca and smoke all skipped. A broken git command would ship an
// unsoaked release, which is what the workflow's own notice describes as
// publishing without a soak.
func (g *GitRepo) IsAncestor(commit string) (bool, error) {
	_, err := g.run("merge-base", "--is-ancestor", commit, "HEAD")
	if err == nil {
		return true, nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}

	return false, err
}

// CountCommits counts commits in from..HEAD.
func (g *GitRepo) CountCommits(from string) (int, error) {
	out, err := g.run("rev-list", "--count", from+"..HEAD")
	if err != nil {
		return 0, err
	}

	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("commit count %q: %w", out, err)
	}

	return n, nil
}

// Subject returns a commit's subject line.
func (g *GitRepo) Subject(commit string) (string, error) {
	return g.run("log", "-1", "--format=%s", commit)
}

// CurrentBranch returns the branch HEAD is on, or empty when detached.
//
// Used to default relctl's --branch, so the bare command answers about the
// branch you are actually on. A detached HEAD reports empty rather than
// guessing: it is what a CI checkout of a tag looks like, and there is no
// honest answer.
func (g *GitRepo) CurrentBranch() (string, error) {
	out, err := g.run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}

	if out == "HEAD" {
		return "", nil
	}

	return out, nil
}
