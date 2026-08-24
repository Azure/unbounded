// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

import (
	"bytes"
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
// and trains_test.go are, because they have to keep protecting this code once
// the oracle is gone.
//
// Both implementations run against the SAME fixture and must agree on the tag,
// the base commit, the latest final, the live and stale trains, and whether
// they refused at all. Refusing for different reasons is fine; refusing in
// different cases is not.

// shellOutcome is everything the shell resolver says about a fixture.
type shellOutcome struct {
	tag         string
	base        string
	latestFinal string
	live        []string
	stale       []string
	ok          bool
}

// shellEnv builds a FULLY SPECIFIED environment for the script.
//
// Not os.Environ(): next-version.sh reads BUMP, SERIES, PRE, VERSION and
// ALLOW_CONCURRENT_TRAINS with `:-` defaults, while the Go side is built from
// the case alone. Inheriting the caller's environment fed the two
// implementations different inputs, which made the agreement a statement about
// the developer's shell rather than about the code. Exporting SERIES=0.3 made
// 33 cases disagree.
//
// Every variable the script reads is set here, defaulted to match Request, so
// both sides see the same inputs by construction. PATH and HOME are inherited
// because git needs them.
func shellEnv(tc resolveCase) []string {
	values := map[string]string{
		"MODE":                    tc.mode,
		"BUMP":                    string(BumpPatch),
		"SERIES":                  "",
		"PRE":                     "",
		"VERSION":                 "",
		"ALLOW_CONCURRENT_TRAINS": "false",
	}

	for _, assignment := range tc.env {
		key, value, _ := strings.Cut(assignment, "=")
		values[key] = value
	}

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}

	for key, value := range values {
		env = append(env, key+"="+value)
	}

	return env
}

// reportList parses a "Live trains:  a b" style line into its entries.
func reportList(line string) []string {
	_, rest, found := strings.Cut(line, ":")
	if !found {
		return nil
	}

	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "(none)" {
		return nil
	}

	// Stale trains carry a trailing explanation in parentheses.
	if idx := strings.Index(rest, " ("); idx >= 0 {
		rest = rest[:idx]
	}

	return strings.Fields(rest)
}

// shellResolve runs next-version.sh against a fixture.
func shellResolve(t *testing.T, dir string, tc resolveCase) shellOutcome {
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
	cmd.Env = shellEnv(tc)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return shellOutcome{}
	}

	out := shellOutcome{ok: true}

	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		switch key {
		case "tag":
			out.tag = value
		case "base":
			out.base = value
		}
	}

	// The state report goes to stderr so stdout stays parseable. Comparing it
	// puts the train model itself under the proof, rather than only the tag it
	// happened to produce, and that model is what the rest of the CLI reads.
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "Latest final:"):
			_, value, _ := strings.Cut(line, ":")
			out.latestFinal = strings.TrimSpace(value)
		case strings.HasPrefix(line, "Live trains:"):
			out.live = reportList(line)
		case strings.HasPrefix(line, "Stale trains:"):
			out.stale = reportList(line)
		}
	}

	return out
}

// compareWithShell asserts the two agree on everything the shell reports.
func compareWithShell(t *testing.T, want shellOutcome, got *Result, err error) {
	t.Helper()

	gotOK := err == nil

	if gotOK != want.ok {
		t.Fatalf("ok = %v, shell ok = %v (err: %v)", gotOK, want.ok, err)
	}

	if !want.ok {
		return
	}

	if got.Tag != want.tag {
		t.Errorf("tag = %q, shell = %q", got.Tag, want.tag)
	}

	if got.Base != want.base {
		t.Errorf("base = %q, shell = %q", got.Base, want.base)
	}

	if got.LatestFinal != want.latestFinal {
		t.Errorf("LatestFinal = %q, shell = %q", got.LatestFinal, want.latestFinal)
	}

	assertSlice(t, "Live (go vs shell)", got.Live, want.live)
	assertSlice(t, "Stale (go vs shell)", got.Stale, want.stale)
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

			want := shellResolve(t, dir, tc)
			got, err := Resolve(NewGitRepo(t.Context(), dir), tc.request())

			compareWithShell(t, want, got, err)
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

					want := shellResolve(t, dir, tc)
					got, err := Resolve(NewGitRepo(t.Context(), dir), tc.request())

					compareWithShell(t, want, got, err)
				})
			}
		}
	}
}
