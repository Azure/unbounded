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

// Differential tests against the shell implementation being replaced.
//
// TEMPORARY. These exist to prove the port is faithful, and are deleted in the
// same change that deletes the shell. They are not the coverage: the ported
// table tests are, because they have to keep protecting this code once the
// oracle is gone. A differential run alone would take the coverage with it.
//
// Each case runs both implementations over identical input and asserts they
// agree on the answer AND on whether they refused at all.

// shellBumpForBranch runs the script under test, returning bump, series and
// whether it succeeded.
func shellBumpForBranch(t *testing.T, branch string, major bool) (bump, series string, ok bool) {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "release", "bump-for-branch.sh"))
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	if _, err := os.Stat(script); err != nil {
		t.Skipf("%s not present; the shell oracle has been removed", script)
	}

	args := []string{script}
	if branch != "" {
		args = append(args, branch)
	}

	command := exec.Command("bash", args...) //nolint:gosec // fixed script path

	command.Env = append(os.Environ(), fmt.Sprintf("MAJOR=%t", major))

	out, err := command.Output()
	if err != nil {
		return "", "", false
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		switch key {
		case "bump":
			bump = value
		case "series":
			series = value
		}
	}

	return bump, series, true
}

// TestForBranchMatchesTheShell is the equivalence proof for the branch policy.
//
// The corpus is deliberately wider than either implementation's own tests: it
// includes shapes nobody thought to write a case for, because the cases encode
// what we expected to matter and the point here is to catch what we did not.
func TestForBranchMatchesTheShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	branches := []string{
		"main",
		"release-0.0", "release-0.1", "release-0.2", "release-1.0",
		"release-12.34", "release-123456789.0",
		// Refusals, which must refuse for the same inputs on both sides.
		"", "release-1", "release-01.2", "release-0.2.4", "release-1.02",
		"release-1234567890.0", "feat/x", "v0.2.4", "main-2", "Main",
		"backup/release-0.2", "release-0.2-backup", "release-", "release--1.0",
		"release-0.2/", "/main", " main", "main ", "release-+1.0", "release-1.0 ",
		"HEAD", "origin/main", "release-0.2\n", "release-٠.١",
	}

	for _, branch := range branches {
		for _, major := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/major=%t", branch, major), func(t *testing.T) {
				wantBump, wantSeries, wantOK := shellBumpForBranch(t, branch, major)

				got, err := ForBranch(branch, major)
				gotOK := err == nil

				if gotOK != wantOK {
					t.Fatalf("ForBranch(%q, %v) ok = %v, shell ok = %v (err: %v)",
						branch, major, gotOK, wantOK, err)
				}

				if !wantOK {
					return
				}

				if string(got.Bump) != wantBump {
					t.Errorf("bump = %q, shell = %q", got.Bump, wantBump)
				}

				if got.Series != wantSeries {
					t.Errorf("series = %q, shell = %q", got.Series, wantSeries)
				}
			})
		}
	}
}
