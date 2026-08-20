// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"
	"strings"

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
	combined, err := combineAffinities(append(append([]map[string]any{}, operator.affinity...), user.affinity...))
	if err != nil {
		return err
	}

	if combined == nil {
		return nil
	}

	if err := setNestedMap(workload.Object, combined, "spec", "template", "spec", "affinity"); err != nil {
		return fmt.Errorf("set affinity: %w", err)
	}

	return nil
}

// maxRequiredTerms bounds the Cartesian product of required node affinity
// terms.
//
// The product is multiplicative: three contributors with four terms each
// produce sixty-four, and there is no natural ceiling because any number of
// override documents can target the same workload. An unbounded product is a
// denial of service against the API server and etcd rather than a merely large
// object, and the resulting affinity would be incomprehensible to anyone
// debugging a scheduling failure.
//
// The operator itself contributes at most two terms (SiteNodeAffinity emits
// the canonical and the deprecated Site label), so this leaves room for
// genuinely complex user constraints while staying far below the point where
// the object becomes a problem.
const maxRequiredTerms = 128

// combineAffinities folds a list of affinity blocks into one.
func combineAffinities(blocks []map[string]any) (map[string]any, error) {
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
		} else if err := mergeAffinityExtras(combined, block); err != nil {
			return nil, err
		}

		if blockTerms == nil {
			continue
		}

		if !haveTerms {
			terms = blockTerms
			haveTerms = true

			continue
		}

		next, err := cartesianTerms(terms, blockTerms)
		if err != nil {
			return nil, err
		}

		terms = next
	}

	if combined == nil {
		return nil, nil
	}

	if haveTerms {
		if err := setNestedSlice(combined, terms, requiredTermsPath...); err != nil {
			return nil, fmt.Errorf("set required node affinity terms: %w", err)
		}
	}

	return combined, nil
}

// affinityExtraSections are the affinity sections that are not required node
// affinity: preferences, and pod affinity in both directions. They concatenate
// rather than taking a product, because none of them is a hard constraint the
// operator relies on.
var affinityExtraSections = [][]string{
	{"nodeAffinity", "preferredDuringSchedulingIgnoredDuringExecution"},
	{"podAffinity", "requiredDuringSchedulingIgnoredDuringExecution"},
	{"podAffinity", "preferredDuringSchedulingIgnoredDuringExecution"},
	{"podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution"},
	{"podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution"},
}

// mergeAffinityExtras concatenates the affinity sections that are not required
// node affinity.
//
// A section of the wrong type is an error rather than an omission, and so is a
// write that fails. Both used to be swallowed, and the result was the worst
// shape a failure can take here: the user's constraint disappeared, the merge
// carried on, the override was hashed, and the Site reported Applied.
//
// It was also inconsistent. With no operator affinity to merge into, a single
// user block is copied wholesale and the API server rejects the malformed
// section loudly; with operator affinity present, which is every per-Site
// workload, the same document was silently dropped instead.
func mergeAffinityExtras(into, from map[string]any) error {
	for _, path := range affinityExtraSections {
		value, found, err := unstructured.NestedFieldNoCopy(from, path...)
		if err != nil || !found {
			continue
		}

		addition, ok := value.([]any)
		if !ok {
			return fmt.Errorf("spec.template.spec.affinity.%s must be a list, but holds %T",
				strings.Join(path, "."), value)
		}

		existing := nestedSlice(into, path...)
		if err := setNestedSlice(into, append(existing, addition...), path...); err != nil {
			return fmt.Errorf("combine spec.template.spec.affinity.%s: %w",
				strings.Join(path, "."), err)
		}
	}

	return nil
}

// cartesianTerms returns the product of two required-term lists.
//
// Each product term carries the concatenation of both sides' matchExpressions
// and both sides' matchFields. Dropping matchFields would silently discard a
// user constraint, since NodeSelectorTerm carries both.
// A term list that is empty, or that holds a non-mapping, is an error rather
// than an identity or a silent skip. Treating an empty list as the identity
// would quietly resolve to whichever side was non-empty, which is the opposite
// of the AND the user asked for.
func cartesianTerms(left, right []any) ([]any, error) {
	if len(left) == 0 || len(right) == 0 {
		return nil, fmt.Errorf("required node affinity has an empty nodeSelectorTerms list; "+
			"an empty list matches nothing, so it cannot be combined with the %d term(s) on the other side",
			max(len(left), len(right)))
	}

	if size := len(left) * len(right); size > maxRequiredTerms {
		return nil, fmt.Errorf("combining %d and %d required node affinity terms would produce %d terms, "+
			"more than the limit of %d; required terms are ORed, so combining them is a product rather than "+
			"a concatenation, and fewer, broader terms are needed here",
			len(left), len(right), size, maxRequiredTerms)
	}

	product := make([]any, 0, len(left)*len(right))

	for _, l := range left {
		leftTerm, ok := l.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("required node affinity term must be a mapping, but holds %T", l)
		}

		for _, r := range right {
			rightTerm, ok := r.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("required node affinity term must be a mapping, but holds %T", r)
			}

			term, err := combineTerms(leftTerm, rightTerm)
			if err != nil {
				return nil, err
			}

			product = append(product, term)
		}
	}

	return product, nil
}

// combineTerms merges the expressions of two node selector terms.
//
// A present-but-wrongly-typed list is an error rather than an omission. The
// assertion used to be discarded, which silently dropped the user's constraint;
// and if every field of a term was malformed the product term came out empty,
// which matches every node, so a constraint meant to narrow scheduling widened
// it instead. Validation rejects this shape, so reaching the error here means
// validation was bypassed.
func combineTerms(left, right map[string]any) (map[string]any, error) {
	combined := map[string]any{}

	for _, field := range []string{"matchExpressions", "matchFields"} {
		var joined []any

		for _, side := range []map[string]any{left, right} {
			value, present := side[field]
			if !present {
				continue
			}

			values, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf(
					"required node affinity term has %s of type %T, want a list", field, value,
				)
			}

			joined = append(joined, values...)
		}

		if len(joined) > 0 {
			combined[field] = joined
		}
	}

	if len(combined) == 0 {
		return nil, fmt.Errorf(
			"combining required node affinity terms produced a term with no constraints, " +
				"which matches every node; this would widen scheduling rather than narrow it",
		)
	}

	return combined, nil
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
