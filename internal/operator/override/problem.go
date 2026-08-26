// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"
	"sort"
	"strings"
)

// Problem is something wrong with the overrides payload, carrying enough
// information to work out which workloads it puts in doubt.
//
// The distinction it exists to draw is between a failure whose blast radius is
// known and one whose is not. An entry that fails validation names the
// component, kind and Sites it would have targeted, so only those workloads
// need be withheld; every other workload can reconcile with its own overrides.
// A key that fails to parse names nothing, because the entries could not be
// read at all, so every workload an override could reach is in doubt.
//
// Conflating the two is what made a mis-indented line in one ConfigMap key
// withhold every workload of every component on every Site, and discard the
// entries in the other keys while it was at it.
type Problem struct {
	// Key is the ConfigMap key the problem was found in.
	Key string

	// Source is set for an entry-level problem and nil for a key-level one.
	Source *Source

	// Component, Kind and Sites describe what the offending entry could have
	// targeted, and are only meaningful for an entry-level problem.
	//
	// An empty Component means the entry could never resolve to a workload at
	// all, because resolution matches on component. An empty Kind means every
	// kind the component emits. Nil Sites means every Site.
	Component string
	Kind      string
	Sites     []string

	Err error
}

// KeyLevel reports whether the problem covers a whole ConfigMap key, in which
// case the entries it held are unknown and every overridable workload is in
// doubt.
func (p Problem) KeyLevel() bool { return p.Source == nil }

// String renders a problem for an error message, naming its origin.
func (p Problem) String() string {
	if p.Source != nil {
		return fmt.Sprintf("%s: %v", p.Source, p.Err)
	}

	return fmt.Sprintf("overrides key %q: %v", p.Key, p.Err)
}

// ProblemsError joins problems into one error, in the shape a user reading a
// document sees: every problem, sorted, one per line.
//
// Reporting every problem rather than only the first is deliberate. A user
// fixing a document should see the whole list, not discover the next one on
// each apply.
func ProblemsError(problems []Problem) error {
	if len(problems) == 0 {
		return nil
	}

	rendered := make([]string, 0, len(problems))
	for _, problem := range problems {
		rendered = append(rendered, problem.String())
	}

	sort.Strings(rendered)

	return fmt.Errorf("invalid override document:\n  %s", strings.Join(rendered, "\n  "))
}

// keyProblem builds a problem covering a whole ConfigMap key.
func keyProblem(key string, err error) Problem {
	return Problem{Key: key, Err: err}
}

// entryProblem builds a problem covering one entry, recording what that entry
// could have targeted so the withholding can be scoped to it.
func entryProblem(source Source, entry Entry, err error) Problem {
	return Problem{
		Key:       source.Key,
		Source:    &source,
		Component: entry.Component,
		Kind:      entry.Kind,
		Sites:     entry.Sites,
		Err:       err,
	}
}
