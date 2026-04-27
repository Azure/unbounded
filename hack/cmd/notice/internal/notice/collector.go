// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package notice

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/license"
)

// Collector enumerates one ecosystem's direct dependencies and produces
// NOTICE entries. Implementations live under
// hack/cmd/notice/internal/<ecosystem>/.
//
// Adding a new ecosystem is three steps:
//  1. Create internal/<name>/ with a Collector implementation.
//  2. Write hermetic tests using internal/testutil for fixtures.
//  3. Append <name>.New() to the explicit collectors slice in main.go.
//
// Collect MUST NOT sort its result; Build performs a global sort by
// Dependency across all collectors.
type Collector interface {
	// Name is a stable identifier used in the Entry.Ecosystem field and in
	// error messages. Examples: "go", "npm".
	Name() string

	// Precheck verifies the host has what the collector needs (e.g. an
	// installed `go` toolchain, a populated `node_modules`). The returned
	// error message is shown verbatim to the user.
	Precheck(root string) error

	// Collect returns one Entry per direct dependency.
	Collect(root string) ([]Entry, error)
}

// AssembleEntry builds a NOTICE entry by reading the LICENSE file in dir,
// classifying it, extracting copyright lines, and rendering one License per
// classified license name pointing at the canonical blob URL.
//
// declaredLicense is an optional ecosystem-supplied SPDX identifier (e.g.
// from npm's package.json "license" field). When non-empty, its friendly
// form is prepended to the classifier output if the classifier did not
// already emit it - this is the npm-recommended source of truth for
// ambiguous license texts.
//
// Ecosystems compute (repoBase, ref) however they need to and pass them in.
func AssembleEntry(name, ecosystem, dir, repoBase, ref, declaredLicense string) (Entry, error) {
	licensePath, err := license.FindFile(dir)
	if err != nil {
		return Entry{}, err
	}

	licenseText, err := os.ReadFile(licensePath)
	if err != nil {
		return Entry{}, fmt.Errorf("reading %s: %w", licensePath, err)
	}

	licNames, err := license.Classify(licenseText)
	if err != nil {
		return Entry{}, fmt.Errorf("classifying %s: %w", licensePath, err)
	}

	if declaredLicense != "" {
		if mapped := license.SPDXFriendly(declaredLicense); mapped != "" {
			if !slices.Contains(licNames, mapped) {
				licNames = append([]string{mapped}, licNames...)
			}
		}
	}

	copyright, err := license.ExtractCopyrightFromDir(dir, licenseText)
	if err != nil {
		return Entry{}, fmt.Errorf("extracting copyright from %s: %w", licensePath, err)
	}

	link, err := license.BuildURL(repoBase, ref, filepath.Base(licensePath))
	if err != nil {
		return Entry{}, fmt.Errorf("deriving license URL: %w", err)
	}

	licList := make([]License, 0, len(licNames))
	for _, n := range licNames {
		licList = append(licList, License{Name: n, Link: link})
	}

	return Entry{
		Dependency: name,
		Ecosystem:  ecosystem,
		Copyright:  copyright,
		License:    licList,
	}, nil
}
