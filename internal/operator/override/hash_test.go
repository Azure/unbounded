// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"strings"
	"testing"
)

// TestContributorSourcesIsBounded is a regression test.
//
// Kubernetes caps all annotations on an object at 256 KiB, and a ConfigMap key
// may be 253 characters, so the rendered source list grows several times faster
// than the document that produced it. Left unbounded, enough contributors to
// one workload made every apply of that workload fail with an opaque size
// rejection, from a ConfigMap the apiserver had happily accepted.
func TestContributorSourcesIsBounded(t *testing.T) {
	key := strings.Repeat("k", 253)

	contributors := make([]SourcedEntry, 0, 4000)
	for i := range 4000 {
		contributors = append(contributors, SourcedEntry{Source: Source{Key: key, Index: i}})
	}

	got := contributorSources(contributors)

	if len(got) > maxSourceAnnotation {
		t.Fatalf("rendered %d bytes, over the %d byte budget", len(got), maxSourceAnnotation)
	}

	// It says it was cut, rather than silently describing the wrong set.
	if !strings.Contains(got, "more") {
		t.Fatalf("sources = %q, want it to say how many were omitted", got[max(0, len(got)-120):])
	}
}

// TestContributorSourcesKeepsShortListsIntact confirms the cap does not change
// the ordinary case, where the annotation is the point.
func TestContributorSourcesKeepsShortListsIntact(t *testing.T) {
	got := contributorSources([]SourcedEntry{
		{Source: Source{Key: "a.yaml", Index: 0}},
		{Source: Source{Key: "b.yaml", Index: 2}},
	})

	if got != "a.yaml[0],b.yaml[2]" {
		t.Fatalf("sources = %q", got)
	}
}
