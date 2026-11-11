// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// test-summary turns a `go test -json` event stream into a Markdown summary for
// the GitHub Actions job page: the counts, every skipped test with its reason,
// and every failure.
//
// Tests run verbosely, but a skipped test is still one line among tens of
// thousands in the log. A test that skips on every run, because a variable is
// never set or a tool is never installed, passes review and CI unnoticed
// indefinitely. The job summary puts the list where people look.
//
// It reports and never judges: a failed test is the test run's verdict to
// give, so this exits zero whatever the results, and non-zero only when the
// stream cannot be read or holds no test results at all.
//
// Usage:
//
//	test-summary [go-test-json-file] >> "$GITHUB_STEP_SUMMARY"
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// modulePrefix is trimmed from package paths to keep the tables readable.
const modulePrefix = "github.com/Azure/unbounded/"

// maxReason bounds a skip reason, so one chatty test cannot swamp the table.
const maxReason = 300

// event is one line of `go test -json` output. See `go doc test2json`.
type event struct {
	Action  string
	Package string
	Test    string
	Output  string
	// OutputType is "frame" for the lines go test writes itself, such as
	// "=== RUN" and "--- SKIP". Older toolchains leave it unset.
	OutputType string
}

// testID names one test, subtests included.
type testID struct {
	pkg  string
	test string
}

// outcome is a test that skipped or failed.
type outcome struct {
	testID

	// reason is what a skipped test logged, framing lines removed.
	reason string
}

// results is everything the summary reports.
type results struct {
	passed int
	skips  []outcome
	fails  []outcome
	// brokenPackages failed with no failing test of their own: a build error,
	// a panic outside a test, or a TestMain that refused to run.
	brokenPackages []string
}

var errNoResults = errors.New("no test results found: the tests did not run, or the report was truncated")

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "test-summary: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	var input io.Reader

	switch len(args) {
	case 0:
		input = stdin
	case 1:
		file, err := os.Open(args[0])
		if err != nil {
			return fmt.Errorf("open test report: %w", err)
		}

		defer file.Close() //nolint:errcheck // read-only

		input = file
	default:
		return errors.New("usage: test-summary [go-test-json-file]")
	}

	res, err := parse(input)
	if err != nil {
		return err
	}

	_, err = io.WriteString(stdout, res.markdown())

	return err
}

// parse reads the event stream. Output is held only until its test finishes,
// and kept only for tests that skipped.
func parse(r io.Reader) (*results, error) {
	decoder := json.NewDecoder(r)
	output := map[testID][]string{}
	failedTests := map[string]bool{}
	failedPackages := map[string]bool{}
	res := &results{}

	for {
		var ev event

		err := decoder.Decode(&ev)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("parse test report: %w", err)
		}

		if ev.Test == "" {
			if ev.Action == "fail" && ev.Package != "" {
				failedPackages[ev.Package] = true
			}

			continue
		}

		id := testID{pkg: ev.Package, test: ev.Test}

		switch ev.Action {
		case "output":
			if !isFrame(ev) {
				output[id] = append(output[id], ev.Output)
			}
		case "pass":
			res.passed++
		case "skip":
			res.skips = append(res.skips, outcome{testID: id, reason: reason(output[id])})
		case "fail":
			res.fails = append(res.fails, outcome{testID: id})
			failedTests[ev.Package] = true
		}

		switch ev.Action {
		case "pass", "skip", "fail":
			delete(output, id)
		}
	}

	for pkg := range failedPackages {
		if !failedTests[pkg] {
			res.brokenPackages = append(res.brokenPackages, pkg)
		}
	}

	if res.passed+len(res.skips)+len(res.fails)+len(res.brokenPackages) == 0 {
		return nil, errNoResults
	}

	sortOutcomes(res.skips)
	sortOutcomes(res.fails)
	sort.Strings(res.brokenPackages)

	return res, nil
}

// isFrame reports whether ev is a line go test writes around a test, rather
// than something the test logged.
func isFrame(ev event) bool {
	if ev.OutputType == "frame" {
		return true
	}

	line := strings.TrimSpace(ev.Output)

	for _, prefix := range []string{"=== ", "--- PASS", "--- SKIP", "--- FAIL"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}

// reason joins what a test logged into one line.
func reason(lines []string) string {
	var parts []string

	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	joined := strings.Join(parts, " ")
	if runes := []rune(joined); len(runes) > maxReason {
		joined = string(runes[:maxReason]) + "..."
	}

	return joined
}

func sortOutcomes(outcomes []outcome) {
	sort.Slice(outcomes, func(i, j int) bool {
		if outcomes[i].pkg != outcomes[j].pkg {
			return outcomes[i].pkg < outcomes[j].pkg
		}

		return outcomes[i].test < outcomes[j].test
	})
}

func (r *results) markdown() string {
	var b strings.Builder

	b.WriteString("## Go tests\n\n")
	fmt.Fprintf(&b, "%d passed, %d skipped, %d failed", r.passed, len(r.skips), len(r.fails))

	if n := len(r.brokenPackages); n > 0 {
		fmt.Fprintf(&b, ", %d %s failed outside any test", n, pluralize(n, "package", "packages"))
	}

	b.WriteString(".\n")

	if len(r.fails) > 0 || len(r.brokenPackages) > 0 {
		fmt.Fprintf(&b, "\n### Failed (%d)\n\nDetails are in the Test step's log.\n\n", len(r.fails)+len(r.brokenPackages))

		for _, pkg := range r.brokenPackages {
			fmt.Fprintf(&b, "- `%s` (package)\n", shortPackage(pkg))
		}

		for _, fail := range r.fails {
			fmt.Fprintf(&b, "- `%s` `%s`\n", shortPackage(fail.pkg), fail.test)
		}
	}

	if len(r.skips) == 0 {
		b.WriteString("\nNo tests were skipped.\n")

		return b.String()
	}

	fmt.Fprintf(&b, "\n### Skipped (%d)\n\n", len(r.skips))
	b.WriteString("| Package | Test | Reason |\n| --- | --- | --- |\n")

	for _, skip := range r.skips {
		why := skip.reason
		if why == "" {
			why = "(no reason given)"
		}

		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", shortPackage(skip.pkg), escapeCell(skip.test), escapeCell(why))
	}

	return b.String()
}

func shortPackage(pkg string) string {
	if short := strings.TrimPrefix(pkg, modulePrefix); short != "" {
		return short
	}

	return pkg
}

// escapeCell keeps a reason inside its Markdown table cell.
func escapeCell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}
