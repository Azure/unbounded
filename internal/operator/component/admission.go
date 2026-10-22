// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package component

import (
	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EffectiveAdmissionSpec returns a copy of the spec of a
// ValidatingAdmissionPolicy or ValidatingAdmissionPolicyBinding with the API
// server defaults filled in, so a desired manifest and its persisted live copy
// compare equal with equality.Semantic.DeepEqual unless they truly differ.
// Any other object yields nil.
func EffectiveAdmissionSpec(obj client.Object) any {
	switch obj := obj.(type) {
	case *admissionv1.ValidatingAdmissionPolicy:
		spec := obj.Spec.DeepCopy()
		if spec.FailurePolicy == nil {
			spec.FailurePolicy = ptr.To(admissionv1.Fail)
		}

		spec.MatchConstraints = effectiveAdmissionMatch(spec.MatchConstraints)

		return spec
	case *admissionv1.ValidatingAdmissionPolicyBinding:
		spec := obj.Spec.DeepCopy()
		spec.MatchResources = effectiveAdmissionMatch(spec.MatchResources)

		return spec
	default:
		return nil
	}
}

func effectiveAdmissionMatch(match *admissionv1.MatchResources) *admissionv1.MatchResources {
	if match == nil {
		match = &admissionv1.MatchResources{}
	}

	if match.MatchPolicy == nil {
		match.MatchPolicy = ptr.To(admissionv1.Equivalent)
	}

	if match.NamespaceSelector == nil {
		match.NamespaceSelector = &metav1.LabelSelector{}
	}

	if match.ObjectSelector == nil {
		match.ObjectSelector = &metav1.LabelSelector{}
	}

	for _, rules := range [][]admissionv1.NamedRuleWithOperations{match.ResourceRules, match.ExcludeResourceRules} {
		for i := range rules {
			if rules[i].Scope == nil {
				rules[i].Scope = ptr.To(admissionv1.AllScopes)
			}
		}
	}

	return match
}
