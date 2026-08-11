// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// requiredTermsPath is where required node affinity terms live.
var requiredTermsPath = []string{
	"nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms",
}

// applyAffinity combines operator and user affinity so both hold.
//
// Required node affinity terms are ORed by Kubernetes, so combining two term
// lists is a Cartesian product, not a concatenation and not an append of the
// user's expressions onto each operator term. Given operator terms O and user
// terms U, the required semantics are:
//
//	(O1 OR O2) AND (U1 OR U2)
//	  = (O1 AND U1) OR (O1 AND U2) OR (O2 AND U1) OR (O2 AND U2)
//
// Appending the user's expressions to each operator term is only correct when
// the user supplies exactly one term, which is why it is not what happens here.
//
// This matters concretely: SiteNodeAffinity emits two terms, matching the
// canonical and the deprecated Site label, so every per-Site workload already
// has a two-term operator constraint.
//
// Everything other than required node affinity is concatenated, since it is
// preference or pod affinity rather than a hard constraint the operator relies
// on.
func applyAffinity(workload *unstructured.Unstructured, operator, user *schedulingSet) error {
	combined := combineAffinities(append(append([]map[string]any{}, operator.affinity...), user.affinity...))
	if combined == nil {
		return nil
	}

	if err := setNestedMap(workload.Object, combined, "spec", "template", "spec", "affinity"); err != nil {
		return fmt.Errorf("set affinity: %w", err)
	}

	return nil
}

// combineAffinities folds a list of affinity blocks into one.
func combineAffinities(blocks []map[string]any) map[string]any {
	var (
		combined  map[string]any
		haveTerms bool
		terms     []any
	)

	for _, block := range blocks {
		if len(block) == 0 {
			continue
		}

		blockTerms := nestedSlice(block, requiredTermsPath...)

		if combined == nil {
			combined = deepCopyMap(block)
		} else {
			mergeAffinityExtras(combined, block)
		}

		if blockTerms == nil {
			continue
		}

		if !haveTerms {
			terms = blockTerms
			haveTerms = true

			continue
		}

		terms = cartesianTerms(terms, blockTerms)
	}

	if combined == nil {
		return nil
	}

	if haveTerms {
		if err := setNestedSlice(combined, terms, requiredTermsPath...); err != nil {
			// The terms came out of the same tree, so this cannot fail in
			// practice; returning the uncombined block is still safe.
			return combined
		}
	}

	return combined
}

// mergeAffinityExtras concatenates the affinity sections that are not required
// node affinity: preferences and pod affinity.
func mergeAffinityExtras(into, from map[string]any) {
	for _, section := range []struct {
		path []string
	}{
		{path: []string{"nodeAffinity", "preferredDuringSchedulingIgnoredDuringExecution"}},
		{path: []string{"podAffinity", "requiredDuringSchedulingIgnoredDuringExecution"}},
		{path: []string{"podAffinity", "preferredDuringSchedulingIgnoredDuringExecution"}},
		{path: []string{"podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution"}},
		{path: []string{"podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution"}},
	} {
		addition := nestedSlice(from, section.path...)
		if addition == nil {
			continue
		}

		existing := nestedSlice(into, section.path...)
		if err := setNestedSlice(into, append(existing, addition...), section.path...); err != nil {
			continue
		}
	}
}

// cartesianTerms returns the product of two required-term lists.
//
// Each product term carries the concatenation of both sides' matchExpressions
// and both sides' matchFields. Dropping matchFields would silently discard a
// user constraint, since NodeSelectorTerm carries both.
func cartesianTerms(left, right []any) []any {
	if len(left) == 0 {
		return right
	}

	if len(right) == 0 {
		return left
	}

	product := make([]any, 0, len(left)*len(right))

	for _, l := range left {
		leftTerm, ok := l.(map[string]any)
		if !ok {
			continue
		}

		for _, r := range right {
			rightTerm, ok := r.(map[string]any)
			if !ok {
				continue
			}

			product = append(product, combineTerms(leftTerm, rightTerm))
		}
	}

	if len(product) == 0 {
		return left
	}

	return product
}

func combineTerms(left, right map[string]any) map[string]any {
	combined := map[string]any{}

	for _, field := range []string{"matchExpressions", "matchFields"} {
		leftValues, _ := left[field].([]any)   //nolint:errcheck // absent means no expressions
		rightValues, _ := right[field].([]any) //nolint:errcheck // absent means no expressions

		joined := append(append([]any{}, leftValues...), rightValues...)
		if len(joined) > 0 {
			combined[field] = joined
		}
	}

	return combined
}

// deepCopyMap copies a decoded YAML or JSON map so callers can mutate freely.
func deepCopyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}

	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = deepCopyValue(value)
	}

	return out
}

func deepCopyValue(in any) any {
	switch typed := in.(type) {
	case map[string]any:
		return deepCopyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, element := range typed {
			out[i] = deepCopyValue(element)
		}

		return out
	default:
		return in
	}
}
