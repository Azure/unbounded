// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// deadcode-report folds several `deadcode -json` runs into one report.
//
// x/tools/cmd/deadcode answers "which functions are unreachable from a main
// function", and its own documentation is explicit that the answer is valid
// for exactly one GOOS/GOARCH/-tags configuration. This tree builds under
// `e2e`, `integrationtest` and `storageboundary` as well as the default set,
// so a single run reports tag-guarded code as dead. Running it once per
// configuration and intersecting the results is what the tool's documentation
// recommends, and it is what this program does.
//
// It also folds in the second axis, -test:
//
//   - Without -test the roots are the 31 main packages. Anything reachable
//     only from a test reads as dead.
//   - With -test the roots include every test binary, so test-only code reads
//     as live.
//
// Both answers matter and they answer different questions, so the report has
// two sections. "Unreachable" is dead from every root in every configuration
// and is a deletion candidate. "Test-only" is reachable from tests and nothing
// else, which is the shape a function takes when its production caller is
// deleted and its test is not. Writing a test for dead code moves it from the
// first section to the second, so the second section is not noise to be
// silenced by adding coverage.
//
// This is a report, not a gate. It exits non-zero only when it could not
// produce an answer: a missing file, malformed JSON, or an input that decoded
// to nothing at all, which is what a truncated scan looks like. See
// hack/cmd/vulncheck-gate for the same contract applied to govulncheck.
//
// RTA is sound with respect to reflection, which means it will not call a
// function dead when reflection could reach it. The cost is that converting a
// value to an interface marks every exported method of its type reachable
// (x/tools/go/callgraph/rta/rta.go, addRuntimeType). Dead exported methods on
// a type that is ever boxed are therefore invisible here. hack/cmd/unreferenced
// covers that case; neither tool subsumes the other.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// deadcodePackage is one element of the array emitted by `deadcode -json`.
type deadcodePackage struct {
	Name  string         `json:"Name"`
	Path  string         `json:"Path"`
	Funcs []deadcodeFunc `json:"Funcs"`
}

type deadcodeFunc struct {
	Name      string   `json:"Name"`
	Position  position `json:"Position"`
	Generated bool     `json:"Generated"`
	Marker    bool     `json:"Marker"`
}

type position struct {
	File string `json:"File"`
	Line int    `json:"Line"`
	Col  int    `json:"Col"`
}

// function identifies one dead function across runs.
//
// The package path and the function name are the identity; the position is
// carried along for the report but deliberately kept out of the key, because
// an unrelated edit above a function shifts its line and would otherwise break
// the intersection between two runs of the same tree.
type function struct {
	pkg  string
	name string
}

type finding struct {
	function
	pos position
}

// errEmptyInput is the failure mode that would silently empty the report.
//
// A truncated or zero-length input decodes to an empty array, which is
// indistinguishable from "the whole tree is reachable". deadcode always emits
// at least one package for this tree, so an empty decode means the run failed.
var errEmptyInput = errors.New("decoded to zero packages: the scan did not run, or its output was truncated")

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "deadcode-report: %v\n", err)
		os.Exit(1)
	}
}

// fileList collects a repeatable flag.
type fileList []string

func (f *fileList) String() string { return strings.Join(*f, ",") }

func (f *fileList) Set(v string) error {
	if v == "" {
		return errors.New("empty path")
	}

	*f = append(*f, v)

	return nil
}

func run(args []string, stdout io.Writer) error {
	var withTests, withoutTests fileList

	flags := flag.NewFlagSet("deadcode-report", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Var(&withTests, "with-tests", "`file` holding the JSON from one `deadcode -test` run (repeatable, one per tag configuration)")
	flags.Var(&withoutTests, "without-tests", "`file` holding the JSON from one `deadcode` run without -test (repeatable, one per tag configuration)")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if len(withTests) == 0 {
		return errors.New("at least one -with-tests file is required")
	}

	unreachable, err := intersect(withTests)
	if err != nil {
		return err
	}

	// Without -test inputs the second section cannot be computed, and an empty
	// section would read as "there is no test-only dead code" rather than "the
	// question was not asked". Leaving it nil distinguishes the two.
	var testOnly map[function]finding

	if len(withoutTests) > 0 {
		deadWithoutTests, err := intersect(withoutTests)
		if err != nil {
			return err
		}

		testOnly = make(map[function]finding, len(deadWithoutTests))

		for key, f := range deadWithoutTests {
			if _, alsoUnreachable := unreachable[key]; !alsoUnreachable {
				testOnly[key] = f
			}
		}
	}

	return report(stdout, unreachable, testOnly)
}

// intersect returns the findings present in every input.
//
// A function has to be dead in all of them to survive: dead under one tag
// configuration and live under another means live.
func intersect(files []string) (map[function]finding, error) {
	var acc map[function]finding

	for _, name := range files {
		current, err := load(name)
		if err != nil {
			return nil, err
		}

		if acc == nil {
			acc = current

			continue
		}

		for key := range acc {
			if _, ok := current[key]; !ok {
				delete(acc, key)
			}
		}
	}

	return acc, nil
}

func load(name string) (map[function]finding, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}

	var pkgs []deadcodePackage

	if err := json.Unmarshal(raw, &pkgs); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}

	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%s: %w", name, errEmptyInput)
	}

	out := make(map[function]finding)

	for _, pkg := range pkgs {
		for _, fn := range pkg.Funcs {
			// deadcode already excludes generated files and marker interface
			// methods by default. The fields are read and honored anyway so
			// that passing -generated to the tool does not quietly turn
			// generated code into deletion candidates here.
			if fn.Generated || fn.Marker {
				continue
			}

			key := function{pkg: pkg.Path, name: fn.Name}
			out[key] = finding{function: key, pos: fn.Position}
		}
	}

	return out, nil
}

func report(w io.Writer, unreachable, testOnly map[function]finding) error {
	var b strings.Builder

	b.WriteString("deadcode: functions unreachable from any main package, in every tag configuration scanned.\n")
	b.WriteString("A dead method may still be needed to satisfy an interface. Some judgment is required.\n\n")

	writeSection(&b, "Unreachable from any binary or test", unreachable)

	if testOnly != nil {
		b.WriteString("\n")
		writeSection(&b, "Reachable only from tests", testOnly)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

func writeSection(b *strings.Builder, title string, findings map[function]finding) {
	if len(findings) == 0 {
		fmt.Fprintf(b, "%s: none\n", title)

		return
	}

	fmt.Fprintf(b, "%s (%d):\n\n", title, len(findings))

	byPackage := map[string][]finding{}
	for _, f := range findings {
		byPackage[f.pkg] = append(byPackage[f.pkg], f)
	}

	pkgs := make([]string, 0, len(byPackage))
	for pkg := range byPackage {
		pkgs = append(pkgs, pkg)
	}

	sort.Strings(pkgs)

	for _, pkg := range pkgs {
		in := byPackage[pkg]
		sort.Slice(in, func(i, j int) bool {
			if in[i].pos.File != in[j].pos.File {
				return in[i].pos.File < in[j].pos.File
			}

			return in[i].pos.Line < in[j].pos.Line
		})

		fmt.Fprintf(b, "  %s (%d)\n", pkg, len(in))

		for _, f := range in {
			fmt.Fprintf(b, "    %s:%d: %s\n", path.Clean(f.pos.File), f.pos.Line, f.name)
		}

		b.WriteString("\n")
	}
}
