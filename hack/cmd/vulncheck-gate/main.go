// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// vulncheck-gate decides whether a govulncheck run should fail the build.
//
// It fails only on vulnerabilities that are both reachable from our code and
// have a final-release fix, because those are the only ones anyone can act on
// with a production module bump. A reachable vulnerability with no final fix
// is reported prominently and allowed through. Prereleases and Go
// pseudo-versions are reported as such rather than forcing unstable dependency
// versions into the build.
//
// That trade is deliberate and it has a cost: a serious vulnerability with no
// final-release fix will pass this gate every day without anyone being forced
// to look at it. The report exists so that "nobody was forced to" does not
// become "nobody knew". Adopting an unstable fix, replacing the dependency, or
// waiting for a final release is a judgment call this tool cannot make.
//
// Input is the JSON stream from `govulncheck -format json`. That format is the
// documented programmatic interface; the human-readable output is not, and the
// previous incarnation of this gate parsed it and broke the first time the
// finding count changed.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// govulncheckMessage is one object in the `govulncheck -format json` stream.
// Exactly one field is populated per message. Only the three that bear on the
// verdict are decoded; config is decoded because its absence means the scan
// did not run (see errNoConfig).
type govulncheckMessage struct {
	Config  *configMessage `json:"config"`
	OSV     *osvRecord     `json:"osv"`
	Finding *finding       `json:"finding"`
}

type configMessage struct {
	ScannerName    string `json:"scanner_name"`
	ScannerVersion string `json:"scanner_version"`
	ScanLevel      string `json:"scan_level"`
}

type osvRecord struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

type finding struct {
	OSV string `json:"osv"`

	// FixedVersion is absent, not empty, when no fix has been published. A
	// nonempty value can still be a prerelease, which the gate reports but does
	// not treat as a production-ready module bump.
	FixedVersion string `json:"fixed_version"`

	// Trace runs from the vulnerable symbol outward. A frame carries a
	// function only at symbol scan level, and only for a vulnerability
	// govulncheck could actually trace into our code, so trace[0].function is
	// what distinguishes "we call this" from "this is somewhere in the module
	// graph". It reproduces govulncheck's own "your code is affected by N
	// vulnerabilities" set.
	Trace []traceFrame `json:"trace"`
}

type traceFrame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Receiver string `json:"receiver"`
}

// vulnerability is what the gate knows about one OSV entry after folding
// together every finding that mentions it.
type vulnerability struct {
	id      string
	summary string
	module  string
	version string

	// reachable is true when any finding traced into our code.
	reachable bool

	// fixedVersion is set when any reachable finding has one. A final release
	// is preferred when an OSV spans findings with different fixes.
	fixedVersion string
}

func (v vulnerability) blocking() bool {
	return v.reachable && blockingFix(v.fixedVersion)
}

// blockingFix reports whether version is suitable for a production module
// bump. Unexpected nonempty versions fail closed so a format change cannot
// silently disarm the gate.
func blockingFix(version string) bool {
	if version == "" {
		return false
	}

	return !semver.IsValid(version) || semver.Prerelease(version) == ""
}

// errNoConfig guards the one failure mode that would silently disarm the gate.
//
// An empty or truncated input decodes cleanly into zero findings, which is
// indistinguishable from a clean scan. govulncheck always emits a config
// message first, so requiring one turns "the scan did not run" into a loud
// failure rather than a pass.
var errNoConfig = errors.New("no govulncheck config message found: the scan did not run, or its output was truncated")

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vulncheck-gate: %v\n", err)
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
			return fmt.Errorf("open findings: %w", err)
		}

		defer file.Close() //nolint:errcheck // read-only

		input = file
	default:
		return errors.New("usage: vulncheck-gate [govulncheck-json-file]")
	}

	vulns, err := parse(input)
	if err != nil {
		return err
	}

	return report(stdout, vulns, os.Getenv("GITHUB_ACTIONS") == "true")
}

// parse folds the govulncheck message stream into one entry per OSV.
func parse(r io.Reader) ([]vulnerability, error) {
	decoder := json.NewDecoder(r)

	var (
		sawConfig bool
		vulns     = map[string]*vulnerability{}
	)

	for {
		var message govulncheckMessage

		switch err := decoder.Decode(&message); {
		case errors.Is(err, io.EOF):
			if !sawConfig {
				return nil, errNoConfig
			}

			return sorted(vulns), nil
		case err != nil:
			return nil, fmt.Errorf("decode govulncheck output: %w", err)
		}

		switch {
		case message.Config != nil:
			sawConfig = true
		case message.OSV != nil:
			entry := entryFor(vulns, message.OSV.ID)
			entry.summary = message.OSV.Summary
		case message.Finding != nil:
			absorb(entryFor(vulns, message.Finding.OSV), message.Finding)
		}
	}
}

func entryFor(vulns map[string]*vulnerability, id string) *vulnerability {
	if entry, ok := vulns[id]; ok {
		return entry
	}

	entry := &vulnerability{id: id}
	vulns[id] = entry

	return entry
}

// absorb merges one finding into the entry for its OSV.
//
// Findings arrive at several levels for the same vulnerability - module,
// package, symbol - so this accumulates rather than overwrites. Only the
// symbol-level ones carry a function, and only those establish reachability or
// contribute a fixed version.
func absorb(entry *vulnerability, f *finding) {
	if len(f.Trace) == 0 {
		return
	}

	top := f.Trace[0]

	// The module-level finding is the one that names the module and the
	// version we are actually on; deeper frames repeat it.
	if entry.module == "" {
		entry.module = top.Module
		entry.version = top.Version
	}

	if top.Function == "" {
		return
	}

	entry.reachable = true

	if f.FixedVersion == "" {
		return
	}

	if entry.fixedVersion == "" {
		entry.fixedVersion = f.FixedVersion

		return
	}

	// Do not let an earlier prerelease hide a final fix reported by another
	// reachable finding for the same OSV.
	if blockingFix(f.FixedVersion) && !blockingFix(entry.fixedVersion) {
		entry.fixedVersion = f.FixedVersion
	}
}

func sorted(vulns map[string]*vulnerability) []vulnerability {
	out := make([]vulnerability, 0, len(vulns))
	for _, entry := range vulns {
		out = append(out, *entry)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })

	return out
}

// report prints the verdict and returns an error when anything blocks.
//
// The whole report is rendered into a builder and written once, so there is a
// single write to check rather than one per line.
func report(w io.Writer, vulns []vulnerability, annotate bool) error {
	var blocking, withoutFinalFix []vulnerability

	for _, v := range vulns {
		switch {
		case v.blocking():
			blocking = append(blocking, v)
		case v.reachable:
			withoutFinalFix = append(withoutFinalFix, v)
		}
	}

	var b strings.Builder

	if len(withoutFinalFix) > 0 {
		fmt.Fprintf(&b, "Reachable without a final-release fix (%d, not blocking):\n\n", len(withoutFinalFix))

		for _, v := range withoutFinalFix {
			b.WriteString(describe(v))

			if annotate {
				// Annotations surface these on the workflow summary, which is
				// the only reason anyone reads a passing job.
				fmt.Fprintf(&b, "::warning title=%s::%s (%s): %s\n", v.id, v.summaryOrID(), v.module, v.nonblockingReason())
			}
		}
	}

	if len(blocking) > 0 {
		fmt.Fprintf(&b, "Reachable with a final-release fix (%d, blocking):\n\n", len(blocking))

		for _, v := range blocking {
			b.WriteString(describe(v))
		}
	} else {
		b.WriteString("vulncheck: no reachable vulnerability has a final-release fix\n")
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	if len(blocking) == 0 {
		return nil
	}

	return fmt.Errorf("%s reachable with a final-release fix: upgrade the affected module(s)", plural(len(blocking)))
}

func describe(v vulnerability) string {
	var b strings.Builder

	fmt.Fprintf(&b, "  %s  %s\n", v.id, v.summaryOrID())
	fmt.Fprintf(&b, "    module:    %s@%s\n", v.module, v.version)

	if blockingFix(v.fixedVersion) {
		fmt.Fprintf(&b, "    fixed in:  %s\n", v.fixedVersion)
	} else if v.fixedVersion != "" {
		fmt.Fprintf(&b, "    prerelease fix: %s\n", v.fixedVersion)
	}

	fmt.Fprintf(&b, "    more info: https://pkg.go.dev/vuln/%s\n\n", v.id)

	return b.String()
}

func (v vulnerability) summaryOrID() string {
	if v.summary != "" {
		return v.summary
	}

	return v.id
}

func (v vulnerability) nonblockingReason() string {
	if v.fixedVersion != "" {
		return "only a prerelease fix is available: " + v.fixedVersion
	}

	return "no fix is available"
}

func plural(n int) string {
	if n == 1 {
		return "1 vulnerability is"
	}

	return fmt.Sprintf("%d vulnerabilities are", n)
}
