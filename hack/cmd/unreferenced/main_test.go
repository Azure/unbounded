// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// scanFixture runs the tool over a testdata module and returns the report.
func scanFixture(t *testing.T, module string, args ...string) jsonReport {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("testdata", module))
	if err != nil {
		t.Fatalf("resolve fixture: %v", err)
	}

	var out strings.Builder

	full := append([]string{"-C", dir, "-json"}, args...)
	full = append(full, "./...")

	if err := run(full, &out); err != nil {
		t.Fatalf("run %v: %v", full, err)
	}

	var report jsonReport
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}

	return report
}

func names(in []declaration) map[string]declaration {
	out := make(map[string]declaration, len(in))
	for _, d := range in {
		out[d.Name] = d
	}

	return out
}

func TestSimpleFixtureReportsExactlyTheDeadDeclarations(t *testing.T) {
	report := scanFixture(t, "simple")

	got := names(report.Unreferenced)

	want := []string{
		"Unused",            // func with no caller
		"Widget.Orphan",     // dead method on a type that is boxed into an interface
		"LonelyType",        // type referenced only by its own method receiver
		"LonelyType.Method", // method on a type nothing uses
		"Recursive",         // self-reference is not a use
		"UnusedConst",       // const hidden by its group-mate being used
		"UnusedVar",
		"UnusedType",
	}

	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("want %s reported, got %v", name, keys(got))
		}
	}

	notWant := map[string]string{
		"Used":              "called from app",
		"Widget":            "app holds one",
		"Widget.Referenced": "called from app",
		"Widget.String":     "deleting it would break fmt.Stringer",
		"TestedOnly":        "a reference from a test is a reference",
		"UsedConst":         "read by app",
		"GeneratedOrphan":   "lives in a generated file",
		"HelperInATestFile": "declared in a test file, so not a candidate",
		"main":              "called by the runtime, not by an identifier",
	}

	for name, why := range notWant {
		if _, ok := got[name]; ok {
			t.Errorf("%s must not be reported: %s", name, why)
		}
	}

	if len(report.Unreferenced) != len(want) {
		t.Errorf("want exactly %d findings, got %d: %v", len(want), len(report.Unreferenced), keys(got))
	}
}

// The interface filter is the difference between a useful report and a
// dangerous one, so its output is asserted directly rather than only by
// absence from the main list.
func TestMethodKeptByInterfaceIsReportedAsKeptWithItsInterface(t *testing.T) {
	report := scanFixture(t, "simple")

	kept, ok := names(report.Suppressed)["Widget.String"]
	if !ok {
		t.Fatalf("want Widget.String in the kept set, got %v", keys(names(report.Suppressed)))
	}

	if !strings.Contains(kept.Interface, "String() string") {
		t.Errorf("want the interface that kept it named, got %q", kept.Interface)
	}
}

func TestPositionsAreRelativeToTheModuleRoot(t *testing.T) {
	report := scanFixture(t, "simple")

	for _, d := range report.Unreferenced {
		if filepath.IsAbs(d.File) {
			t.Errorf("want a path relative to the module root, got %s", d.File)
		}

		if !strings.HasPrefix(d.File, "lib/") {
			t.Errorf("want lib/..., got %s", d.File)
		}

		if d.Line == 0 {
			t.Errorf("%s has no line number", d.Name)
		}
	}
}

func TestScanningWithoutATagReportsCodeThatOnlyThatTagUses(t *testing.T) {
	report := scanFixture(t, "tagged")

	got := names(report.Unreferenced)
	if _, ok := got["OnlyUnderTag"]; !ok {
		t.Errorf("without -tags special its only caller is invisible, so it must be reported: %v", keys(got))
	}

	if _, ok := got["UseIt"]; ok {
		t.Error("UseIt is behind the tag and was not compiled, so it cannot be reported")
	}
}

// A reference under any scanned configuration is a reference. Intersecting the
// configurations instead of unioning them would delete live code.
func TestReferenceUnderAnyTagConfigurationCounts(t *testing.T) {
	report := scanFixture(t, "tagged", "-tags", "", "-tags", "special")

	got := names(report.Unreferenced)

	if _, ok := got["OnlyUnderTag"]; ok {
		t.Errorf("the `special` configuration calls it, so it is live: %v", keys(got))
	}

	if _, ok := got["NeverUsed"]; !ok {
		t.Errorf("NeverUsed is dead in both configurations: %v", keys(got))
	}

	if _, ok := got["UseIt"]; !ok {
		t.Errorf("UseIt is compiled under `special` and called by nothing: %v", keys(got))
	}
}

func TestNoPackagesIsAnError(t *testing.T) {
	var out strings.Builder

	err := run([]string{"-C", t.TempDir(), "./..."}, &out)
	if err == nil {
		t.Fatal("want an error when the scan cannot run")
	}
}

func TestGeneratedHeaderIsRecognizedOnlyBeforeThePackageClause(t *testing.T) {
	// The convention at https://go.dev/s/generatedcode places the marker
	// before the package clause. A line matching it further down is a comment
	// about generated code, not a claim to be generated.
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"before package", "// Code generated by x. DO NOT EDIT.\n\npackage p\n", true},
		{"after package", "package p\n\n// Code generated by x. DO NOT EDIT.\nfunc F() {}\n", false},
		{"not a marker", "// Code generated by hand\n\npackage p\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGeneratedSource(t, tc.text); got != tc.want {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func keys(m map[string]declaration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func isGeneratedSource(t *testing.T, src string) bool {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	return isGenerated(file)
}
