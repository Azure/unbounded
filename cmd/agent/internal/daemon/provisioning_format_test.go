// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

func TestRegistrationReportsOnlyExplicitFormat(t *testing.T) {
	t.Parallel()

	for _, existing := range []bool{false, true} {
		for _, format := range []string{"", "cloud-init", " ignition "} {
			t.Run(format+"/"+map[bool]string{true: "existing", false: "new"}[existing], func(t *testing.T) {
				t.Parallel()

				m := &api.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: api.MachineSpec{Host: &api.HostSpec{Image: "opaque", ProvisioningFormat: api.ProvisioningFormatCloudInit}}}

				builder := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(m)
				if existing {
					builder = builder.WithObjects(m)
				}

				c := builder.Build()
				cfg := &provision.AgentConfig{MachineName: m.Name, ProvisioningFormat: format, Kubelet: provision.AgentKubeletConfig{Auth: provision.KubeletAuthInfo{BootstrapToken: "abc123.secret"}}}
				require.NoError(t, registerMachine(t.Context(), discardLogger(), c, cfg))

				var got api.Machine
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), &got))

				want := map[string]api.ProvisioningFormat{"": "", "cloud-init": api.ProvisioningFormatCloudInit, " ignition ": api.ProvisioningFormatIgnition}[format]
				require.Equal(t, want, got.Status.ObservedProvisioningFormat)

				if existing {
					require.Equal(t, m.Spec, got.Spec)
				} else {
					require.Nil(t, got.Spec.Host)
				}
			})
		}
	}
}

func TestFormatObservationRetriesConflictAndPreservesOtherStatus(t *testing.T) {
	t.Parallel()

	m := &api.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}
	patches := 0
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(m).WithStatusSubresource(m).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches++
			if patches == 1 {
				var latest api.Machine
				if err := c.Get(ctx, client.ObjectKeyFromObject(m), &latest); err != nil {
					return err
				}

				latest.Status.Conditions = []metav1.Condition{{Type: "OtherController", Status: metav1.ConditionTrue, Reason: "Ready", Message: "preserve me", LastTransitionTime: metav1.Now()}}
				if err := c.Status().Update(ctx, &latest); err != nil {
					return err
				}

				return apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "machines"}, m.Name, nil)
			}

			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	require.NoError(t, reportProvisioningFormat(t.Context(), c, &provision.AgentConfig{MachineName: m.Name, ProvisioningFormat: "ignition"}))
	require.Equal(t, 2, patches)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
	require.Equal(t, api.ProvisioningFormatIgnition, m.Status.ObservedProvisioningFormat)
	require.Len(t, m.Status.Conditions, 1)
	require.Equal(t, "preserve me", m.Status.Conditions[0].Message)
}
