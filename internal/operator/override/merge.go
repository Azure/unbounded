// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// schedulingPaths are pulled out of a patch before the strategic merge and
// combined additively afterwards.
var schedulingPaths = [][]string{
	{"spec", "template", "spec", "affinity"},
	{"spec", "template", "spec", "tolerations"},
	{"spec", "template", "spec", "nodeSelector"},
}

// merge applies a workload's contributors to it, in place.
//
// The order matters and is fixed:
//
//  1. capture the operator's identity and scheduling from the workload
//  2. strategic-merge every contributor's patch, minus scheduling
//  3. combine scheduling additively, so operator constraints survive
//  4. append extraArgs
//  5. re-stamp identity, so correctness does not depend on validation being
//     exhaustive
//
// Step 5 is what makes the group, version and kind unbreakable rather than
// merely rejected, which matters because apply is GVK-directed and the operator
// holds escalate and bind on ClusterRoleBindings.
func merge(workload *unstructured.Unstructured, contributors []SourcedEntry) error {
	identity := captureIdentity(workload)

	operatorScheduling := captureScheduling(workload)
	userScheduling := newSchedulingSet()

	for _, contributor := range contributors {
		patch := deepCopyMap(contributor.Entry.Patch)
		userScheduling.absorb(patch)

		if len(patch) == 0 {
			continue
		}

		merged, err := strategicMerge(workload.Object, patch, identity.kind)
		if err != nil {
			return fmt.Errorf("%s: %w", contributor.Source, err)
		}

		workload.Object = merged
	}

	if err := applyScheduling(workload, operatorScheduling, userScheduling); err != nil {
		return err
	}

	if err := applyExtraArgs(workload, contributors); err != nil {
		return err
	}

	return restoreIdentity(workload, identity)
}

// strategicMerge merges a patch using the schema of the workload's own type, so
// containers merge by name, env by name and volumeMounts by mountPath.
func strategicMerge(original, patch map[string]any, kind string) (map[string]any, error) {
	var target any

	switch kind {
	case "DaemonSet":
		target = &appsv1.DaemonSet{}
	case "Deployment":
		target = &appsv1.Deployment{}
	default:
		return nil, fmt.Errorf("cannot merge into unsupported kind %q", kind)
	}

	schema, err := strategicpatch.NewPatchMetaFromStruct(target)
	if err != nil {
		return nil, fmt.Errorf("load patch schema for %s: %w", kind, err)
	}

	merged, err := strategicpatch.StrategicMergeMapPatchUsingLookupPatchMeta(original, patch, schema)
	if err != nil {
		return nil, fmt.Errorf("merge patch: %w", err)
	}

	return merged, nil
}

// identity is the set of fields re-stamped after merging.
type identity struct {
	apiVersion  string
	kind        string
	name        string
	namespace   string
	ownerRefs   []any
	finalizers  []any
	selector    map[string]any
	templateSet map[string]any
}

func captureIdentity(workload *unstructured.Unstructured) identity {
	captured := identity{
		apiVersion: workload.GetAPIVersion(),
		kind:       workload.GetKind(),
		name:       workload.GetName(),
		namespace:  workload.GetNamespace(),
	}

	captured.ownerRefs = nestedSlice(workload.Object, "metadata", "ownerReferences")
	captured.finalizers = nestedSlice(workload.Object, "metadata", "finalizers")
	captured.selector = nestedMap(workload.Object, "spec", "selector")

	// Only the template labels the selector actually matches are restored. The
	// rest are the user's to set: a workload whose template labels stop
	// satisfying its selector is rejected outright by the API server.
	matchLabels := nestedStringMap(workload.Object, "spec", "selector", "matchLabels")
	templateLabels := nestedStringMap(workload.Object, "spec", "template", "metadata", "labels")

	captured.templateSet = map[string]any{}

	for key := range matchLabels {
		if value, ok := templateLabels[key]; ok {
			captured.templateSet[key] = value
		}
	}

	return captured
}

func restoreIdentity(workload *unstructured.Unstructured, captured identity) error {
	workload.SetAPIVersion(captured.apiVersion)
	workload.SetKind(captured.kind)
	workload.SetName(captured.name)
	workload.SetNamespace(captured.namespace)

	if err := setOrClearSlice(workload, captured.ownerRefs, "metadata", "ownerReferences"); err != nil {
		return err
	}

	if err := setOrClearSlice(workload, captured.finalizers, "metadata", "finalizers"); err != nil {
		return err
	}

	if captured.selector != nil {
		if err := setNestedMap(workload.Object, captured.selector, "spec", "selector"); err != nil {
			return fmt.Errorf("restore selector: %w", err)
		}
	}

	labels := nestedMap(workload.Object, "spec", "template", "metadata", "labels")
	if labels == nil {
		labels = map[string]any{}
	}

	for key, value := range captured.templateSet {
		labels[key] = value
	}

	if len(labels) > 0 {
		if err := setNestedMap(workload.Object, labels, "spec", "template", "metadata", "labels"); err != nil {
			return fmt.Errorf("restore selector-matched template labels: %w", err)
		}
	}

	return nil
}

func setOrClearSlice(workload *unstructured.Unstructured, value []any, fields ...string) error {
	if len(value) == 0 {
		unstructured.RemoveNestedField(workload.Object, fields...)

		return nil
	}

	if err := setNestedSlice(workload.Object, value, fields...); err != nil {
		return fmt.Errorf("restore %v: %w", fields, err)
	}

	return nil
}

// schedulingSet accumulates the scheduling constraints contributors ask for.
type schedulingSet struct {
	affinity     []map[string]any
	tolerations  []any
	nodeSelector map[string]any
}

func newSchedulingSet() *schedulingSet {
	return &schedulingSet{nodeSelector: map[string]any{}}
}

// absorb removes scheduling from a patch and records it, so the strategic merge
// never sees it.
//
// This is the whole reason scheduling is handled separately: NodeSelectorTerms
// carries no patchMergeKey, so a merge would replace the operator's terms
// outright. metalman and storage rely on a mandatory per-Site node affinity, so
// replacing it would let two Sites' workloads schedule onto the same nodes.
func (s *schedulingSet) absorb(patch map[string]any) {
	for _, path := range schedulingPaths {
		value, found, err := unstructured.NestedFieldNoCopy(patch, path...)
		if err != nil || !found {
			continue
		}

		switch path[len(path)-1] {
		case "affinity":
			if affinity, ok := value.(map[string]any); ok {
				s.affinity = append(s.affinity, affinity)
			}
		case "tolerations":
			if tolerations, ok := value.([]any); ok {
				s.tolerations = append(s.tolerations, tolerations...)
			}
		case "nodeSelector":
			if selector, ok := value.(map[string]any); ok {
				for key, entry := range selector {
					s.nodeSelector[key] = entry
				}
			}
		}

		unstructured.RemoveNestedField(patch, path...)
	}

	prune(patch, []string{"spec", "template", "spec"})
	prune(patch, []string{"spec", "template"})
	prune(patch, []string{"spec"})
}

// prune removes a map that became empty after scheduling was lifted out, so an
// otherwise empty patch does not merge an empty spec.
func prune(patch map[string]any, path []string) {
	value := nestedMap(patch, path...)
	if value == nil {
		return
	}

	if len(value) == 0 {
		unstructured.RemoveNestedField(patch, path...)
	}
}

// captureScheduling records the operator's own constraints before merging.
func captureScheduling(workload *unstructured.Unstructured) *schedulingSet {
	captured := newSchedulingSet()

	if affinity := nestedMap(workload.Object, "spec", "template", "spec", "affinity"); affinity != nil {
		captured.affinity = append(captured.affinity, affinity)
	}

	captured.tolerations = nestedSlice(workload.Object, "spec", "template", "spec", "tolerations")

	if selector := nestedMap(workload.Object, "spec", "template", "spec", "nodeSelector"); selector != nil {
		captured.nodeSelector = selector
	}

	return captured
}

// applyScheduling combines the operator's constraints with the user's so both
// hold, rather than letting either replace the other.
func applyScheduling(workload *unstructured.Unstructured, operator, user *schedulingSet) error {
	if err := applyNodeSelector(workload, operator, user); err != nil {
		return err
	}

	tolerations := append(append([]any{}, operator.tolerations...), user.tolerations...)
	if len(tolerations) > 0 {
		if err := setNestedSlice(workload.Object, tolerations, "spec", "template", "spec", "tolerations"); err != nil {
			return fmt.Errorf("set tolerations: %w", err)
		}
	}

	return applyAffinity(workload, operator, user)
}

func applyNodeSelector(workload *unstructured.Unstructured, operator, user *schedulingSet) error {
	if len(user.nodeSelector) == 0 && len(operator.nodeSelector) == 0 {
		return nil
	}

	combined := map[string]any{}
	for key, value := range operator.nodeSelector {
		combined[key] = value
	}

	keys := make([]string, 0, len(user.nodeSelector))
	for key := range user.nodeSelector {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		if existing, set := operator.nodeSelector[key]; set && existing != user.nodeSelector[key] {
			return fmt.Errorf(
				"nodeSelector key %q is set by the operator to %v; overriding it would change where the workload may run",
				key, existing)
		}

		combined[key] = user.nodeSelector[key]
	}

	if err := setNestedMap(workload.Object, combined, "spec", "template", "spec", "nodeSelector"); err != nil {
		return fmt.Errorf("set nodeSelector: %w", err)
	}

	return nil
}
