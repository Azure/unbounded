// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"bytes"
	"context"
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
func (g *GitRepo) ReachableTags(pattern string) ([]string, error) {
	// A repository with no matching tags is not an error, and neither is one
	// with no commits at all, so failures here are reported as empty.
	out, err := g.run("tag", "--merged", "HEAD", "--list", pattern)
	if err != nil {
		return nil, nil //nolint:nilerr // absence is not failure; see above
	}

	return lines(out), nil
}

// TagExists reports whether a tag exists anywhere in the repository.
func (g *GitRepo) TagExists(tag string) (bool, error) {
	_, err := g.run("rev-parse", "-q", "--verify", "refs/tags/"+tag)

	return err == nil, nil
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
func (g *GitRepo) IsAncestor(commit string) (bool, error) {
	_, err := g.run("merge-base", "--is-ancestor", commit, "HEAD")

	return err == nil, nil
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
