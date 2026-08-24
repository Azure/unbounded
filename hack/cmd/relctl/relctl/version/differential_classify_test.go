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
	"sync"
	"testing"
)

// Differential test against classify-release.sh.
//
// TEMPORARY, and deleted with the shell. classify_test.go is the coverage.
//
// The shell delegates its ordering question to hack/cmd/semver, which this port
// absorbs, so this proves both at once.

//nolint:gochecknoglobals // built once per test binary, guarded by semverOnce
var (
	semverOnce sync.Once
	semverPath string
	semverErr  error
)

// semverBinary builds hack/cmd/semver once and returns its path.
//
// The script defaults SEMVER to `go run ./hack/cmd/semver`, which cannot work
// here: it runs with the fixture as its working directory, and that is a git
// repository but not a Go module, so go run has no module context. Building
// ahead of time also keeps the compile out of every subtest.
func semverBinary(t *testing.T) string {
	t.Helper()

	semverOnce.Do(func() {
		dir, err := os.MkdirTemp("", "relctl-semver")
		if err != nil {
			semverErr = err

			return
		}

		semverPath = filepath.Join(dir, "semver")

		out, err := exec.Command("go", "build", "-o", semverPath,
			"github.com/Azure/unbounded/hack/cmd/semver").CombinedOutput()
		if err != nil {
			semverErr = fmt.Errorf("build semver: %w: %s", err, out)
		}
	})

	if semverErr != nil {
		t.Fatalf("%v", semverErr)
	}

	return semverPath
}

// shellClassify runs classify-release.sh against a fixture.
func shellClassify(t *testing.T, dir, tag string) (fromMain, latest string, ok bool) {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "release", "classify-release.sh"))
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	if _, err := os.Stat(script); err != nil {
		t.Skipf("%s not present; the shell oracle has been removed", script)
	}

	args := []string{script}
	if tag != "" {
		args = append(args, tag)
	}

	cmd := exec.Command("bash", args...) //nolint:gosec // fixed script path
	cmd.Dir = dir

	// Fully specified, for the same reason as shellEnv: the script reads SEMVER
	// with a `:-` default, and an ambient value would point the oracle at a
	// different comparator than the one it is meant to be using.
	//
	// SEMVER points at a prebuilt binary rather than the script's own
	// `go run ./hack/cmd/semver` default, which resolves relative to the
	// working directory and would look for a module inside the fixture.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"SEMVER=" + semverBinary(t),
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", "", false
	}

	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		switch key {
		case "from_main":
			fromMain = value
		case "latest":
			latest = value
		}
	}

	return fromMain, latest, true
}

// TestClassifyMatchesTheShell is the equivalence proof for classification.
func TestClassifyMatchesTheShell(t *testing.T) {
	requireGit(t)

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	t.Parallel()

	// Topologies chosen for the two questions and how they differ: a release
	// branch newer than main, one older, a backfill, a stray final, and
	// prereleases on both sides.
	fixtures := []struct {
		name    string
		fixture classifyFixture
		tags    []string
	}{
		{
			name:    "main only",
			fixture: classifyFixture{mainTags: []string{"v0.4.0", "v0.5.0"}},
			tags:    []string{"v0.4.0", "v0.5.0"},
		},
		{
			name: "release branch ahead of main",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v0.4.1"},
			},
			tags: []string{"v0.4.0", "v0.4.1"},
		},
		{
			name: "release branch behind main",
			fixture: classifyFixture{
				mainTags:      []string{"v0.4.0"},
				branchFrom:    "v0.4.0",
				branchTags:    []string{"v0.4.1"},
				extraMainTags: []string{"v0.5.0"},
			},
			tags: []string{"v0.4.0", "v0.4.1", "v0.5.0"},
		},
		{
			name: "backfill on the branch",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v0.4.1", "v0.4.2"},
			},
			tags: []string{"v0.4.0", "v0.4.1", "v0.4.2"},
		},
		{
			name: "stray final off-branch",
			fixture: classifyFixture{
				mainTags:   []string{"v0.4.0"},
				branchFrom: "v0.4.0",
				branchTags: []string{"v9.0.0"},
			},
			tags: []string{"v0.4.0", "v9.0.0"},
		},
		{
			name:    "candidates on main",
			fixture: classifyFixture{mainTags: []string{"v0.4.0", "v0.5.0-rc.1", "v0.5.0-rc.2"}},
			tags:    []string{"v0.4.0", "v0.5.0-rc.1", "v0.5.0-rc.2"},
		},
		{
			name:    "legacy suffixes",
			fixture: classifyFixture{mainTags: []string{"v0.4.0", "v0.5.0-alpha.1", "v0.5.0-beta.2"}},
			tags:    []string{"v0.5.0-alpha.1", "v0.5.0-beta.2"},
		},
		{
			name:    "nothing final yet",
			fixture: classifyFixture{mainTags: []string{"v0.1.0-rc.1"}},
			tags:    []string{"v0.1.0-rc.1"},
		},
	}

	for _, f := range fixtures {
		for _, tag := range f.tags {
			t.Run(f.name+"/"+tag, func(t *testing.T) {
				t.Parallel()

				dir := f.fixture.build(t)

				wantFromMain, wantLatest, wantOK := shellClassify(t, dir, tag)

				got, err := Classify(NewGitRepo(t.Context(), dir), tag)
				gotOK := err == nil

				if gotOK != wantOK {
					t.Fatalf("ok = %v, shell ok = %v (err: %v)", gotOK, wantOK, err)
				}

				if !wantOK {
					return
				}

				if boolString(got.FromMain) != wantFromMain {
					t.Errorf("from_main = %v, shell = %s", got.FromMain, wantFromMain)
				}

				if boolString(got.Latest) != wantLatest {
					t.Errorf("latest = %v, shell = %s", got.Latest, wantLatest)
				}
			})
		}
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}

	return "false"
}
