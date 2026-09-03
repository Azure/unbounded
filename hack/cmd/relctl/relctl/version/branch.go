// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package version resolves what a branch is allowed to release, and what the
// next tag on it should be.
//
// This is the code that decides version numbers. Everything here is a pure
// function of a branch name and a set of tags, so it can be tested without a
// repository and without a network.
package version

import (
	"fmt"
	"regexp"
)

// Bump is how much of the version advances.
type Bump string

// The three bumps. There is no "none": a release always advances something.
const (
	BumpPatch Bump = "patch"
	BumpMinor Bump = "minor"
	BumpMajor Bump = "major"
)

// Policy is what a branch may cut.
type Policy struct {
	// Bump is the component that advances.
	Bump Bump
	// Series is the X.Y a release branch is confined to, empty on main.
	Series string
}

// releaseBranch matches release-X.Y.
//
// Anchored, and no leading zeros, matching the component rule used for tags. A
// branch named release-01.2 would otherwise imply a series that can never match
// a tag, since v01.2.3 is not a version. The nine-digit ceiling keeps a
// pathological name from being treated as a series at all.
var releaseBranch = regexp.MustCompile(`^release-(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// ForBranch decides what branch may release, and how much it may advance.
//
// The versioning rule (#627):
//
//	main          cuts vX.Y.0  - a minor, or a major when explicitly asked
//	release-X.Y   cuts vX.Y.Z  - a patch, and nothing else
//
// Every minor's patch space therefore belongs to exactly one branch, and main
// never enters it. That is what makes a release branch possible at all: without
// it, main and release-0.2 would both compute v0.2.5 and compete for the same
// tag, which pre-1.0 they would do for months, since a minor series here runs
// twenty-odd releases.
//
// major is never derived. A major is always a deliberate human decision, and
// deriving one from commit contents would make it an accident waiting for the
// right commit message.
func ForBranch(branch string, major bool) (Policy, error) {
	if branch == "" {
		return Policy{}, fmt.Errorf("no branch given; expected main or release-X.Y")
	}

	if branch == "main" {
		if major {
			return Policy{Bump: BumpMajor}, nil
		}

		return Policy{Bump: BumpMinor}, nil
	}

	if match := releaseBranch.FindStringSubmatch(branch); match != nil {
		// A release branch exists to patch a series that has already shipped.
		// Letting it cut a minor would escape the series it is named for and
		// collide with main, which is the exact failure this rule prevents.
		if major {
			return Policy{}, fmt.Errorf("major is only valid on main; %s cuts patches", branch)
		}

		return Policy{Bump: BumpPatch, Series: match[1] + "." + match[2]}, nil
	}

	return Policy{}, fmt.Errorf("refusing to release from %s; expected main or release-X.Y", branch)
}
