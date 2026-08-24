// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"os/exec"
	"strings"
	"testing"
)

// Tests for Resolve, ported from hack/release/next-version-test.sh.
//
// Each case builds a throwaway git repository containing only tags and resolves
// against it. Tags are the only input the resolver has, so a synthetic tag set
// is a complete fixture.
//
// This exists because the logic it covers mints version tags. Before it was
// extracted from the workflow the only way to test a change was to push a real
// tag to the real repository and see what happened.

// caseKind says what a case asserts.
type caseKind int

const (
	// wantTag asserts the computed tag, or ERROR.
	wantTag caseKind = iota
	// wantBase asserts which commit is tagged, named by a tag or HEAD. This is
	// what says the version being minted points at the tree that was soaked
	// rather than at whatever has landed since.
	wantBase
)

// resolveCase is one fixture plus what it should produce.
type resolveCase struct {
	kind caseKind
	name string
	mode string
	want string
	tags []string
	env  []string
}

// request turns a case's env assignments into a Request.
func (c resolveCase) request() Request {
	// patch is the shell's default for BUMP, and cases rely on it.
	req := Request{Mode: Mode(c.mode), Bump: BumpPatch}

	for _, assignment := range c.env {
		key, value, _ := strings.Cut(assignment, "=")

		switch key {
		case "BUMP":
			req.Bump = Bump(value)
		case "SERIES":
			req.Series = value
		case "PRE":
			req.Pre = value
		case "VERSION":
			req.Version = value
		case "ALLOW_CONCURRENT_TRAINS":
			req.AllowConcurrentTrains = value == "true"
		}
	}

	return req
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

	dir := t.TempDir()

	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")

	branch := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

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

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...) //nolint:gosec // fixed binary, test-controlled args
	cmd.Dir = dir

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

// TestResolve is the coverage. It has to keep protecting this code once the
// shell oracle is gone, which is why every case is asserted here directly
// rather than only through the differential run.
func TestResolve(t *testing.T) {
	requireGit(t)
	t.Parallel()

	for _, tc := range resolveCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := fixture(t, tc.tags)
			repo := NewGitRepo(t.Context(), dir)

			result, err := Resolve(repo, tc.request())

			if tc.want == "ERROR" {
				if err == nil {
					t.Fatalf("Resolve: want an error, got tag=%s", result.Tag)
				}

				return
			}

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			switch tc.kind {
			case wantTag:
				if result.Tag != tc.want {
					t.Errorf("tag = %q, want %q", result.Tag, tc.want)
				}

			case wantBase:
				want := git(t, dir, "rev-parse", tc.want+"^{commit}")
				if result.Base != want {
					t.Errorf("base = %q, want %q (%s)", result.Base, want, tc.want)
				}
			}
		})
	}
}

// TestGreaterFinal covers the comparison directly, including the refusal that
// keeps a prerelease from being compared as though version-sort order were
// semver precedence. Ported from expect_version_gt.
func TestGreaterFinal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		a, b    string
		want    bool
		wantErr bool
	}{
		{name: "finals compare normally", a: "v0.3.0", b: "v0.2.9", want: true},
		{name: "finals order numerically", a: "v0.10.0", b: "v0.9.0", want: true},
		{name: "equal finals are not greater", a: "v0.3.0", b: "v0.3.0", want: false},
		{name: "prerelease on the left refused", a: "v1.0.0-rc.1", b: "v1.0.0", wantErr: true},
		{name: "prerelease on the right refused", a: "v1.0.0", b: "v1.0.0-rc.1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := greaterFinal(tc.a, tc.b)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("greaterFinal(%q, %q) = %v, want an error", tc.a, tc.b, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("greaterFinal(%q, %q): %v", tc.a, tc.b, err)
			}

			if got != tc.want {
				t.Errorf("greaterFinal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
