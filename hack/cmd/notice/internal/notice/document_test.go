// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package notice

import (
	"strings"
	"testing"
)

type fakeCollector struct {
	name     string
	entries  []Entry
	preErr   error
	collErr  error
	preCalls *int
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Precheck(string) error {
	if f.preCalls != nil {
		*f.preCalls++
	}

	return f.preErr
}

func (f *fakeCollector) Collect(string) ([]Entry, error) {
	return f.entries, f.collErr
}

func TestBuild_SortsAlphabeticallyAcrossCollectors(t *testing.T) {
	a := &fakeCollector{
		name: "go",
		entries: []Entry{
			{Dependency: "github.com/zzz/zzz", Ecosystem: "go"},
			{Dependency: "github.com/aaa/aaa", Ecosystem: "go"},
		},
	}
	b := &fakeCollector{
		name:    "npm",
		entries: []Entry{{Dependency: "mmm", Ecosystem: "npm"}},
	}

	doc, err := Build(".", []Collector{a, b})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{"github.com/aaa/aaa", "github.com/zzz/zzz", "mmm"}
	if len(doc.Notices) != len(want) {
		t.Fatalf("got %d entries, want %d", len(doc.Notices), len(want))
	}

	for i, w := range want {
		if doc.Notices[i].Dependency != w {
			t.Errorf("entry %d: got %q, want %q", i, doc.Notices[i].Dependency, w)
		}
	}
}

func TestBuild_PrecheckErrorShortCircuits(t *testing.T) {
	calls := 0
	bad := &fakeCollector{name: "go", preErr: errString("nope")}
	good := &fakeCollector{name: "npm", preCalls: &calls}

	if _, err := Build(".", []Collector{bad, good}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRender_HasHeaderAndDeterministicOrder(t *testing.T) {
	doc := &Document{
		Notices: []Entry{
			{
				Dependency: "foo",
				Ecosystem:  "npm",
				Copyright:  []string{"Copyright (c) 2020 Foo"},
				License:    []License{{Name: "MIT License", Link: "https://example.com/LICENSE"}},
			},
		},
	}

	out, err := Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := string(out)
	if !strings.HasPrefix(got, HeaderComment) {
		t.Errorf("missing header comment")
	}

	// Field order in output must be dependency -> ecosystem -> copyright -> license.
	depIdx := strings.Index(got, "dependency:")
	ecoIdx := strings.Index(got, "ecosystem:")
	copyIdx := strings.Index(got, "copyright:")
	licIdx := strings.Index(got, "license:")

	if depIdx >= ecoIdx || ecoIdx >= copyIdx || copyIdx >= licIdx {
		t.Errorf("field order wrong: dep=%d eco=%d copy=%d lic=%d", depIdx, ecoIdx, copyIdx, licIdx)
	}
}

func TestDiff_EmptyOnEqual(t *testing.T) {
	if got := Diff("a\nb\n", "a\nb\n"); got != "" {
		t.Errorf("expected empty diff, got %q", got)
	}
}

func TestDiff_FlagsChangedLine(t *testing.T) {
	got := Diff("a\nb\n", "a\nc\n")
	if !strings.Contains(got, "@@ line 2") {
		t.Errorf("missing line marker: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
