// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package manifesttest stops tests from running against manifests that were
// never rendered.
//
// The deploy/*/rendered directories are embedded with go:embed, but they are
// gitignored output of the make *-manifests targets. On a fresh clone each one
// holds only its .gitignore placeholder, so the embedded filesystem is empty,
// and tests that read it fail with unrelated errors, crash the test binary, or
// pass without checking anything. `make test` renders every set first; plain
// `go test`, `go test -run` and IDE test runners do not.
//
// A package whose tests read rendered manifests calls Main from TestMain, so it
// fails once, before any test runs, and names the make targets to run.
package manifesttest

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	tokenrefreshermanifests "github.com/Azure/unbounded/deploy/token-refresher"
	operatormanifests "github.com/Azure/unbounded/deploy/unbounded-operator"
)

// Set is one embedded directory of rendered manifests.
type Set struct {
	// Dir is the directory's path in the repository.
	Dir string
	// Target is the make target that renders it.
	Target string
	// FS is the embedded directory.
	FS fs.FS
}

// The rendered sets that tests read.
var (
	Gantry = Set{
		Dir: "deploy/gantry/rendered", Target: "gantry-manifests", FS: gantrymanifests.Manifests,
	}
	Machina = Set{
		Dir: "deploy/machina/rendered", Target: "machina-manifests", FS: machinamanifests.Manifests,
	}
	Net = Set{
		Dir: "deploy/net/rendered", Target: "net-manifests", FS: netmanifests.Manifests,
	}
	TokenRefresher = Set{
		Dir: "deploy/token-refresher/rendered", Target: "token-refresher-manifests", FS: tokenrefreshermanifests.Manifests,
	}
	UnboundedOperator = Set{
		Dir: "deploy/unbounded-operator/rendered", Target: "unbounded-operator-manifests", FS: operatormanifests.Manifests,
	}
)

// Main runs the package's tests only when every set is rendered. Otherwise it
// explains what to render and returns a failing exit code without running any
// test. Use it as the whole body of TestMain:
//
//	func TestMain(m *testing.M) {
//		os.Exit(manifesttest.Main(m, manifesttest.Machina))
//	}
func Main(m *testing.M, sets ...Set) int {
	code, message := run(m, sets)
	if message != "" {
		fmt.Fprint(os.Stderr, message)
	}

	return code
}

// runner is the part of *testing.M that run uses.
type runner interface {
	Run() int
}

// run returns the exit code, and the message to print when the tests did not
// run.
func run(m runner, sets []Set) (int, string) {
	missing, err := Unrendered(sets...)
	if err != nil {
		return 1, fmt.Sprintf("check rendered manifests: %v\n", err)
	}

	if len(missing) > 0 {
		return 1, Explain(missing)
	}

	return m.Run(), ""
}

// Unrendered returns the sets that hold no YAML manifests.
func Unrendered(sets ...Set) ([]Set, error) {
	var missing []Set

	for _, set := range sets {
		rendered, err := hasManifests(set.FS)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", set.Dir, err)
		}

		if !rendered {
			missing = append(missing, set)
		}
	}

	return missing, nil
}

// Explain says which sets are missing and the make command that renders them.
func Explain(missing []Set) string {
	var b strings.Builder

	b.WriteString("rendered manifests are missing, so no test in this package can run:\n")

	targets := make([]string, 0, len(missing))
	for _, set := range missing {
		fmt.Fprintf(&b, "  %s has no manifests\n", set.Dir)

		targets = append(targets, set.Target)
	}

	fmt.Fprintf(&b, "run `make %s`, or `make test`, which renders every set before testing\n",
		strings.Join(targets, " "))

	return b.String()
}

// hasManifests reports whether fsys holds a YAML file at any depth, the same
// files the operator and the kubectl plugin apply.
func hasManifests(fsys fs.FS) (bool, error) {
	found := false

	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		switch strings.ToLower(path.Ext(name)) {
		case ".yaml", ".yml":
			found = true

			return fs.SkipAll
		}

		return nil
	})

	return found, err
}
