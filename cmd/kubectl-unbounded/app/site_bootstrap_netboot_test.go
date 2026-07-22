// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestSiteBootstrapNetbootCommandContract(t *testing.T) {
	group := siteCommandGroup()

	cmd, _, err := group.Find([]string{"bootstrap-netboot"})
	require.NoError(t, err)
	require.Equal(t, "bootstrap-netboot SITE", cmd.Use)

	for _, name := range []string{
		"machine",
		"interface",
		"address",
		"endpoint-name",
		"http-port",
		"kubeconfig",
		"namespace",
		"metalman-binary",
		"timeout",
		"routed-cidr",
	} {
		require.NotNilf(t, cmd.Flags().Lookup(name), "missing --%s", name)
	}

	for _, name := range []string{"machine", "interface", "address"} {
		flag := cmd.Flags().Lookup(name)
		require.Contains(t, flag.Annotations, "cobra_annotation_bash_completion_one_required_flag")
	}
}

func TestBootstrapNetbootPreparesAndRestoresClusterResources(t *testing.T) {
	ctx := context.Background()
	site := &v1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: v1alpha3.SiteSpec{
			Components: v1alpha3.SiteComponents{},
		},
	}
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "first-node",
			Labels: map[string]string{v1alpha3.MachineSiteLabelKey: site.Name},
		},
		Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
			EndpointRef: "permanent-edge",
		}}},
	}
	resources := fakeclient.NewClientBuilder().WithScheme(buildScheme()).WithObjects(site, machine).Build()
	h := &siteBootstrapNetbootHandler{
		site:         site.Name,
		machine:      machine.Name,
		address:      "192.0.2.10",
		httpPort:     8880,
		endpointName: "bootstrap-first-node",
		resources:    resources,
	}

	state, err := h.prepareClusterResources(ctx)
	require.NoError(t, err)
	require.Equal(t, "permanent-edge", state.originalEndpointRef)

	var gotSite v1alpha3.Site
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: site.Name}, &gotSite))
	require.NotNil(t, gotSite.Spec.Components.Metalman)
	require.NotNil(t, gotSite.Spec.Components.Metalman.Enabled)
	require.True(t, *gotSite.Spec.Components.Metalman.Enabled)

	var gotMachine v1alpha3.Machine
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine))
	require.Equal(t, h.endpointName, gotMachine.Spec.Netboot().EndpointRef)

	var endpoint v1alpha3.NetbootEndpoint
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &endpoint))
	require.Equal(t, v1alpha3.NetbootEndpointTypeExternalL2, endpoint.Spec.Type)
	require.Equal(t, site.Name, endpoint.Spec.SiteRef)
	require.Equal(t, "http://192.0.2.10:8880", endpoint.Spec.ExternalURL)
	require.Equal(t, v1alpha3.NetbootEndpointTrustTrustedLAN, endpoint.Spec.TLS.Trust)

	require.NoError(t, h.restoreClusterResources(ctx, state))
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine))
	require.Equal(t, "permanent-edge", gotMachine.Spec.Netboot().EndpointRef)
	require.Error(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &endpoint))
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: site.Name}, &gotSite))
	require.True(t, *gotSite.Spec.Components.Metalman.Enabled)
}

func TestBootstrapNetbootEndpointCollisionDoesNotMutateMachine(t *testing.T) {
	ctx := context.Background()
	enabled := true
	site := &v1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: v1alpha3.SiteSpec{Components: v1alpha3.SiteComponents{
			Metalman: &v1alpha3.MetalmanComponentSpec{SiteComponentSpec: v1alpha3.SiteComponentSpec{Enabled: &enabled}},
		}},
	}
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "first-node", Labels: map[string]string{v1alpha3.MachineSiteLabelKey: site.Name}},
		Spec:       v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{EndpointRef: "permanent-edge"}}},
	}
	existing := &v1alpha3.NetbootEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-first-node"}}
	resources := fakeclient.NewClientBuilder().WithScheme(buildScheme()).WithObjects(site, machine, existing).Build()
	h := &siteBootstrapNetbootHandler{
		site: site.Name, machine: machine.Name, address: "192.0.2.10", httpPort: 8880,
		endpointName: existing.Name, resources: resources,
	}

	_, err := h.prepareClusterResources(ctx)
	require.Error(t, err)

	var gotMachine v1alpha3.Machine
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine))
	require.Equal(t, "permanent-edge", gotMachine.Spec.Netboot().EndpointRef)
}
