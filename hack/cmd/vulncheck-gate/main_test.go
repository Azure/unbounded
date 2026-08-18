// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

// TestGateBlocksOnlyOnFixableReachableVulnerabilities is the policy.
//
// The failing case cannot be observed against the repository's own scan: every
// vulnerability that currently reaches our code is unfixable upstream, so
// without a fixture the blocking path would ship untested and a regression
// would look exactly like a clean run.
func TestGateBlocksOnlyOnFixableReachableVulnerabilities(t *testing.T) {
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
			wantMention: []string{"GO-2026-6170", "no fix available", "not blocking"},
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
			wantMention: []string{"no reachable vulnerability has an available fix"},
		},
		{
			// One OSV can span modules. If any reachable finding names a fix,
			// there is something to bump, so the whole entry blocks.
			name: "mixed findings for one OSV block on the fixable one",
			input: stream(
				configMsg,
				osvMsg("GO-2026-7777", "Two modules, one fixed"),
				symbolFinding("GO-2026-7777", "example.com/nofix", "v1.0.0", "", "A"),
				symbolFinding("GO-2026-7777", "example.com/fixed", "v1.0.0", "v1.1.0", "B"),
			),
			wantBlocked: true,
			wantMention: []string{"GO-2026-7777", "fixed in:  v1.1.0"},
		},
		{
			name:        "a clean scan passes",
			input:       stream(configMsg),
			wantBlocked: false,
			wantMention: []string{"no reachable vulnerability has an available fix"},
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
// A passing job's log is not read, so the unfixable set is surfaced as a
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
