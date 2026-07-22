// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
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

func TestBootstrapNetbootMetalmanReadinessRequiresCurrentControllerAndServers(t *testing.T) {
	controller := readyDeployment("metalman-controller-rack-a", 1)
	server := readyDeployment("metalman-server-rack-a", 2)
	server.Status.ObservedGeneration--
	kubeClient := fake.NewSimpleClientset(controller, server)
	h := &siteBootstrapNetbootHandler{site: "rack-a", namespace: "unbounded-system", kubeClient: kubeClient}

	ready, err := h.metalmanDeploymentsReady(context.Background())
	require.NoError(t, err)
	require.False(t, ready)

	server.Status.ObservedGeneration = server.Generation
	_, err = kubeClient.AppsV1().Deployments(h.namespace).UpdateStatus(context.Background(), server, metav1.UpdateOptions{})
	require.NoError(t, err)

	ready, err = h.metalmanDeploymentsReady(context.Background())
	require.NoError(t, err)
	require.True(t, ready)
}

func TestBootstrapNetbootWaitsUntilMetalmanRolloutCompletes(t *testing.T) {
	controller := readyDeployment("metalman-controller-rack-a", 1)
	server := readyDeployment("metalman-server-rack-a", 2)
	server.Status.AvailableReplicas = 1
	kubeClient := fake.NewSimpleClientset(controller, server)
	h := &siteBootstrapNetbootHandler{
		site: "rack-a", namespace: "unbounded-system", kubeClient: kubeClient,
		pollInterval: time.Millisecond,
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		server.Status.AvailableReplicas = 2
		_, _ = kubeClient.AppsV1().Deployments(h.namespace).UpdateStatus(context.Background(), server, metav1.UpdateOptions{})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, h.waitForMetalman(ctx))
}

func readyDeployment(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "unbounded-system", Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			AvailableReplicas:  replicas,
		},
	}
}

func TestBootstrapNetbootEdgeArgumentsContainOnlyDataPlaneConfiguration(t *testing.T) {
	h := &siteBootstrapNetbootHandler{
		endpointName:  "bootstrap-first-node",
		interfaceName: "eno1",
		address:       "192.0.2.10",
		httpPort:      8880,
	}

	args := h.edgeArguments("http://127.0.0.1:32123", "/run/token")
	require.Equal(t, []string{
		"edge",
		"--backend-url=http://127.0.0.1:32123",
		"--endpoint=bootstrap-first-node",
		"--bind-address=192.0.2.10",
		"--http-port=8880",
		"--dhcp-enabled",
		"--dhcp-interface=eno1",
		"--dhcp-server-ip=192.0.2.10",
		"--edge-token-file=/run/token",
		"--tftp-enabled",
		"--tftp-bind-address=192.0.2.10",
	}, args)

	for _, arg := range args {
		require.NotContains(t, arg, "--site")
		require.NotContains(t, arg, "--cache-dir")
		require.NotContains(t, arg, "leader-elect")
	}
}
