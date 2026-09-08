// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"fmt"
	"strings"
	"testing"
)

// TestCombineAffinitiesBoundsTheProduct checks that the Cartesian product of
// required terms cannot be used to build an arbitrarily large object.
//
// The product is multiplicative and any number of documents can target one
// workload, so without a ceiling a handful of modest-looking overrides
// compounds into something that is a denial of service against etcd rather
// than merely a large object.
func TestCombineAffinitiesBoundsTheProduct(t *testing.T) {
	block := func(count int) map[string]any {
		terms := make([]any, 0, count)
		for i := range count {
			terms = append(terms, map[string]any{
				"matchExpressions": []any{map[string]any{
					"key":      fmt.Sprintf("example.com/k%d", i),
					"operator": "Exists",
				}},
			})
		}

		return map[string]any{"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": terms},
		}}
	}

	// Just at the limit is accepted, so the ceiling is not merely a low cap
	// that rejects reasonable input.
	combined, err := combineAffinities([]map[string]any{block(2), block(64)})
	if err != nil {
		t.Fatalf("a product of exactly the limit must be accepted: %v", err)
	}

	if got := len(nestedSlice(combined, requiredTermsPath...)); got != maxRequiredTerms {
		t.Fatalf("terms = %d, want %d", got, maxRequiredTerms)
	}

	// One term more is refused.
	if _, err := combineAffinities([]map[string]any{block(3), block(64)}); err == nil {
		t.Fatal("a product over the limit must be refused")
	}

	// Compounding across many contributors is caught as well, since that is
	// how it happens in practice rather than in one document.
	blocks := make([]map[string]any, 0, 8)
	for range 8 {
		blocks = append(blocks, block(4))
	}

	if _, err := combineAffinities(blocks); err == nil {
		t.Fatal("a product compounded over many contributors must be refused")
	}
}

// TestCartesianTermsRejectsDegenerateInput covers the two inputs that have no
// meaningful product. An empty list was previously treated as the identity,
// which quietly resolved to whichever side was non-empty: the user's affinity
// would be accepted and reported Applied while the operator's own constraint
// remained the entire result.
func TestCartesianTermsRejectsDegenerateInput(t *testing.T) {
	term := []any{map[string]any{"matchExpressions": []any{}}}

	for name, args := range map[string][2][]any{
		"empty left":   {{}, term},
		"empty right":  {term, {}},
		"non-map term": {term, {"not a mapping"}},
		"both empty":   {{}, {}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cartesianTerms(args[0], args[1]); err == nil {
				t.Fatal("a degenerate term list must be an error, not an identity")
			}
		})
	}
}

// TestValidateRejectsEmptyNodeSelectorTerms gives the user the same feedback at
// validation time, before anything is applied.
func TestValidateRejectsEmptyNodeSelectorTerms(t *testing.T) {
	err := validateFragment(t, `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        affinity:
          nodeAffinity:
            requiredDuringSchedulingIgnoredDuringExecution:
              nodeSelectorTerms: []
`)
	if err == nil {
		t.Fatal("an empty nodeSelectorTerms list must be rejected")
	}

	if !strings.Contains(err.Error(), "matches nothing") {
		t.Fatalf("error = %q, want it to explain what an empty list means", err)
	}
}

// TestCombineAffinitiesRejectsMalformedExtras is a regression test.
//
// Everything other than required node affinity was concatenated by reading it
// with a helper that returns nil for both "absent" and "present but the wrong
// type", and the write that followed discarded its error. A preference written
// as a mapping rather than a list therefore vanished: the user's constraint was
// dropped, the merge carried on, the override was hashed, and the Site reported
// Applied.
//
// It was also inconsistent, which is what made it hard to spot. With no
// operator affinity to merge into, the single user block is copied wholesale
// and the API server rejects it loudly. With operator affinity present, which
// is every per-Site workload, the same document was silently dropped.
func TestCombineAffinitiesRejectsMalformedExtras(t *testing.T) {
	operator := map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{
					map[string]any{"matchExpressions": []any{
						map[string]any{"key": "site", "operator": "In", "values": []any{"edge"}},
					}},
				},
			},
		},
	}

	for _, tc := range []struct {
		name    string
		section string
		parent  string
	}{
		{name: "node preference", parent: "nodeAffinity", section: "preferredDuringSchedulingIgnoredDuringExecution"},
		{name: "pod affinity", parent: "podAffinity", section: "requiredDuringSchedulingIgnoredDuringExecution"},
		{name: "pod anti-affinity", parent: "podAntiAffinity", section: "preferredDuringSchedulingIgnoredDuringExecution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := map[string]any{
				// A mapping where Kubernetes requires a list: one missing "-".
				tc.parent: map[string]any{tc.section: map[string]any{"weight": int64(1)}},
			}

			_, err := combineAffinities([]map[string]any{operator, user})
			if err == nil {
				t.Fatal("a malformed affinity section must be rejected, not dropped")
			}

			if !strings.Contains(err.Error(), tc.section) {
				t.Fatalf("error = %v, want it to name %s", err, tc.section)
			}
		})
	}
}

// TestCombineAffinitiesConcatenatesExtras pins that the well-formed case still
// appends rather than replacing, so the operator's own preferences survive.
func TestCombineAffinitiesConcatenatesExtras(t *testing.T) {
	block := func(weight int64) map[string]any {
		return map[string]any{
			"nodeAffinity": map[string]any{
				"preferredDuringSchedulingIgnoredDuringExecution": []any{
					map[string]any{"weight": weight, "preference": map[string]any{}},
				},
			},
		}
	}

	combined, err := combineAffinities([]map[string]any{block(1), block(2)})
	if err != nil {
		t.Fatalf("combineAffinities: %v", err)
	}

	preferences := nestedSlice(combined, "nodeAffinity", "preferredDuringSchedulingIgnoredDuringExecution")
	if len(preferences) != 2 {
		t.Fatalf("preferences = %v, want both contributors' terms", preferences)
	}
}
