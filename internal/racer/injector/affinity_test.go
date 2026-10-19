// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package injector

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func userReq(key string) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}
}

func requiredOf(affinity *corev1.Affinity) *corev1.NodeSelector {
	return &corev1.NodeSelector{NodeSelectorTerms: affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms}
}

func TestAddExclusionAffinity(t *testing.T) {
	excl := exclusionRequirement()
	field := corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"n1"}}

	withRequired := func(sel *corev1.NodeSelector) *corev1.Affinity {
		return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: sel}}
	}

	tests := []struct {
		name string
		in   *corev1.Affinity
		want *corev1.NodeSelector
	}{
		{
			name: "absent affinity synthesizes exclusion-only selector",
			in:   nil,
			want: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{excl}}}},
		},
		{
			name: "absent required selector synthesizes exclusion-only selector",
			in:   &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
			want: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{excl}}}},
		},
		{
			name: "zero terms keep matching no nodes",
			in:   withRequired(&corev1.NodeSelector{}),
			want: &corev1.NodeSelector{},
		},
		{
			name: "empty term alone keeps matching no nodes",
			in:   withRequired(&corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}),
			want: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}},
		},
		{
			name: "empty term ORed with nonempty term is not widened",
			in: withRequired(&corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{},
				{MatchExpressions: []corev1.NodeSelectorRequirement{userReq("zone")}},
			}}),
			want: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{},
				{MatchExpressions: []corev1.NodeSelectorRequirement{userReq("zone"), excl}},
			}},
		},
		{
			name: "match fields only term gets exclusion",
			in: withRequired(&corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchFields: []corev1.NodeSelectorRequirement{field}},
			}}),
			want: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{excl}, MatchFields: []corev1.NodeSelectorRequirement{field}},
			}},
		},
		{
			name: "multiple terms each get exclusion once",
			in: withRequired(&corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{userReq("zone")}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{userReq("rack"), excl}},
			}}),
			want: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{userReq("zone"), excl}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{userReq("rack"), excl}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &corev1.PodSpec{Affinity: tt.in}
			addExclusionAffinity(spec)

			got := requiredOf(spec.Affinity)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("required selector = %+v, want %+v", got, tt.want)
			}

			if excl.Key != wire.ExclusionLabel {
				t.Fatalf("exclusion key = %q", excl.Key)
			}
		})
	}
}
