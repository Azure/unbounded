// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestAdmissionPoliciesPrecedeRuntimePermissions(t *testing.T) {
	env := testEnv(t, volume("cache"))
	initialize(t, env)
	plan := planPass(t, env)
	order, err := plan.ExecutionOrder()
	require.NoError(t, err)

	var policies []component.ObjectRef

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "ValidatingAdmissionPolicy" || op.Object.GetKind() == "ValidatingAdmissionPolicyBinding" {
			policies = append(policies, op.Ref())
		}

		if op.Object.GetKind() == "ValidatingAdmissionPolicy" && op.Object.GetName() == "racer-node-write-restriction" {
			conditions, found, err := unstructured.NestedSlice(op.Object.Object, "spec", "matchConditions")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, []any{map[string]any{"name": "racer-controller", "expression": `request.userInfo.username == "system:serviceaccount:` + env.Namespace + `:racer-controller"`}}, conditions)
		}
	}

	require.Len(t, policies, 4)

	var gated []string

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "ClusterRoleBinding", "RoleBinding", "Deployment", "DaemonSet":
			gated = append(gated, op.Object.GetKind())
			for _, policy := range policies {
				require.Contains(t, op.DependsOn, policy)
				require.Less(t, strings.Index(order, "Apply "+policy.String()+"\n"), strings.Index(order, "Apply "+op.Ref().String()+"\n"))
			}
		}
	}

	require.ElementsMatch(t, []string{"ClusterRoleBinding", "RoleBinding", "Deployment", "DaemonSet"}, gated)
	persist(t, env, plan)
}

func TestAdmissionPolicyPairFailuresGateRuntimePermissions(t *testing.T) {
	for _, name := range []string{"racer-runtime-write-restriction", "racer-node-write-restriction"} {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				env := testEnv(t, volume("cache"))
				initialize(t, env)

				policy := &unstructured.Unstructured{}
				policy.SetAPIVersion("admissionregistration.k8s.io/v1")
				policy.SetKind(kind)
				policy.SetName(name)
				require.NoError(t, env.Client.Delete(t.Context(), policy))
				plan := planPass(t, env)
				env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Apply: func(ctx context.Context, c client.WithWatch, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
						data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
						require.NoError(t, err)

						if data["kind"] == kind && data["metadata"].(map[string]any)["name"] == name {
							return errors.New("policy unavailable")
						}

						return c.Apply(ctx, cfg, opts...)
					},
				})
				result, err := env.Execute(t.Context(), plan)
				require.NoError(t, err)
				require.Len(t, result.Failed(), 1)
				require.Equal(t, name, result.Failed()[0].Ref.Name)
				require.Equal(t, kind, result.Failed()[0].Ref.GVK.Kind)

				var skipped []string

				for _, op := range result.Results {
					switch op.Ref.GVK.Kind {
					case "ClusterRoleBinding", "RoleBinding", "Deployment", "DaemonSet":
						require.Equal(t, component.OpSkipped, op.Status, "%s", op.Ref)
						skipped = append(skipped, op.Ref.GVK.Kind)
					}
				}

				require.ElementsMatch(t, []string{"ClusterRoleBinding", "RoleBinding", "Deployment", "DaemonSet"}, skipped)
			})
		}
	}
}
