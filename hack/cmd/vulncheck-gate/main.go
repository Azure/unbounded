// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// vulncheck-gate decides whether a govulncheck run should fail the build.
//
// It fails only on vulnerabilities that are both reachable from our code and
// have a published fix, because those are the only ones anyone can act on: the
// fix is a module bump. A reachable vulnerability with no fix available is
// reported prominently and allowed through. Blocking on it would wedge every
// branch in the repository for as long as upstream takes to publish a fix,
// which can be indefinitely, while changing nothing about our exposure.
//
// That trade is deliberate and it has a cost: a serious vulnerability with no
// upstream fix will pass this gate every day without anyone being forced to
// look at it. The report exists so that "nobody was forced to" does not become
// "nobody knew". Acting on those means dropping or replacing the dependency,
// which is a judgment call this tool cannot make.
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

	// FixedVersion is absent, not empty, when no fix has been published. That
	// distinction is the whole basis of the verdict, so it is read through a
	// plain string: absent and empty both decode to "" and both mean "nothing
	// to bump to".
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

	// fixedVersion is set when any reachable finding has one. Taking it from
	// any rather than all is deliberate: an OSV can span modules, and one of
	// them being fixable is enough to make the finding actionable.
	fixedVersion string
}

func (v vulnerability) blocking() bool {
	return v.reachable && v.fixedVersion != ""
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

	if entry.fixedVersion == "" {
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
	var blocking, unfixed []vulnerability

	for _, v := range vulns {
		switch {
		case v.blocking():
			blocking = append(blocking, v)
		case v.reachable:
			unfixed = append(unfixed, v)
		}
	}

	var b strings.Builder

	if len(unfixed) > 0 {
		fmt.Fprintf(&b, "Reachable, no fix available (%d, not blocking):\n\n", len(unfixed))

		for _, v := range unfixed {
			b.WriteString(describe(v))

			if annotate {
				// Annotations surface these on the workflow summary, which is
				// the only reason anyone reads a passing job.
				fmt.Fprintf(&b, "::warning title=%s::%s (%s): no fix available\n", v.id, v.summaryOrID(), v.module)
			}
		}
	}

	if len(blocking) > 0 {
		fmt.Fprintf(&b, "Reachable with a fix available (%d, blocking):\n\n", len(blocking))

		for _, v := range blocking {
			b.WriteString(describe(v))
		}
	} else {
		b.WriteString("vulncheck: no reachable vulnerability has an available fix\n")
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	if len(blocking) == 0 {
		return nil
	}

	return fmt.Errorf("%s reachable and fixable: upgrade the affected module(s)", plural(len(blocking)))
}

func describe(v vulnerability) string {
	var b strings.Builder

	fmt.Fprintf(&b, "  %s  %s\n", v.id, v.summaryOrID())
	fmt.Fprintf(&b, "    module:    %s@%s\n", v.module, v.version)

	if v.fixedVersion != "" {
		fmt.Fprintf(&b, "    fixed in:  %s\n", v.fixedVersion)
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

func plural(n int) string {
	if n == 1 {
		return "1 vulnerability is"
	}

	return fmt.Sprintf("%d vulnerabilities are", n)
}
