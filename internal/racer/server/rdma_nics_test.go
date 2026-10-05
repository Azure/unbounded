// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestAuthenticatedRDMANICProposalAndEmptyRemoval(t *testing.T) {
	f := newServingFixture(t)
	f.request.Shares = 9
	f.request.RDMANICs = []wire.RDMANIC{{Device: "mlx5_1", Port: 1, Rail: 0}, {Device: "mlx5_0", Port: 2, Rail: 0}}
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, wire.BootstrapPath, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.token)
	_, err = f.a.Server.enroll(f.ctx, req, f.request)
	require.NoError(t, err)

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
	require.Equal(t, "9", node.Annotations[enrolledSharesAnnotation])
	nics, err := wire.DecodeRDMANICs(strings.NewReader(node.Annotations[enrolledRDMANICsAnnotation]))
	require.NoError(t, err)
	require.Equal(t, wire.CanonicalRDMANICs(f.request.RDMANICs), nics)

	node.Annotations[wire.RDMANICsAnnotation] = "[]"
	require.NoError(t, f.a.Topology.Update(f.ctx, &node))
	attributes, err := members.ParseAnnotations(&node)
	require.NoError(t, err)
	require.Empty(t, attributes.RDMANICs)
	f.request.RDMANICs = nil
	f.request.Shares = 10
	_, err = f.a.Server.enroll(f.ctx, req, f.request)
	require.NoError(t, err)
	require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
	require.NotContains(t, node.Annotations, enrolledRDMANICsAnnotation)
	require.Equal(t, "10", node.Annotations[enrolledSharesAnnotation])
	require.Equal(t, "[]", node.Annotations[wire.RDMANICsAnnotation])
}

func TestEnrollmentRDMANICAtomicPatchFailure(t *testing.T) {
	f := newServingFixture(t)
	f.request.Shares = 9
	f.request.RDMANICs = []wire.RDMANIC{{Device: "mlx5_0", Port: 1}}
	patchFailure := errors.New("patch rejected")
	patched := false
	fixtureDependencies[f.a.authority].Client = interceptor.NewClient(fixtureDependencies[f.a.authority].Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
			patched = true
			data, err := patch.Data(obj)
			require.NoError(t, err)
			require.Contains(t, string(data), enrolledSharesAnnotation)
			require.Contains(t, string(data), enrolledRDMANICsAnnotation)
			require.Contains(t, string(data), "resourceVersion")

			return patchFailure
		},
	})
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, wire.BootstrapPath, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.token)
	_, err = f.a.Server.enroll(f.ctx, req, f.request)
	require.ErrorIs(t, err, patchFailure)
	require.True(t, patched)

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
	require.NotContains(t, node.Annotations, enrolledSharesAnnotation)
	require.NotContains(t, node.Annotations, enrolledRDMANICsAnnotation)
}

func TestEnrollmentRDMANICLiveNodeRecheck(t *testing.T) {
	for _, scenario := range []string{"replacement UID", "excluded"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			f.request.RDMANICs = []wire.RDMANIC{{Device: "mlx5_0", Port: 1}}
			nodeReads := 0
			fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}

					if node, ok := obj.(*corev1.Node); ok {
						nodeReads++
						if nodeReads > 1 {
							if scenario == "replacement UID" {
								node.UID = testOtherUID
							} else {
								node.Labels = map[string]string{wire.ExclusionLabel: ""}
							}
						}
					}

					return nil
				},
			})
			req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, wire.BootstrapPath, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+f.token)
			_, err = f.a.Server.enroll(f.ctx, req, f.request)
			require.ErrorIs(t, err, wire.Forbidden)
			require.Equal(t, 2, nodeReads)

			var node corev1.Node
			require.NoError(t, f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
			require.NotContains(t, node.Annotations, enrolledSharesAnnotation)
			require.NotContains(t, node.Annotations, enrolledRDMANICsAnnotation)
		})
	}
}
