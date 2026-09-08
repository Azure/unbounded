// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The one git fixture builder these tests use.
//
// There were three copies, none of which neutralized the ambient git
// configuration. hack/release/next_version_test.go already solved this for the
// shell suite and said why; the Go port dropped the guard and reintroduced the
// problem it describes:
//
//	The fixtures create throwaway repositories and tag them, so the ambient
//	git configuration has to be neutralized: a maintainer with
//	tag.gpgSign=true, or a commit template, or a hook path, would otherwise
//	fail every case for reasons that have nothing to do with the resolver.
//
// Verified rather than assumed. With commit.gpgsign=true set globally, the
// suites failed with "fatal: failed to write commit object" - 76 cases plus
// hundreds of differential comparisons going red at once, with no obvious
// cause.

// gitEnv is the environment every fixture command runs under.
//
// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM point at /dev/null so nothing in
// ~/.gitconfig or /etc/gitconfig applies. The identity variables are set here
// rather than by `git config` so the repository needs no configuration at all,
// and so a template or hook path cannot be inherited before the first commit.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		// A stray template directory would seed hooks into every fixture.
		"GIT_TEMPLATE_DIR=",
	)
}

// git runs one git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...) //nolint:gosec // fixed binary, test-controlled args
	cmd.Dir = dir
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// initRepo creates an empty repository with one base commit.
func initRepo(t *testing.T) (dir, branch string) {
	t.Helper()

	dir = t.TempDir()

	// -b main explicitly: with the global config neutralized, init.defaultBranch
	// is unset and git falls back to master. The fixtures should look like the
	// repository they stand in for, and a mismatch shows up as spurious
	// branch-policy warnings rather than as anything obvious.
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")

	return dir, git(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// fixture creates a repository whose tags are exactly those given.
//
// A bare tag is placed on the current commit. `<tag>@new` creates a fresh
// commit first, so a fixture can express a train whose candidates are behind
// HEAD, which is the shape promote has to get right. `<tag>@off` places the tag
// on a commit that is NOT an ancestor of HEAD, as a tag cut on someone else's
// branch would be.
func fixture(t *testing.T, tags []string) string {
	t.Helper()

	dir, branch := initRepo(t)

	for _, tag := range tags {
		switch {
		case strings.HasSuffix(tag, "@new"):
			tag = strings.TrimSuffix(tag, "@new")
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "work before "+tag)

		case strings.HasSuffix(tag, "@off"):
			tag = strings.TrimSuffix(tag, "@off")
			git(t, dir, "checkout", "-q", "-b", "side-"+tag)
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "off-branch work for "+tag)
			git(t, dir, "tag", tag)
			git(t, dir, "checkout", "-q", branch)

			continue
		}

		git(t, dir, "tag", tag)
	}

	return dir
}

// classifyFixture builds a repository with main and an optional side branch,
// for the questions Classify asks about provenance.
type classifyFixture struct {
	// mainTags are tagged on main, in order, each on its own commit.
	mainTags []string
	// branchTags are tagged on a branch cut from branchFrom, in order.
	branchTags []string
	// branchFrom is the tag the side branch is cut from.
	branchFrom string
	// extraMainTags are tagged on main AFTER the branch was cut.
	extraMainTags []string
}

func (f classifyFixture) build(t *testing.T) string {
	t.Helper()

	dir, main := initRepo(t)

	for _, tag := range f.mainTags {
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "work for "+tag)
		git(t, dir, "tag", tag)
	}

	if f.branchFrom != "" {
		git(t, dir, "checkout", "-q", "-b", "side", f.branchFrom)

		for _, tag := range f.branchTags {
			git(t, dir, "commit", "-q", "--allow-empty", "-m", "fix for "+tag)
			git(t, dir, "tag", tag)
		}

		git(t, dir, "checkout", "-q", main)
	}

	for _, tag := range f.extraMainTags {
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "work for "+tag)
		git(t, dir, "tag", tag)
	}

	return dir
}
