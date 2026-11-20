// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestResolveTestBindingsCapturesAcceptedIdentity(t *testing.T) {
	t.Parallel()

	test := certificationTestObject(
		"script-check",
		types.UID("11111111-1111-1111-1111-111111111111"),
		2,
		2,
		"Approved",
		"sha256:"+strings.Repeat("a", 64),
	)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), test)

	references, err := resolveTestBindings(
		context.Background(),
		client,
		[]string{"script-check"},
	)
	require.NoError(t, err)
	require.Equal(t, test.GetUID(), references["script-check"].UID)
	require.Equal(t, int64(2), references["script-check"].Generation)
	require.Equal(
		t,
		"sha256:"+strings.Repeat("a", 64),
		references["script-check"].ContentDigest,
	)
}

func TestResolveTestBindingsRejectsUnobservedOrUntrustedGeneration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		test      *unstructured.Unstructured
		wantError string
	}{
		{
			name: "unobserved generation",
			test: certificationTestObject(
				"script-check",
				types.UID("11111111-1111-1111-1111-111111111111"),
				2,
				1,
				"Approved",
				"sha256:"+strings.Repeat("a", 64),
			),
			wantError: "generation 2 has not been observed",
		},
		{
			name: "rejected trust",
			test: certificationTestObject(
				"script-check",
				types.UID("11111111-1111-1111-1111-111111111111"),
				2,
				2,
				"Rejected",
				"sha256:"+strings.Repeat("a", 64),
			),
			wantError: "trust state is Rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), tt.test)
			_, err := resolveTestBindings(
				context.Background(),
				client,
				[]string{"script-check"},
			)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func certificationTestObject(
	name string,
	uid types.UID,
	generation,
	observedGeneration int64,
	trustState,
	contentDigest string,
) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "signal.unbounded-cloud.io/v1alpha1",
			"kind":       "CertificationTest",
			"metadata": map[string]any{
				"name":       name,
				"uid":        string(uid),
				"generation": generation,
			},
			"status": map[string]any{
				"observedGeneration": observedGeneration,
				"trust": map[string]any{
					"state":         trustState,
					"contentDigest": contentDigest,
				},
			},
		},
	}
}
