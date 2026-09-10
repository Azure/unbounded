// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
)

// TestBuildMachineCRCarriesProvisioningFormat covers the declaration a
// controller-driven HostReplace depends on.
//
// The agent is the only party that knows how the host was provisioned: the
// image identifier is opaque, and the running host cannot be probed for it
// because a replacement may change the image. If the Machine does not carry it,
// replacement renders cloud-init for an Ignition host, destroying a working
// node and returning an unprovisioned one.
func TestBuildMachineCRCarriesProvisioningFormat(t *testing.T) {
	t.Parallel()

	cfg := &provision.AgentConfig{
		MachineName:        "node-1",
		ProvisioningFormat: config.ProvisioningFormatIgnition,
		Kubelet: provision.AgentKubeletConfig{
			Auth: provision.KubeletAuthInfo{BootstrapToken: "abc123.secret456"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&v1alpha3.Machine{}).Build()
	require.NoError(t, registerMachine(context.Background(), slog.New(slog.DiscardHandler), c, cfg))

	var machine v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: cfg.MachineName}, &machine))
	require.Nil(t, machine.Spec.Host)
	assert.Equal(t, v1alpha3.ProvisioningFormatIgnition, machine.Status.ObservedProvisioningFormat)
}

func TestRegistrationReportsFormatWithoutChangingExistingSpec(t *testing.T) {
	t.Parallel()

	for _, desired := range []v1alpha3.ProvisioningFormat{"", v1alpha3.ProvisioningFormatCloudInit} {
		t.Run(string(desired), func(t *testing.T) {
			machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "existing"}, Spec: v1alpha3.MachineSpec{
				Host: &v1alpha3.HostSpec{Image: "/opaque/image", ProvisioningFormat: desired},
			}}
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(machine).WithStatusSubresource(machine).Build()
			cfg := &provision.AgentConfig{
				MachineName: "existing", ProvisioningFormat: config.ProvisioningFormatIgnition,
				Kubelet: provision.AgentKubeletConfig{Auth: provision.KubeletAuthInfo{BootstrapToken: "abc.secret"}},
			}
			require.NoError(t, registerMachine(context.Background(), slog.New(slog.DiscardHandler), c, cfg))

			var got v1alpha3.Machine
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "existing"}, &got))
			require.Equal(t, machine.Spec, got.Spec)
			require.Equal(t, v1alpha3.ProvisioningFormatIgnition, got.Status.ObservedProvisioningFormat)
		})
	}
}

// TestBuildMachineCRLeavesCloudInitUnset keeps hosts that predate the field
// working: unset already means cloud-init, and writing it out would suggest a
// declaration the host never made.
func TestBuildMachineCRLeavesCloudInitUnset(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"", config.ProvisioningFormatCloudInit} {
		cfg := &provision.AgentConfig{
			MachineName:        "node-1",
			ProvisioningFormat: format,
			Kubelet: provision.AgentKubeletConfig{
				Auth: provision.KubeletAuthInfo{BootstrapToken: "abc123.secret456"},
			},
		}

		machine := buildMachineCR(cfg)

		assert.Nil(t, machine.Spec.Host,
			"an undeclared or cloud-init host must not gain a host spec")
	}
}
