// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// stream builds a govulncheck JSON stream from message literals. The real tool
// output is a sequence of pretty-printed objects rather than an array or
// NDJSON, but json.Decoder treats all three the same, so joining with newlines
// is a faithful stand-in.
func stream(messages ...string) string {
	return strings.Join(messages, "\n") + "\n"
}

const configMsg = `{"config":{"scanner_name":"govulncheck","scanner_version":"v1.2.0","scan_level":"symbol"}}`

// osvMsg is the advisory record govulncheck emits for every vulnerability it
// knows about, whether or not our code reaches it.
func osvMsg(id, summary string) string {
	return `{"osv":{"id":"` + id + `","summary":"` + summary + `"}}`
}

// moduleFinding is the shallowest level govulncheck reports: the module is in
// the graph, but nothing has been traced into our code. It carries no function.
func moduleFinding(id, module, version, fixed string) string {
	return `{"finding":{"osv":"` + id + `"` + fixedField(fixed) +
		`,"trace":[{"module":"` + module + `","version":"` + version + `"}]}}`
}

// symbolFinding is the level that establishes reachability: trace[0] names the
// vulnerable function our code calls.
func symbolFinding(id, module, version, fixed, function string) string {
	return `{"finding":{"osv":"` + id + `"` + fixedField(fixed) +
		`,"trace":[{"module":"` + module + `","version":"` + version +
		`","package":"` + module + `","function":"` + function + `"}]}}`
}

// fixedField omits the key entirely when there is no fix, which is what
// govulncheck does. The gate must read an absent key and an empty string
// identically, so the fixtures exercise the absent form.
func fixedField(fixed string) string {
	if fixed == "" {
		return ""
	}

	return `,"fixed_version":"` + fixed + `"`
}

// TestGateBlocksOnlyOnReachableVulnerabilitiesWithFinalFixes is the policy.
//
// Fixtures keep the verdict independent of whichever advisories happen to be
// in the live vulnerability database when the tests run.
func TestGateBlocksOnlyOnReachableVulnerabilitiesWithFinalFixes(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantBlocked bool
		wantMention []string
	}{
		{
			name: "reachable with no fix is reported and allowed",
			input: stream(
				configMsg,
				osvMsg("GO-2026-6170", "Malformed backend frame length causes panic"),
				moduleFinding("GO-2026-6170", "github.com/lib/pq", "v1.12.3", ""),
				symbolFinding("GO-2026-6170", "github.com/lib/pq", "v1.12.3", "", "Open"),
			),
			wantBlocked: false,
			wantMention: []string{"GO-2026-6170", "without a final-release fix", "not blocking"},
		},
		{
			name: "reachable with only a prerelease fix is reported and allowed",
			input: stream(
				configMsg,
				osvMsg("GO-2026-6443", "Something fixed only in development"),
				moduleFinding("GO-2026-6443", "example.com/dev", "v1.84.0", "v1.85.0-dev.0.20260825072537-93e31b48545e"),
				symbolFinding("GO-2026-6443", "example.com/dev", "v1.84.0", "v1.85.0-dev.0.20260825072537-93e31b48545e", "Vulnerable"),
			),
			wantBlocked: false,
			wantMention: []string{"GO-2026-6443", "prerelease fix: v1.85.0-dev.0.20260825072537-93e31b48545e", "not blocking"},
		},
		{
			name: "reachable with a fix blocks",
			input: stream(
				configMsg,
				osvMsg("GO-2026-9999", "Something fixable"),
				moduleFinding("GO-2026-9999", "example.com/mod", "v1.0.0", "v1.2.0"),
				symbolFinding("GO-2026-9999", "example.com/mod", "v1.0.0", "v1.2.0", "Vulnerable"),
			),
			wantBlocked: true,
			wantMention: []string{"GO-2026-9999", "fixed in:  v1.2.0", "blocking"},
		},
		{
			// govulncheck reports these; they are somebody else's problem
			// (dependabot's), and blocking on code we never call would be the
			// same false urgency the old count-based guard produced.
			name: "unreachable with a fix does not block",
			input: stream(
				configMsg,
				osvMsg("GO-2026-8888", "Fixable but never called"),
				moduleFinding("GO-2026-8888", "example.com/unused", "v0.1.0", "v0.2.0"),
			),
			wantBlocked: false,
			wantMention: []string{"no reachable vulnerability has a final-release fix"},
		},
		{
			// One OSV can span modules. A final fix must replace a prerelease
			// encountered first so the actionable finding still blocks.
			name: "mixed findings for one OSV prefer the final fix",
			input: stream(
				configMsg,
				osvMsg("GO-2026-7777", "Two modules, two kinds of fix"),
				symbolFinding("GO-2026-7777", "example.com/prerelease", "v1.0.0", "v1.1.0-rc.1", "A"),
				symbolFinding("GO-2026-7777", "example.com/fixed", "v1.0.0", "v1.1.0", "B"),
			),
			wantBlocked: true,
			wantMention: []string{"GO-2026-7777", "fixed in:  v1.1.0"},
		},
		{
			name:        "a clean scan passes",
			input:       stream(configMsg),
			wantBlocked: false,
			wantMention: []string{"no reachable vulnerability has a final-release fix"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vulns, err := parse(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			var out bytes.Buffer

			err = report(&out, vulns, false)

			if blocked := err != nil; blocked != tc.wantBlocked {
				t.Fatalf("blocked = %v (err %v), want %v\noutput:\n%s", blocked, err, tc.wantBlocked, out.String())
			}

			for _, want := range tc.wantMention {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("output does not mention %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestBlockingFix(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "empty", version: "", want: false},
		{name: "final", version: "v1.2.3", want: true},
		{name: "final with build metadata", version: "v1.2.3+build.1", want: true},
		{name: "development", version: "v1.3.0-dev.0.20260825072537-93e31b48545e", want: false},
		{name: "release candidate", version: "v1.3.0-rc.1", want: false},
		{name: "beta", version: "v1.3.0-beta.1", want: false},
		{name: "Go pseudo-version", version: "v0.0.0-20260719225207-c76316d4aa82", want: false},
		{name: "malformed fails closed", version: "not-semver", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockingFix(tc.version); got != tc.want {
				t.Fatalf("blockingFix(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// TestGateRejectsOutputThatProvesNothing guards the one failure mode that would
// silently disarm the gate.
//
// An empty or truncated stream decodes into zero findings, which is
// indistinguishable from a clean scan. govulncheck always emits config first,
// so its absence means the scan did not run and the result means nothing.
func TestGateRejectsOutputThatProvesNothing(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty output", input: ""},
		{
			name: "findings with no config",
			input: stream(
				osvMsg("GO-2026-6170", "Reachable and unfixable"),
				symbolFinding("GO-2026-6170", "github.com/lib/pq", "v1.12.3", "", "Open"),
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parse(strings.NewReader(tc.input)); !errors.Is(err, errNoConfig) {
				t.Fatalf("parse err = %v, want errNoConfig", err)
			}
		})
	}
}

func TestGateRejectsMalformedOutput(t *testing.T) {
	_, err := parse(strings.NewReader(configMsg + "\n{\"finding\": {\"osv\":\n"))
	if err == nil || errors.Is(err, errNoConfig) {
		t.Fatalf("parse err = %v, want a decode failure", err)
	}
}

// TestAnnotationsAreEmittedOnlyUnderActions pins the reporting side channel.
//
// A passing job's log is not read, so the nonblocking set is surfaced as a
// workflow annotation instead. Emitting the same syntax locally would be noise.
func TestAnnotationsAreEmittedOnlyUnderActions(t *testing.T) {
	vulns, err := parse(strings.NewReader(stream(
		configMsg,
		osvMsg("GO-2026-6170", "Malformed backend frame length causes panic"),
		symbolFinding("GO-2026-6170", "github.com/lib/pq", "v1.12.3", "", "Open"),
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var annotated, plain bytes.Buffer

	if err := report(&annotated, vulns, true); err != nil {
		t.Fatalf("report: %v", err)
	}

	if err := report(&plain, vulns, false); err != nil {
		t.Fatalf("report: %v", err)
	}

	if !strings.Contains(annotated.String(), "::warning title=GO-2026-6170::") {
		t.Fatalf("no workflow annotation emitted:\n%s", annotated.String())
	}

	if strings.Contains(plain.String(), "::warning") {
		t.Fatalf("workflow annotation emitted outside Actions:\n%s", plain.String())
	}
}
