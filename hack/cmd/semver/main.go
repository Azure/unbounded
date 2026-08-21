// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command semver answers semantic version questions that shell cannot answer
// correctly.
//
// The release tooling is otherwise shell, and deliberately so, but `sort -V`
// does not implement semantic version precedence. It orders a final release
// BELOW its own release candidates:
//
//	$ printf 'v1.0.0\nv1.0.0-rc.1\n' | sort -V
//	v1.0.0
//	v1.0.0-rc.1
//
// Semver 2.0.0 clause 11.3 requires the opposite: a pre-release has lower
// precedence than the associated normal version. Nothing in next-version.sh is
// affected, because it filters to finals before sorting and strips `-rc.N`
// before comparing cores, but the release soak has to compare an arbitrary
// incoming tag against the highest release, and that tag is routinely a
// candidate. Getting it backwards there deploys a superseded candidate to the
// soak cluster.
//
// Tags arrive on stdin rather than being read from git, so the tests need no
// repository fixture and the caller decides what set of tags the question is
// being asked about.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/mod/semver"
)

// errUsage is returned for anything the caller got wrong about invocation, as
// opposed to a legitimate false answer.
var errUsage = errors.New("usage")

const usage = `semver answers semantic version questions for the release tooling.

Usage:
  semver compare <a> <b>            print -1, 0 or 1 for a against b
  semver is-maintenance <tag>       read tags on stdin, print true or false

is-maintenance reports whether <tag> is a maintenance release: a release that
is not the newest, and so must not be deployed to the soak cluster because
doing so would downgrade it.

  git tag --list 'v*' | semver is-maintenance v1.4.1
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
		}

		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no subcommand", errUsage)
	}

	switch args[0] {
	case "compare":
		if len(args) != 3 {
			return fmt.Errorf("%w: compare takes two versions, got %d", errUsage, len(args)-1)
		}

		return compare(args[1], args[2], stdout)

	case "is-maintenance":
		if len(args) != 2 {
			return fmt.Errorf("%w: is-maintenance takes one tag, got %d", errUsage, len(args)-1)
		}

		return isMaintenance(args[1], stdin, stdout, stderr)

	default:
		return fmt.Errorf("%w: unknown subcommand %q", errUsage, args[0])
	}
}

// compare prints the precedence of a against b as -1, 0 or 1.
func compare(a, b string, stdout io.Writer) error {
	if err := validate(a); err != nil {
		return err
	}

	if err := validate(b); err != nil {
		return err
	}

	_, err := fmt.Fprintln(stdout, semver.Compare(a, b))

	return err
}

// isMaintenance prints whether tag is a maintenance release, given the tags on
// stdin.
//
// A tag is a maintenance release when it ranks strictly below the highest
// release in the set. Equality is deliberately not maintenance: re-running the
// soak for the release that is already current is a supported operation
// (RELEASING.md documents dispatching it by hand for backfills), and treating
// it as maintenance would silently skip the deploy it was asked for.
func isMaintenance(tag string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := validate(tag); err != nil {
		return err
	}

	tags, err := readTags(stdin)
	if err != nil {
		return err
	}

	if len(tags) == 0 {
		// Distinguished from "no finals among the tags" below. An entirely
		// empty stream means the caller's `git tag` produced nothing, which is
		// a broken invocation rather than an answer, and answering "false"
		// would let a release deploy on the strength of a failed command.
		return errors.New("no tags on stdin; expected the output of `git tag --list`")
	}

	highest := highestFinal(tags)
	if highest == "" {
		// Tags exist but none is a final release. Nothing has shipped yet, so
		// nothing can be older than it.
		//
		// Diagnostics are deliberately unchecked throughout: failing to write
		// an explanation is not worth failing a release over, whereas failing
		// to write the answer is, so only that write is checked.
		fmt.Fprintln(stderr, "no final release tags found; treating as not a maintenance release") //nolint:errcheck // diagnostic

		return answer(false, stdout)
	}

	result := semver.Compare(tag, highest) < 0

	fmt.Fprintf(stderr, "highest release: %s; %s is %sa maintenance release\n", //nolint:errcheck // diagnostic
		highest, tag, map[bool]string{true: "", false: "not "}[result])

	return answer(result, stdout)
}

// answer writes the decision, which is the command's entire contract with its
// caller and so is the one write whose failure matters.
func answer(result bool, stdout io.Writer) error {
	if _, err := fmt.Fprintln(stdout, result); err != nil {
		return fmt.Errorf("writing result: %w", err)
	}

	return nil
}

// readTags reads one tag per line, ignoring blanks and surrounding whitespace.
func readTags(stdin io.Reader) ([]string, error) {
	var tags []string

	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		tags = append(tags, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading tags: %w", err)
	}

	return tags, nil
}

// highestFinal returns the highest final release among tags, or "" when there
// is none.
//
// Only finals count. A candidate for a version that has not shipped is not
// something a maintenance release can be older than, and including it would
// make an unreleased train suppress the soak for everything below it.
//
// Anything that is not a strictly canonical vX.Y.Z is skipped rather than
// rejected: this repository's tag namespace contains genuine debris, such as
// v0.1.23-alpha.0 and a v0.1.24 train abandoned at rc.18, and a stray malformed
// tag must not be able to break every subsequent release.
func highestFinal(tags []string) string {
	highest := ""

	for _, tag := range tags {
		if validate(tag) != nil {
			continue
		}

		if semver.Prerelease(tag) != "" {
			continue
		}

		if highest == "" || semver.Compare(tag, highest) > 0 {
			highest = tag
		}
	}

	return highest
}

// validate accepts only a strictly canonical version.
//
// semver.IsValid alone is too permissive here. golang.org/x/mod/semver
// documents two deviations from the spec: it requires the leading "v", which
// suits us because our tags carry it, and it accepts vMAJOR and vMAJOR.MINOR as
// shorthands for vMAJOR.0.0 and vMAJOR.MINOR.0. A tag named "v1" would
// therefore be read as v1.0.0 and could outrank a real release.
//
// Comparing against Canonical rejects those shorthands, and rejects build
// metadata too, which we do not use and which the spec requires be ignored in
// precedence, so a tag carrying it could never be ordered reliably.
func validate(v string) error {
	if !semver.IsValid(v) {
		return fmt.Errorf("not a semantic version: %q", v)
	}

	if semver.Canonical(v) != v {
		return fmt.Errorf("not a canonical vX.Y.Z version: %q (canonical form is %q)", v, semver.Canonical(v))
	}

	return nil
}
