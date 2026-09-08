// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"fmt"
	"path"
	"strings"
	"testing"
)

// fakeRepo answers resolution questions from a synthetic history.
//
// The permanent suite runs against this rather than a real repository, for
// three reasons. It makes Live, Stale and LatestFinal assertable without
// scraping output. It removes any dependence on the developer's git config.
// And it drops the cost of 76 fixtures from seconds to milliseconds.
//
// The risk it introduces is that the fake and real git could disagree, which
// would make the whole suite a test of the fake. git_test.go closes that with a
// conformance test asserting both answer identically for the same tag specs.
type fakeRepo struct {
	// head is the commit HEAD points at.
	head string
	// ancestors are the commits reachable from head, head included.
	ancestors map[string]bool
	// commits maps a tag to the commit it names.
	commits map[string]string
	// order is tag creation order, so listings are deterministic.
	order []string
	// subjects maps a commit to its subject line.
	subjects map[string]string
	// depth maps a commit to its distance from the root, for CountCommits.
	depth map[string]int
}

// newFakeRepo builds a history from the same tag specs the git fixture uses.
//
// A bare tag sits on the current commit. `<tag>@new` advances HEAD first, so a
// fixture can express a train whose candidates are behind HEAD. `<tag>@off`
// places the tag on a commit that is not an ancestor of HEAD, as a tag cut on
// someone else's branch would be.
func newFakeRepo(tags []string) *fakeRepo {
	r := &fakeRepo{
		ancestors: map[string]bool{},
		commits:   map[string]string{},
		subjects:  map[string]string{},
		depth:     map[string]int{},
	}

	r.head = "commit0000000000000000000000000000000000"
	r.ancestors[r.head] = true
	r.subjects[r.head] = "base"
	r.depth[r.head] = 0

	next := 1
	side := 0

	for _, spec := range tags {
		tag := spec

		switch {
		case strings.HasSuffix(spec, "@new"):
			tag = strings.TrimSuffix(spec, "@new")

			commit := fmt.Sprintf("commit%034d", next)
			next++

			r.depth[commit] = r.depth[r.head] + 1
			r.head = commit
			r.ancestors[commit] = true
			r.subjects[commit] = "work before " + tag

		case strings.HasSuffix(spec, "@off"):
			tag = strings.TrimSuffix(spec, "@off")

			commit := fmt.Sprintf("offside%033d", side)
			side++

			// Deliberately not added to ancestors: that is what makes it
			// invisible to reachability-scoped discovery.
			r.subjects[commit] = "off-branch work for " + tag
			r.depth[commit] = 0
			r.commits[tag] = commit
			r.order = append(r.order, tag)

			continue
		}

		r.commits[tag] = r.head
		r.order = append(r.order, tag)
	}

	return r
}

// ReachableTags lists tags matching a glob whose commit is an ancestor of HEAD.
func (r *fakeRepo) ReachableTags(pattern string) ([]string, error) {
	var out []string

	for _, tag := range r.order {
		if !r.ancestors[r.commits[tag]] {
			continue
		}

		// path.Match is fnmatch-like, which is what git --list uses. The
		// conformance test in git_test.go is what says "like" is close enough
		// for the patterns this resolver actually issues.
		ok, err := path.Match(pattern, tag)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
		}

		if ok {
			out = append(out, tag)
		}
	}

	return out, nil
}

// AllTags lists tags matching a glob anywhere, reachable or not.
func (r *fakeRepo) AllTags(pattern string) ([]string, error) {
	var out []string

	for _, tag := range r.order {
		ok, err := path.Match(pattern, tag)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
		}

		if ok {
			out = append(out, tag)
		}
	}

	return out, nil
}

// TagExists reports whether a tag exists anywhere, reachable or not.
func (r *fakeRepo) TagExists(tag string) (bool, error) {
	_, ok := r.commits[tag]

	return ok, nil
}

// Head returns the commit HEAD points at.
func (r *fakeRepo) Head() (string, error) { return r.head, nil }

// CommitOf resolves a tag to its commit.
func (r *fakeRepo) CommitOf(tag string) (string, error) {
	commit, ok := r.commits[tag]
	if !ok {
		return "", fmt.Errorf("unknown revision %s", tag)
	}

	return commit, nil
}

// IsAncestor reports whether a commit is an ancestor of HEAD.
func (r *fakeRepo) IsAncestor(commit string) (bool, error) {
	return r.ancestors[commit], nil
}

// CountCommits counts commits in from..HEAD.
func (r *fakeRepo) CountCommits(from string) (int, error) {
	depth, ok := r.depth[from]
	if !ok {
		return 0, fmt.Errorf("unknown revision %s", from)
	}

	return r.depth[r.head] - depth, nil
}

// Subject returns a commit's subject line.
func (r *fakeRepo) Subject(commit string) (string, error) {
	subject, ok := r.subjects[commit]
	if !ok {
		return "", fmt.Errorf("unknown revision %s", commit)
	}

	return subject, nil
}

// resolveTagSpec maps a fixture's tag spec to the tag it creates, so a case can
// name the commit it expects without knowing the fake's numbering.
func resolveTagSpec(t *testing.T, r *fakeRepo, spec string) string {
	t.Helper()

	if spec == "HEAD" {
		return r.head
	}

	commit, err := r.CommitOf(spec)
	if err != nil {
		t.Fatalf("resolve %q: %v", spec, err)
	}

	return commit
}
