// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for forced-publish-notes.sh.
//
// The notice it composes is the durable record of a release that skipped its
// soak: read long after the run log expires, and never exercised until an
// emergency. Two defects lived in it undetected while it was inline YAML - a
// skipped smoke matrix described as a successful soak, and a re-run appending a
// second copy - so both have a case here.

// results is one combination of upstream job outcomes.
type results struct {
	deploy   string
	orca     string
	discover string
	smoke    string
}

var allPassed = results{deploy: "success", orca: "success", discover: "success", smoke: "success"}

// composeNotes runs the script over a body and returns stdout+stderr and the
// exit code.
func composeNotes(t *testing.T, body string, r results, env map[string]string) (string, int) {
	t.Helper()

	script, err := filepath.Abs("forced-publish-notes.sh")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "body.md")

	if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	base := map[string]string{
		"PATH":            os.Getenv("PATH"),
		"ACTOR":           "someone",
		"REASON":          "stable cluster unreachable during DC maintenance",
		"DEPLOY_RESULT":   r.deploy,
		"ORCA_RESULT":     r.orca,
		"DISCOVER_RESULT": r.discover,
		"SMOKE_RESULT":    r.smoke,
	}
	for key, value := range env {
		base[key] = value
	}

	environ := make([]string, 0, len(base))
	for key, value := range base {
		environ = append(environ, key+"="+value)
	}

	command := exec.Command("bash", script, bodyFile)
	command.Env = environ

	out, err := command.CombinedOutput()

	code := 0

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run script: %v", err)
	}

	return string(out), code
}

func TestForcedNotesRecordAnUnsoakedRelease(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	output, code := composeNotes(t, "Original notes.\n",
		results{deploy: "failure", orca: "success", discover: "success", smoke: "skipped"}, nil)

	requireCode(t, code, 0, output)
	requireContains(t, output, "Original notes.")
	requireContains(t, output, "Published without a successful soak.")
	requireContains(t, output, "Forced by @someone")
	requireContains(t, output, "Reason: stable cluster unreachable during DC maintenance")
	requireContains(t, output, "<!-- unbounded:forced-publish -->")
}

// TestForcedNotesDoNotClaimASoakThatWasSkipped is the regression guard: a
// failed smoke-discover skips the matrix, and `skipped` was being treated as a
// pass. Since discovery now fails rather than emitting an empty matrix, a
// skipped smoke job means the tests never ran.
func TestForcedNotesDoNotClaimASoakThatWasSkipped(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	for _, r := range []results{
		{deploy: "success", orca: "success", discover: "failure", smoke: "skipped"},
		{deploy: "success", orca: "success", discover: "success", smoke: "skipped"},
		{deploy: "success", orca: "success", discover: "success", smoke: "failure"},
		{deploy: "success", orca: "skipped", discover: "skipped", smoke: "skipped"},
	} {
		output, code := composeNotes(t, "Notes.\n", r, nil)

		requireCode(t, code, 0, output)
		requireContains(t, output, "Published without a successful soak.")
		requireNotContains(t, output, "the soak did pass")
	}
}

// TestForcedNotesAdmitWhenTheSoakPassed covers the other direction: a forced
// dispatch lands here even when every gate was green, and claiming otherwise
// would be its own false record.
func TestForcedNotesAdmitWhenTheSoakPassed(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	output, code := composeNotes(t, "Notes.\n", allPassed, nil)

	requireCode(t, code, 0, output)
	requireContains(t, output, "though the soak did pass")
	requireNotContains(t, output, "Published without a successful soak.")
}

// TestForcedNotesAreIdempotent keeps a re-run from stacking a second notice
// onto the release body.
func TestForcedNotesAreIdempotent(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	first, code := composeNotes(t, "Notes.\n", allPassed, nil)
	requireCode(t, code, 0, first)

	second, code := composeNotes(t, first, allPassed, nil)

	if code != 3 {
		t.Errorf("expected exit 3 for an already-recorded body, got %d\n%s", code, second)
	}

	if strings.Count(second, "<!-- unbounded:forced-publish -->") != 1 {
		t.Errorf("expected exactly one marker, got %d\n%s",
			strings.Count(second, "<!-- unbounded:forced-publish -->"), second)
	}

	// The body still has to be emitted, because the caller applies stdout and
	// clears the draft flag in one call.
	requireContains(t, second, "Notes.")
}

func TestForcedNotesRequireAReason(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	for _, reason := range []string{"", "   "} {
		output, code := composeNotes(t, "Notes.\n", allPassed, map[string]string{"REASON": reason})

		requireCode(t, code, 2, output)
		requireContains(t, output, "requires a non-empty reason")
	}
}

// TestForcedNotesHandleABodyWithoutATrailingNewline covers a release whose
// generated notes do not end cleanly, which must not run the marker onto the
// last line of the changelog.
func TestForcedNotesHandleABodyWithoutATrailingNewline(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	output, code := composeNotes(t, "No trailing newline", allPassed, nil)

	requireCode(t, code, 0, output)
	requireContains(t, output, "No trailing newline\n\n\n---")
}

func TestForcedNotesRejectAMissingBody(t *testing.T) {
	requireBash4(t)
	t.Parallel()

	script, err := filepath.Abs("forced-publish-notes.sh")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	command := exec.Command("bash", script, filepath.Join(t.TempDir(), "absent.md"))
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"ACTOR=someone", "REASON=why",
		"DEPLOY_RESULT=success", "ORCA_RESULT=success",
		"DISCOVER_RESULT=success", "SMOKE_RESULT=success",
	}

	out, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a failure for a missing body file\n%s", out)
	}

	requireContains(t, string(out), "no release body")
}
