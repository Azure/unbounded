// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pkg = modulePrefix + "internal/widget"

// stream encodes events the way `go test -json` writes them, one per line.
func stream(t *testing.T, events ...event) string {
	t.Helper()

	var b strings.Builder

	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		b.Write(line)
		b.WriteByte('\n')
	}

	return b.String()
}

func out(test, text string) event {
	return event{Action: "output", Package: pkg, Test: test, Output: text}
}

func frame(test, text string) event {
	return event{Action: "output", Package: pkg, Test: test, Output: text, OutputType: "frame"}
}

func done(action, test string) event {
	return event{Action: action, Package: pkg, Test: test}
}

func summarize(t *testing.T, input string) string {
	t.Helper()

	var stdout bytes.Buffer
	if err := run(nil, strings.NewReader(input), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	return stdout.String()
}

// realSkip is a skipped test exactly as go1.27 `go test -json` reported it.
const realSkip = `{"Action":"run","Package":"github.com/Azure/unbounded/internal/net/html","Test":"TestClusterStatusFS"}
{"Action":"output","Package":"github.com/Azure/unbounded/internal/net/html","Test":"TestClusterStatusFS","Output":"=== RUN   TestClusterStatusFS\n","OutputType":"frame"}
{"Action":"output","Package":"github.com/Azure/unbounded/internal/net/html","Test":"TestClusterStatusFS","Output":"    pages_test.go:21: frontend not built; run ` + "`make net-frontend`" + ` to enable this test\n"}
{"Action":"output","Package":"github.com/Azure/unbounded/internal/net/html","Test":"TestClusterStatusFS","Output":"--- SKIP: TestClusterStatusFS (0.00s)\n","OutputType":"frame"}
{"Action":"skip","Package":"github.com/Azure/unbounded/internal/net/html","Test":"TestClusterStatusFS","Elapsed":0}
{"Action":"output","Package":"github.com/Azure/unbounded/internal/net/html","Output":"PASS\n"}
{"Action":"pass","Package":"github.com/Azure/unbounded/internal/net/html","Elapsed":0.01}
`

func TestSummaryListsARealSkipWithItsReason(t *testing.T) {
	t.Parallel()

	got := summarize(t, realSkip)

	for _, want := range []string{
		"0 passed, 1 skipped, 0 failed.",
		"### Skipped (1)",
		"| `internal/net/html` | `TestClusterStatusFS` | pages_test.go:21: frontend not built; run `make net-frontend` to enable this test |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not contain %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "=== RUN") || strings.Contains(got, "--- SKIP") {
		t.Errorf("summary includes framing lines:\n%s", got)
	}
}

func TestSummaryCountsAndSortsResults(t *testing.T) {
	t.Parallel()

	const other = modulePrefix + "cmd/alpha"

	got := summarize(t, stream(t,
		done("pass", "TestA"),
		done("pass", "TestB"),
		done("pass", "TestB/sub"),
		out("TestZ", "    z_test.go:9: needs root\n"),
		done("skip", "TestZ"),
		out("TestC/sub", "    c_test.go:4: no fixture\n"),
		done("skip", "TestC/sub"),
		done("pass", "TestC"),
		event{Action: "output", Package: other, Test: "TestY", Output: "    y_test.go:1: set Y_DIR\n"},
		event{Action: "skip", Package: other, Test: "TestY"},
	))

	if !strings.Contains(got, "4 passed, 3 skipped, 0 failed.") {
		t.Errorf("wrong counts:\n%s", got)
	}

	alpha := strings.Index(got, "`cmd/alpha` | `TestY`")
	sub := strings.Index(got, "`internal/widget` | `TestC/sub`")
	z := strings.Index(got, "`internal/widget` | `TestZ`")

	if alpha < 0 || sub < 0 || z < 0 || alpha >= sub || sub >= z {
		t.Errorf("skips are missing or not sorted by package, then test:\n%s", got)
	}

	if strings.Contains(got, "### Failed") {
		t.Errorf("summary has a Failed section with no failures:\n%s", got)
	}
}

// TestSummaryAttributesInterleavedOutput covers parallel tests, whose output
// arrives interleaved. Each line belongs to the test the event names.
func TestSummaryAttributesInterleavedOutput(t *testing.T) {
	t.Parallel()

	got := summarize(t, stream(t,
		frame("TestOne", "=== CONT  TestOne\n"),
		out("TestOne", "    one_test.go:1: reason one\n"),
		frame("TestTwo", "=== CONT  TestTwo\n"),
		out("TestTwo", "    two_test.go:1: reason two\n"),
		done("skip", "TestTwo"),
		done("skip", "TestOne"),
	))

	for _, want := range []string{
		"| `TestOne` | one_test.go:1: reason one |",
		"| `TestTwo` | two_test.go:1: reason two |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not contain %q:\n%s", want, got)
		}
	}
}

// TestSummaryDropsFramesFromOlderToolchains covers streams with no OutputType,
// where framing lines are recognized by their prefix.
func TestSummaryDropsFramesFromOlderToolchains(t *testing.T) {
	t.Parallel()

	got := summarize(t, stream(t,
		out("TestOld", "=== RUN   TestOld\n"),
		out("TestOld", "    old_test.go:3: first line\n"),
		out("TestOld", "    second line\n"),
		out("TestOld", "--- SKIP: TestOld (0.00s)\n"),
		done("skip", "TestOld"),
	))

	if !strings.Contains(got, "| `TestOld` | old_test.go:3: first line second line |") {
		t.Errorf("framing lines were not dropped, or lines were not joined:\n%s", got)
	}
}

func TestSummaryOutputDoesNotLeakBetweenRuns(t *testing.T) {
	t.Parallel()

	// The same test passes in one package run and skips in a later one, as
	// with -count=2. Only the second run's output is the skip's reason.
	got := summarize(t, stream(t,
		out("TestFlip", "    flip_test.go:1: passing chatter\n"),
		done("pass", "TestFlip"),
		out("TestFlip", "    flip_test.go:2: skipping now\n"),
		done("skip", "TestFlip"),
	))

	if !strings.Contains(got, "| `TestFlip` | flip_test.go:2: skipping now |") {
		t.Errorf("reason includes output from an earlier run:\n%s", got)
	}
}

func TestSummaryKeepsTheTableIntact(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", maxReason+50)

	got := summarize(t, stream(t,
		out("TestPipe", "    p_test.go:1: a|b\n"),
		done("skip", "TestPipe"),
		out("TestLong", "    "+long+"\n"),
		done("skip", "TestLong"),
		done("skip", "TestSilent"),
		done("skip", "TestTable/a|b"),
	))

	for _, want := range []string{
		`| p_test.go:1: a\|b |`,
		"| " + strings.Repeat("x", maxReason) + "... |",
		"| `TestSilent` | (no reason given) |",
		"| `TestTable/a\\|b` |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not contain %q:\n%s", want, got)
		}
	}
}

func TestSummaryListsFailures(t *testing.T) {
	t.Parallel()

	const broken = modulePrefix + "internal/broken"

	got := summarize(t, stream(t,
		done("pass", "TestOK"),
		out("TestBad", "    bad_test.go:7: got 1, want 2\n"),
		done("fail", "TestBad"),
		event{Action: "fail", Package: pkg},
		event{Action: "output", Package: broken, Output: "rendered manifests are missing\n"},
		event{Action: "fail", Package: broken},
	))

	for _, want := range []string{
		"1 passed, 0 skipped, 1 failed, 1 package failed outside any test.",
		"### Failed (2)",
		"- `internal/broken` (package)",
		"- `internal/widget` `TestBad`",
		"No tests were skipped.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not contain %q:\n%s", want, got)
		}
	}

	// internal/widget failed because TestBad did, so it is not also reported
	// as failing outside a test.
	if strings.Contains(got, "`internal/widget` (package)") {
		t.Errorf("a package with a failing test is reported as broken:\n%s", got)
	}
}

func TestSummaryShortensOnlyThisModulesPackages(t *testing.T) {
	t.Parallel()

	got := summarize(t, stream(t,
		event{Action: "skip", Package: "example.com/elsewhere", Test: "TestX"},
	))

	if !strings.Contains(got, "| `example.com/elsewhere` | `TestX` |") {
		t.Errorf("a foreign package path was altered:\n%s", got)
	}
}

func TestSummaryRejectsAReportWithNoResults(t *testing.T) {
	t.Parallel()

	for name, input := range map[string]string{
		"empty":         "",
		"build started": `{"Action":"start","Package":"` + pkg + `"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := run(nil, strings.NewReader(input), &bytes.Buffer{})
			if !errors.Is(err, errNoResults) {
				t.Errorf("err = %v, want errNoResults", err)
			}
		})
	}
}

func TestSummaryRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	err := run(nil, strings.NewReader("PASS\nok  \tpkg\t0.1s\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "parse test report") {
		t.Errorf("err = %v, want a parse error", err)
	}
}

func TestRunReadsAFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "go-test.json")
	if err := os.WriteFile(path, []byte(realSkip), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := run([]string{path}, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(stdout.String(), "TestClusterStatusFS") {
		t.Errorf("summary does not come from the file:\n%s", stdout.String())
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	t.Parallel()

	if err := run([]string{filepath.Join(t.TempDir(), "absent.json")}, nil, &bytes.Buffer{}); err == nil {
		t.Error("run with a missing file: want an error")
	}

	err := run([]string{"a.json", "b.json"}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("err = %v, want a usage error", err)
	}
}
