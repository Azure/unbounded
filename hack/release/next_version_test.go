// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNextVersionResolver runs hack/release/next-version-test.sh.
//
// The cases live in shell rather than here on purpose: they are read during an
// incident, by someone deciding whether a version is safe to cut, and running
// them by hand then must not require a Go toolchain. This wrapper exists so CI
// runs them at all. Without it the suite was executed by nothing, which for
// logic that mints version tags is barely better than having none.
func TestNextVersionResolver(t *testing.T) {
	requireBash4(t)
	requireGit(t)
	t.Parallel()

	script, err := filepath.Abs("next-version-test.sh")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	command := exec.Command("bash", script)

	// The fixtures create throwaway repositories and tag them, so the ambient
	// git configuration has to be neutralised: a maintainer with
	// tag.gpgSign=true, or a commit template, or a hook path, would otherwise
	// fail every case for reasons that have nothing to do with the resolver.
	command.Env = append(os.Environ(),
		"TMPDIR="+t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)

	output, err := command.CombinedOutput()

	// Always logged: on a failure the per-case PASS/FAIL lines are the whole
	// diagnosis, and `go test` only shows them when something went wrong.
	t.Logf("next-version-test.sh output:\n%s", output)

	if err != nil {
		t.Fatalf("next-version-test.sh failed: %v", err)
	}

	// Guards against the suite reporting success without running: a typo that
	// left every case unreachable would otherwise pass here silently.
	if !strings.Contains(string(output), "failed=0") {
		t.Errorf("expected the resolver suite to report failed=0")
	}

	if strings.Contains(string(output), "passed=0") {
		t.Errorf("the resolver suite ran no cases")
	}
}
