// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package relctl

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Tests for the dispatch commands.
//
// The confirmation model is what these are really about. Everything here can
// mint a tag or ship a release, and the dispatch endpoint returns no run ID and
// does not report an input a workflow ignored, so the description printed
// before the prompt is the only chance to see what is being sent.

// runDispatch executes a command with stdin wired to the given answer.
//
// The run lookup is disabled: these stubs have no runner, so looking for the
// run a dispatch created would poll its whole timeout and prove nothing.
func runDispatch(t *testing.T, stub *stubGitHub, stdin string, args ...string) (string, error) {
	t.Helper()

	original := dispatchRunTimeout
	dispatchRunTimeout = 0

	t.Cleanup(func() { dispatchRunTimeout = original })

	var out bytes.Buffer

	cmd := newRoot(&Options{
		Repo:    "Azure/unbounded",
		Output:  OutputText,
		BaseURL: stub.start(t),
		Token:   func(context.Context) (string, error) { return "t", nil },
	})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)

	err := cmd.ExecuteContext(t.Context())

	return out.String(), err
}

// TestDryRunDispatchesNothing is the safety property every one of these needs:
// --dry-run must describe and stop, not describe and send.
func TestDryRunDispatchesNothing(t *testing.T) {
	stub := &stubGitHub{}

	out, err := runDispatch(t, stub, "", "cut", "--branch", "main", "--repo-path", tagRepo(t), "--dry-run")
	if err != nil {
		t.Fatalf("cut --dry-run: %v\n%s", err, out)
	}

	if !strings.Contains(out, "Nothing was dispatched") {
		t.Errorf("output does not say it stopped:\n%s", out)
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched %d times during a dry run", stub.dispatched)
	}
}

// TestCutPreviewsTheVersionBeforeAsking is why this is worth having over gh:
// the workflow's own dry_run costs a dispatch and a minute, and the answer is
// computable locally from the same code.
func TestCutPreviewsTheVersionBeforeAsking(t *testing.T) {
	out, err := runDispatch(t, &stubGitHub{}, "",
		"cut", "--branch", "main", "--repo-path", tagRepo(t), "--dry-run")
	if err != nil {
		t.Fatalf("cut: %v\n%s", err, out)
	}

	if !strings.Contains(out, "This will cut v0.5.0 from main") {
		t.Errorf("no version preview:\n%s", out)
	}
}

// TestDispatchRefusedWithoutConfirmation covers the default: no --yes, no "y",
// nothing sent.
func TestDispatchRefusedWithoutConfirmation(t *testing.T) {
	stub := &stubGitHub{}

	out, err := runDispatch(t, stub, "n\n", "cut", "--branch", "main", "--repo-path", tagRepo(t))
	if err == nil {
		t.Fatalf("want an error when the prompt is declined:\n%s", out)
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched despite being declined")
	}
}

func TestDispatchProceedsOnYes(t *testing.T) {
	stub := &stubGitHub{}

	out, err := runDispatch(t, stub, "y\n", "cut", "--branch", "main", "--repo-path", tagRepo(t))
	if err != nil {
		t.Fatalf("cut: %v\n%s", err, out)
	}

	if stub.dispatched != 1 {
		t.Errorf("dispatched %d times, want 1", stub.dispatched)
	}
}

func TestDispatchProceedsWithYesFlag(t *testing.T) {
	stub := &stubGitHub{}

	out, err := runDispatch(t, stub, "", "cut", "--branch", "main", "--repo-path", tagRepo(t), "--yes")
	if err != nil {
		t.Fatalf("cut --yes: %v\n%s", err, out)
	}

	if stub.dispatched != 1 {
		t.Errorf("dispatched %d times, want 1", stub.dispatched)
	}
}

// TestPublishIgnoresYes is the point of the break-glass model. --yes must not
// reach a publish that skipped its soak, so a script passing --yes everywhere
// cannot take this path by accident.
func TestPublishIgnoresYes(t *testing.T) {
	stub := &stubGitHub{}

	out, err := runDispatch(t, stub, "y\n",
		"publish", "v0.4.0", "--reason", "cluster down", "--yes")

	// --yes is not even a flag here, so this fails at parse time. That is the
	// intent: not "ignored", but absent.
	if err == nil {
		t.Fatalf("want an error: --yes must not exist on publish\n%s", out)
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched despite --yes not being a valid flag here")
	}
}

// TestPublishNeedsTheExactPhrase keeps the confirmation from being satisfiable
// by anything typed in a hurry.
func TestPublishNeedsTheExactPhrase(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		want  bool
	}{
		{name: "the exact phrase", stdin: "publish v0.4.0 unsoaked\n", want: true},
		{name: "yes", stdin: "yes\n"},
		{name: "y", stdin: "y\n"},
		{name: "the tag alone", stdin: "v0.4.0\n"},
		{name: "empty", stdin: "\n"},
		{name: "close but wrong", stdin: "publish v0.4.0\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubGitHub{}

			_, err := runDispatch(t, stub, tc.stdin,
				"publish", "v0.4.0", "--reason", "cluster down")

			if tc.want {
				if err != nil {
					t.Fatalf("publish with the exact phrase: %v", err)
				}

				if stub.dispatched != 1 {
					t.Errorf("dispatched %d times, want 1", stub.dispatched)
				}

				return
			}

			if err == nil {
				t.Fatal("want an error for a phrase that does not match")
			}

			if stub.dispatched != 0 {
				t.Errorf("dispatched on a non-matching phrase")
			}
		})
	}
}

func TestPublishRequiresAReason(t *testing.T) {
	stub := &stubGitHub{}

	_, err := runDispatch(t, stub, "publish v0.4.0 unsoaked\n", "publish", "v0.4.0")
	if err == nil {
		t.Fatal("want an error without --reason")
	}

	if !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("error = %q", err)
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched without a reason")
	}
}

// TestSoakForceInitNeedsATypedPhrase keeps the other break-glass path out of
// reach of --yes. A site init on an initialised cluster is not a retry.
func TestSoakForceInitNeedsATypedPhrase(t *testing.T) {
	stub := &stubGitHub{}

	_, err := runDispatch(t, stub, "y\n", "soak", "v0.4.0", "--force-init", "--yes")
	if err == nil {
		t.Fatal("want an error: --yes must not satisfy force-init")
	}

	if stub.dispatched != 0 {
		t.Errorf("dispatched force_init on a bare yes")
	}
}

// TestSoakRetryNeedsNoPhrase is the other half: an ordinary retry is not a
// break-glass action and should not be made to feel like one.
func TestSoakRetryNeedsNoPhrase(t *testing.T) {
	stub := &stubGitHub{}

	out, err := runDispatch(t, stub, "", "soak", "v0.4.0", "--yes")
	if err != nil {
		t.Fatalf("soak: %v\n%s", err, out)
	}

	if stub.dispatched != 1 {
		t.Errorf("dispatched %d times, want 1", stub.dispatched)
	}
}

// TestSoakDistinguishesZeroFromUnset covers a flag whose zero value is
// meaningful: --max-notready-nodes=0 disables tolerance entirely, which is the
// opposite of leaving it alone.
func TestSoakDistinguishesZeroFromUnset(t *testing.T) {
	withZero := &stubGitHub{}

	if _, err := runDispatch(t, withZero, "", "soak", "v0.4.0", "--max-notready-nodes", "0", "--yes"); err != nil {
		t.Fatalf("soak: %v", err)
	}

	if _, ok := withZero.lastInputs["max_notready_nodes"]; !ok {
		t.Error("an explicit 0 was not sent")
	}

	unset := &stubGitHub{}

	if _, err := runDispatch(t, unset, "", "soak", "v0.4.0", "--yes"); err != nil {
		t.Fatalf("soak: %v", err)
	}

	if _, ok := unset.lastInputs["max_notready_nodes"]; ok {
		t.Error("max_notready_nodes was sent when the flag was not given")
	}
}

// TestPrepareDispatchesOnMain pins the ref. release-prepare takes its tooling
// from the default branch deliberately, so dispatching it on a release branch
// would run that branch's copy of the workflow.
func TestPrepareDispatchesOnMain(t *testing.T) {
	stub := &stubGitHub{}

	if _, err := runDispatch(t, stub, "",
		"cut", "--branch", "release-0.4", "--repo-path", tagRepo(t), "--yes"); err != nil {
		t.Fatalf("cut: %v", err)
	}

	if stub.lastRef != "main" {
		t.Errorf("dispatched on ref %q, want main", stub.lastRef)
	}

	if stub.lastInputs["branch"] != "release-0.4" {
		t.Errorf("branch input = %v, want release-0.4", stub.lastInputs["branch"])
	}
}
