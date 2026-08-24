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
	// wantErr is a substring of the refusal, required whenever want is ERROR.
	//
	// Asserting only that an error happened would let every refusal collapse
	// into one message and the suite would stay green, which matters more here
	// than usual: 34 of these cases are refusals, and the reasons are the
	// behaviour. Several are load-bearing scar tissue, like refusing a core
	// whose final exists off this branch.
	wantErr string
	tags    []string
	env     []string
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
//
// Runs against the fake repository rather than real git: it makes the train
// model assertable, removes any dependence on the developer's git config, and
// keeps the loop fast. git_test.go is what stops the fake drifting from git.
func TestResolve(t *testing.T) {
	t.Parallel()

	for _, tc := range resolveCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newFakeRepo(tc.tags)

			result, err := Resolve(repo, tc.request())

			if tc.want == "ERROR" {
				if err == nil {
					t.Fatalf("Resolve: want an error, got tag=%s", result.Tag)
				}

				if tc.wantErr == "" {
					t.Fatalf("case has no wantErr; every refusal must name its reason (got: %v)", err)
				}

				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
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
				want := resolveTagSpec(t, repo, tc.want)
				if result.Base != want {
					t.Errorf("base = %q, want %q (%s)", result.Base, want, tc.want)
				}
			}
		})
	}
}

// TestEveryRefusalNamesADistinctReason is the guard against this suite quietly
// degrading back into "an error happened".
//
// 34 of the 76 cases are refusals. If the implementation ever collapsed them
// into one generic message, each individual case would still pass its substring
// check only if that substring were shared, so the property worth pinning is
// the number of DISTINCT reasons rather than any one of them.
func TestEveryRefusalNamesADistinctReason(t *testing.T) {
	t.Parallel()

	reasons := map[string]int{}
	refusals := 0

	for _, tc := range resolveCases {
		if tc.want != "ERROR" {
			continue
		}

		refusals++

		if tc.wantErr == "" {
			t.Errorf("case %q is a refusal with no wantErr", tc.name)

			continue
		}

		reasons[tc.wantErr]++
	}

	// Chosen from the reasons the resolver actually distinguishes today. It is
	// a floor, not a target: raise it when a refusal gains its own message,
	// and be suspicious of anything that lowers it.
	const minimumDistinct = 15

	if len(reasons) < minimumDistinct {
		t.Errorf("refusals name %d distinct reasons across %d cases, want at least %d",
			len(reasons), refusals, minimumDistinct)
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
