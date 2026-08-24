// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Differential test against the shell resolver being replaced.
//
// TEMPORARY. This exists to prove the port is faithful, and is deleted in the
// same change that deletes the shell. It is not the coverage: resolve_test.go
// is, because it has to keep protecting this code once the oracle is gone.
//
// Both implementations run against the SAME fixture, and must agree on the
// computed tag, the base commit, and whether they refused at all. Refusing for
// different reasons is fine; refusing in different cases is not.

// shellResolve runs next-version.sh against a fixture.
func shellResolve(t *testing.T, dir string, tc resolveCase) (tag, base string, ok bool) {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "release", "next-version.sh"))
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	if _, err := os.Stat(script); err != nil {
		t.Skipf("%s not present; the shell oracle has been removed", script)
	}

	cmd := exec.Command("bash", script) //nolint:gosec // fixed script path
	cmd.Dir = dir

	cmd.Env = append(os.Environ(), "MODE="+tc.mode)
	cmd.Env = append(cmd.Env, tc.env...)

	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		switch key {
		case "tag":
			tag = value
		case "base":
			base = value
		}
	}

	return tag, base, true
}

// TestResolveMatchesTheShell is the equivalence proof for the resolver.
func TestResolveMatchesTheShell(t *testing.T) {
	requireGit(t)

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	t.Parallel()

	for _, tc := range resolveCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := fixture(t, tc.tags)

			wantTagValue, wantBase, wantOK := shellResolve(t, dir, tc)

			result, err := Resolve(NewGitRepo(t.Context(), dir), tc.request())
			gotOK := err == nil

			if gotOK != wantOK {
				t.Fatalf("ok = %v, shell ok = %v (err: %v)", gotOK, wantOK, err)
			}

			if !wantOK {
				return
			}

			if result.Tag != wantTagValue {
				t.Errorf("tag = %q, shell = %q", result.Tag, wantTagValue)
			}

			if result.Base != wantBase {
				t.Errorf("base = %q, shell = %q", result.Base, wantBase)
			}
		})
	}
}

// TestResolveMatchesTheShellOnGeneratedTagSets widens the proof past the cases
// anyone thought to write.
//
// The hand-written fixtures encode what we expected to matter. This one walks
// combinations of finals, candidates, off-branch tags and malformed metadata
// across every mode and bump, which is where a port is most likely to differ:
// not in the shapes it was written against, but in the ones nobody considered.
func TestResolveMatchesTheShellOnGeneratedTagSets(t *testing.T) {
	requireGit(t)

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	t.Parallel()

	// Each entry is a whole repository's tag set.
	tagSets := [][]string{
		{},
		{"v0.0.0"},
		{"v1.0.0"},
		{"v0.2.4", "v0.2.5-rc.1"},
		{"v0.2.4", "v0.2.5-rc.1", "v0.2.5-rc.2"},
		{"v0.2.4", "v0.2.5-rc.9", "v0.2.5-rc.10"},
		{"v0.2.4", "v0.2.5-rc.1", "v0.3.0-rc.1"},
		{"v0.2.4", "v0.1.24-rc.1"},
		{"v0.2.4", "v0.2.5-rc.1@new"},
		{"v0.2.4", "v0.2.5-rc.1@off"},
		{"v0.2.4@off", "v0.1.0"},
		{"v0.2.4", "v0.2.5", "v0.2.5-rc.1"},
		{"v0.2.4", "v0.2.5-rc.0"},
		{"v0.2.4", "v0.2.5-rc.01"},
		{"v0.2.4", "v0.2.5-alpha.1"},
		{"v0.2.4", "v0.2.5-rc"},
		{"v0.2.4", "v0.2"},
		{"v0.2.4", "v0.2.4.1"},
		{"v0.2.4", "v01.2.3"},
		{"v0.2.4", "v1234567890.0.0"},
		{"v9.9.9", "v10.0.0"},
		{"v0.9.0", "v0.10.0"},
		{"v0.2.4", "v0.3.0-rc.1", "v0.3.0-rc.2", "v0.4.0-rc.1"},
	}

	modes := []string{"release", "prerelease", "promote"}
	bumps := []string{"patch", "minor", "major"}

	for i, tags := range tagSets {
		for _, mode := range modes {
			for _, bump := range bumps {
				name := fmt.Sprintf("set%02d/%s/%s", i, mode, bump)

				t.Run(name, func(t *testing.T) {
					t.Parallel()

					tc := resolveCase{
						mode: mode,
						tags: tags,
						env:  []string{"BUMP=" + bump},
					}

					dir := fixture(t, tags)

					wantTagValue, wantBase, wantOK := shellResolve(t, dir, tc)

					result, err := Resolve(NewGitRepo(t.Context(), dir), tc.request())
					gotOK := err == nil

					if gotOK != wantOK {
						t.Fatalf("tags=%v ok = %v, shell ok = %v (err: %v)", tags, gotOK, wantOK, err)
					}

					if !wantOK {
						return
					}

					if result.Tag != wantTagValue {
						t.Errorf("tags=%v tag = %q, shell = %q", tags, result.Tag, wantTagValue)
					}

					if result.Base != wantBase {
						t.Errorf("tags=%v base = %q, shell = %q", tags, result.Base, wantBase)
					}
				})
			}
		}
	}
}
