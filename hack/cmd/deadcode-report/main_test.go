// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJSON puts a deadcode JSON document in a temp dir and returns its path.
func writeJSON(t *testing.T, name, body string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	return p
}

const (
	// twoFuncs is one package with two dead functions.
	twoFuncs = `[{"Name":"netlink","Path":"example.com/m/internal/netlink","Funcs":[
	  {"Name":"Alpha","Position":{"File":"internal/netlink/a.go","Line":10,"Col":6},"Generated":false,"Marker":false},
	  {"Name":"Beta","Position":{"File":"internal/netlink/b.go","Line":20,"Col":6},"Generated":false,"Marker":false}]}]`

	// onlyAlpha is the same package with Beta live, as a second tag
	// configuration would report it.
	onlyAlpha = `[{"Name":"netlink","Path":"example.com/m/internal/netlink","Funcs":[
	  {"Name":"Alpha","Position":{"File":"internal/netlink/a.go","Line":11,"Col":6},"Generated":false,"Marker":false}]}]`
)

func TestIntersectionKeepsOnlyFunctionsDeadInEveryConfiguration(t *testing.T) {
	a := writeJSON(t, "a.json", twoFuncs)
	b := writeJSON(t, "b.json", onlyAlpha)

	var out strings.Builder
	if err := run([]string{"-with-tests", a, "-with-tests", b}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Alpha") {
		t.Errorf("Alpha is dead in both inputs and must be reported:\n%s", got)
	}

	if strings.Contains(got, "Beta") {
		t.Errorf("Beta is live in the second input and must not be reported:\n%s", got)
	}

	if !strings.Contains(got, "Unreachable from any binary or test (1)") {
		t.Errorf("want a count of 1 in the section header:\n%s", got)
	}
}

// A shifted line number is not a different function. Intersecting on position
// rather than identity would silently empty the report after any edit above a
// declaration, which is the failure this pins down.
func TestIntersectionIgnoresLineNumberDrift(t *testing.T) {
	a := writeJSON(t, "a.json", onlyAlpha)
	b := writeJSON(t, "b.json", twoFuncs)

	var out strings.Builder
	if err := run([]string{"-with-tests", a, "-with-tests", b}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "Alpha") {
		t.Errorf("Alpha is at line 11 in one input and line 10 in the other; it is still the same function:\n%s", out.String())
	}
}

func TestTestOnlySectionExcludesFunctionsAlreadyReportedAsUnreachable(t *testing.T) {
	withTests := writeJSON(t, "with.json", onlyAlpha)
	withoutTests := writeJSON(t, "without.json", twoFuncs)

	var out strings.Builder

	err := run([]string{"-with-tests", withTests, "-without-tests", withoutTests}, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()

	unreachable, testOnly, ok := strings.Cut(got, "Reachable only from tests")
	if !ok {
		t.Fatalf("want a test-only section:\n%s", got)
	}

	if !strings.Contains(unreachable, "Alpha") {
		t.Errorf("Alpha is dead with -test and belongs in the first section:\n%s", got)
	}

	if strings.Contains(testOnly, "Alpha") {
		t.Errorf("Alpha is already reported as unreachable and must not repeat in the test-only section:\n%s", got)
	}

	if !strings.Contains(testOnly, "Beta") {
		t.Errorf("Beta is dead without -test and live with it, which is the definition of test-only:\n%s", got)
	}
}

// Omitting -without-tests means the question was not asked. Printing an empty
// section would answer it, wrongly.
func TestTestOnlySectionIsOmittedWhenNotAsked(t *testing.T) {
	withTests := writeJSON(t, "with.json", twoFuncs)

	var out strings.Builder
	if err := run([]string{"-with-tests", withTests}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(out.String(), "Reachable only from tests") {
		t.Errorf("no -without-tests input was given, so the section must be absent:\n%s", out.String())
	}
}

func TestGeneratedAndMarkerFunctionsAreNeverReported(t *testing.T) {
	body := `[{"Name":"api","Path":"example.com/m/api","Funcs":[
	  {"Name":"DeepCopy","Position":{"File":"api/zz_generated.deepcopy.go","Line":10,"Col":6},"Generated":true,"Marker":false},
	  {"Name":"isNode","Position":{"File":"api/types.go","Line":20,"Col":6},"Generated":false,"Marker":true},
	  {"Name":"Real","Position":{"File":"api/types.go","Line":30,"Col":6},"Generated":false,"Marker":false}]}]`

	var out strings.Builder
	if err := run([]string{"-with-tests", writeJSON(t, "a.json", body)}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()

	for _, name := range []string{"DeepCopy", "isNode"} {
		if strings.Contains(got, name) {
			t.Errorf("%s is generated or a marker method and must not be reported:\n%s", name, got)
		}
	}

	if !strings.Contains(got, "Real") {
		t.Errorf("want the ordinary function reported:\n%s", got)
	}
}

// An empty array is what a truncated or failed run produces, and it is
// indistinguishable from a clean tree. Treating it as clean would disarm the
// report exactly when it was needed.
func TestEmptyInputIsAFailureNotACleanReport(t *testing.T) {
	var out strings.Builder

	err := run([]string{"-with-tests", writeJSON(t, "a.json", "[]")}, &out)
	if !errors.Is(err, errEmptyInput) {
		t.Fatalf("want errEmptyInput, got %v", err)
	}
}

func TestMalformedInputIsRejected(t *testing.T) {
	var out strings.Builder

	err := run([]string{"-with-tests", writeJSON(t, "a.json", "not json")}, &out)
	if err == nil {
		t.Fatal("want an error for malformed JSON")
	}

	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("want a decode error, got %v", err)
	}
}

func TestMissingFileIsRejected(t *testing.T) {
	var out strings.Builder

	err := run([]string{"-with-tests", filepath.Join(t.TempDir(), "absent.json")}, &out)
	if err == nil {
		t.Fatal("want an error for a missing file")
	}

	if !strings.Contains(err.Error(), "read") {
		t.Errorf("want a read error, got %v", err)
	}
}

func TestAtLeastOneWithTestsInputIsRequired(t *testing.T) {
	var out strings.Builder

	if err := run(nil, &out); err == nil {
		t.Fatal("want an error when no input is given")
	}
}

func TestCleanTreeReportsNoneRatherThanNothing(t *testing.T) {
	// Every function in the input is generated, so the tree is clean but the
	// scan did run. That has to read differently from a failed scan.
	body := `[{"Name":"api","Path":"example.com/m/api","Funcs":[
	  {"Name":"DeepCopy","Position":{"File":"api/zz_generated.deepcopy.go","Line":10,"Col":6},"Generated":true,"Marker":false}]}]`

	var out strings.Builder
	if err := run([]string{"-with-tests", writeJSON(t, "a.json", body)}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "Unreachable from any binary or test: none") {
		t.Errorf("want an explicit none:\n%s", out.String())
	}
}
