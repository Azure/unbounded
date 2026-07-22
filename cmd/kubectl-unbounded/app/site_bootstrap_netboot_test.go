// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/nodeagent"
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
		"gateway-external-address",
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

func TestBootstrapNetbootClaimsEndpointAndWaitsForDesignatedNode(t *testing.T) {
	ctx := context.Background()
	endpoint := &v1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-first-node", Generation: 3},
		Spec:       v1alpha3.NetbootEndpointSpec{SiteRef: "rack-a"},
	}
	resources := fakeclient.NewClientBuilder().WithScheme(buildScheme()).WithStatusSubresource(endpoint).WithObjects(endpoint).Build()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "node-1",
		Labels: map[string]string{v1alpha3.MachineSiteLabelKey: "rack-a"},
	}}
	kubeClient := fake.NewSimpleClientset(node)
	h := &siteBootstrapNetbootHandler{
		site: "rack-a", endpointName: endpoint.Name, resources: resources, kubeClient: kubeClient,
		pollInterval: time.Millisecond,
	}

	require.NoError(t, h.claimEndpoint(ctx, "bootstrap/123"))
	var claimed v1alpha3.NetbootEndpoint
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: endpoint.Name}, &claimed))
	require.Equal(t, endpoint.Generation, claimed.Status.ObservedGeneration)
	require.Equal(t, "bootstrap/123", claimed.Status.Claim.HolderIdentity)
	require.Equal(t, metav1.ConditionTrue, claimed.Status.Conditions[0].Status)

	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- h.waitForNodeReady(waitCtx, node.Name) }()

	time.Sleep(5 * time.Millisecond)
	other := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{v1alpha3.MachineSiteLabelKey: "rack-a"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	_, err := kubeClient.CoreV1().Nodes().Create(ctx, other, metav1.CreateOptions{})
	require.NoError(t, err)

	select {
	case err := <-ready:
		require.Failf(t, "wait returned for wrong Node", "error: %v", err)
	case <-time.After(5 * time.Millisecond):
	}

	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	_, err = kubeClient.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, <-ready)
}

func TestBootstrapNetbootDesignatedNodeNameUsesMachineNodeRef(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-name"},
		Spec: v1alpha3.MachineSpec{Kubernetes: &v1alpha3.KubernetesSpec{
			NodeRef: &v1alpha3.LocalObjectReference{Name: "node-name"},
		}},
	}

	require.Equal(t, "node-name", designatedNodeName(machine))
	machine.Spec.Kubernetes.NodeRef = nil
	require.Equal(t, "machine-name", designatedNodeName(machine))
}

func TestBootstrapPortForwardReconnectsToReadyServerPod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deployment := readyDeployment("metalman-server-rack-a", 2)
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "metalman-server"}}
	unready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "server-a", Namespace: deployment.Namespace, Labels: deployment.Spec.Selector.MatchLabels},
	}
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "server-b", Namespace: deployment.Namespace, Labels: deployment.Spec.Selector.MatchLabels},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	replacement := ready.DeepCopy()
	replacement.Name = "server-c"
	kubeClient := fake.NewSimpleClientset(deployment, unready, ready, replacement)
	starter := &fakeBootstrapPortForwardStarter{started: make(chan string, 4)}

	forward, err := newBootstrapPortForward(
		ctx,
		kubeClient,
		deployment.Namespace,
		deployment.Name,
		32123,
		8880,
		starter.start,
		time.Millisecond,
	)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:32123", forward.URL())
	require.Equal(t, "server-b", <-starter.started)

	starter.fail("server-b", errors.New("connection lost"))
	select {
	case podName := <-starter.started:
		require.Equal(t, "server-c", podName)
	case <-time.After(time.Second):
		require.Fail(t, "port-forward did not reconnect")
	}

	require.NoError(t, forward.Close())
	require.Equal(t, []int{32123, 32123}, starter.ports())
}

func TestBootstrapPortForwardClosesWhileWaitingToReconnect(t *testing.T) {
	deployment := readyDeployment("metalman-server-rack-a", 1)
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "metalman-server"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "server-a", Namespace: deployment.Namespace, Labels: deployment.Spec.Selector.MatchLabels},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	starter := &fakeBootstrapPortForwardStarter{started: make(chan string, 2)}
	forward, err := newBootstrapPortForward(
		context.Background(),
		fake.NewSimpleClientset(deployment, pod),
		deployment.Namespace,
		deployment.Name,
		32123,
		8880,
		starter.start,
		time.Hour,
	)
	require.NoError(t, err)
	require.Equal(t, pod.Name, <-starter.started)
	starter.fail(pod.Name, errors.New("connection lost"))
	time.Sleep(time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- forward.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.Fail(t, "port-forward close blocked during reconnect")
	}
}

func TestBootstrapEdgeTokenUsesAudienceAndRotatesSecureFile(t *testing.T) {
	ctx := context.Background()
	kubeClient := fake.NewSimpleClientset()
	var mu sync.Mutex
	requests := 0
	kubeClient.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateAction)
		request := create.GetObject().(*authenticationv1.TokenRequest)
		require.Equal(t, []string{"metalman-edge"}, request.Spec.Audiences)
		require.Equal(t, int64(3600), *request.Spec.ExpirationSeconds)

		mu.Lock()
		requests++
		token := fmt.Sprintf("token-%d", requests)
		mu.Unlock()

		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: token},
		}, nil
	})

	credential, err := newBootstrapEdgeToken(ctx, kubeClient, "unbounded-system", t.TempDir(), time.Millisecond)
	require.NoError(t, err)
	path := credential.Path()
	require.Equal(t, "edge-token", filepath.Base(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Eventually(t, func() bool {
		contents, readErr := os.ReadFile(path)
		return readErr == nil && string(contents) != "token-1"
	}, time.Second, time.Millisecond)

	require.NoError(t, credential.Close())
	_, err = os.Stat(filepath.Dir(path))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestBootstrapWaitsForEdgeListenerAndReportsEarlyExit(t *testing.T) {
	h := &siteBootstrapNetbootHandler{address: "192.0.2.10", httpPort: 8880, pollInterval: time.Millisecond}
	process := &fakeBootstrapEdgeProcess{done: make(chan struct{})}
	attempts := 0
	dial := func(_ context.Context, address string) error {
		require.Equal(t, "192.0.2.10:8880", address)
		attempts++
		if attempts < 2 {
			return errors.New("not listening")
		}

		return nil
	}

	require.NoError(t, h.waitForEdgeReady(context.Background(), process, dial))
	require.Equal(t, 2, attempts)

	failed := &fakeBootstrapEdgeProcess{done: make(chan struct{}), err: errors.New("bind: address already in use")}
	close(failed.done)
	err := h.waitForEdgeReady(context.Background(), failed, func(context.Context, string) error {
		return errors.New("not listening")
	})
	require.ErrorContains(t, err, "edge exited before becoming ready")
	require.ErrorContains(t, err, "address already in use")
}

func TestBootstrapPreflightResolvesMetalmanBeforeClusterMutation(t *testing.T) {
	explicitPath := filepath.Join(t.TempDir(), "metalman")
	require.NoError(t, os.WriteFile(explicitPath, []byte("binary"), 0o700))
	h := &siteBootstrapNetbootHandler{metalmanBinary: explicitPath}
	lookedUp := ""
	path, err := h.resolveMetalmanBinary(func(name string) (string, error) {
		lookedUp = name

		return name, nil
	})
	require.NoError(t, err)
	require.Equal(t, explicitPath, path)
	require.Empty(t, lookedUp)

	h.metalmanBinary = ""
	path, err = h.resolveMetalmanBinary(func(name string) (string, error) {
		lookedUp = name

		return "/usr/local/bin/metalman", nil
	})
	require.NoError(t, err)
	require.Equal(t, "metalman", lookedUp)
	require.Equal(t, "/usr/local/bin/metalman", path)

	_, err = h.resolveMetalmanBinary(func(string) (string, error) {
		return "", exec.ErrNotFound
	})
	require.ErrorContains(t, err, "--metalman-binary")
}

func TestBootstrapNetbootExecuteCleansUpAfterDesignatedNodeReady(t *testing.T) {
	ctx := context.Background()
	site, machine, resources, kubeClient := bootstrapLifecycleFixture(t, true)
	process := &fakeBootstrapEdgeProcess{done: make(chan struct{})}
	forward := testBootstrapPortForward(ctx)
	token := testBootstrapEdgeToken(t, ctx)
	h := &siteBootstrapNetbootHandler{
		site: site.Name, machine: machine.Name, interfaceName: "eno1", address: "192.0.2.10",
		httpPort: 8880, namespace: "unbounded-system", timeout: time.Second,
		resources: resources, kubeClient: kubeClient, restConfig: &rest.Config{}, pollInterval: time.Millisecond,
		dependencies: bootstrapNetbootDependencies{
			resolveBinary:    func() (string, error) { return "/usr/bin/metalman", nil },
			preflightNetwork: func() error { return nil },
			localPort:        func() (int, error) { return 32123, nil },
			portForward:      func(context.Context, int) (*bootstrapPortForward, error) { return forward, nil },
			edgeToken:        func(context.Context) (*bootstrapEdgeToken, error) { return token, nil },
			startEdge: func(binary string, args []string) (bootstrapEdgeProcess, error) {
				require.Equal(t, "/usr/bin/metalman", binary)
				require.Contains(t, args, "--backend-url=http://127.0.0.1:32123")

				return process, nil
			},
			dialEdge: func(context.Context, string) error { return nil },
		},
	}

	require.NoError(t, h.execute(ctx))
	require.True(t, process.stopped)
	require.Equal(t, "bootstrap-first-node", h.endpointName)

	var gotMachine v1alpha3.Machine
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine))
	require.Equal(t, "permanent-edge", gotMachine.Spec.Netboot().EndpointRef)
	var endpoint v1alpha3.NetbootEndpoint
	require.Error(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &endpoint))
	_, err := os.Stat(filepath.Dir(token.Path()))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestBootstrapNetbootExecuteReportsEarlyEdgeExitAndRollsBack(t *testing.T) {
	ctx := context.Background()
	site, machine, resources, kubeClient := bootstrapLifecycleFixture(t, false)
	process := &fakeBootstrapEdgeProcess{done: make(chan struct{}), err: errors.New("bind: address already in use")}
	close(process.done)
	h := &siteBootstrapNetbootHandler{
		site: site.Name, machine: machine.Name, interfaceName: "eno1", address: "192.0.2.10",
		httpPort: 8880, namespace: "unbounded-system", timeout: time.Second,
		resources: resources, kubeClient: kubeClient, restConfig: &rest.Config{}, pollInterval: time.Millisecond,
		dependencies: bootstrapNetbootDependencies{
			resolveBinary:    func() (string, error) { return "/usr/bin/metalman", nil },
			preflightNetwork: func() error { return nil },
			localPort:        func() (int, error) { return 32123, nil },
			portForward: func(ctx context.Context, _ int) (*bootstrapPortForward, error) {
				return testBootstrapPortForward(ctx), nil
			},
			edgeToken: func(ctx context.Context) (*bootstrapEdgeToken, error) {
				return testBootstrapEdgeToken(t, ctx), nil
			},
			startEdge: func(string, []string) (bootstrapEdgeProcess, error) { return process, nil },
			dialEdge:  func(context.Context, string) error { return errors.New("not listening") },
		},
	}

	err := h.execute(ctx)
	require.ErrorContains(t, err, "edge exited before becoming ready")

	var gotMachine v1alpha3.Machine
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine))
	require.Equal(t, "permanent-edge", gotMachine.Spec.Netboot().EndpointRef)
	var endpoint v1alpha3.NetbootEndpoint
	require.Error(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &endpoint))
}

func TestBootstrapNetbootExecuteRunsPreflightBeforeClusterMutation(t *testing.T) {
	site, machine, resources, kubeClient := bootstrapLifecycleFixture(t, true)
	h := &siteBootstrapNetbootHandler{
		site: site.Name, machine: machine.Name, interfaceName: "eno1", address: "192.0.2.10",
		httpPort: 8880, namespace: "unbounded-system", timeout: time.Second,
		resources: resources, kubeClient: kubeClient, restConfig: &rest.Config{},
		dependencies: bootstrapNetbootDependencies{
			resolveBinary:    func() (string, error) { return "", errors.New("metalman missing") },
			preflightNetwork: func() error { require.Fail(t, "network preflight should not follow binary failure"); return nil },
		},
	}

	err := h.execute(context.Background())
	require.ErrorContains(t, err, "metalman missing")

	var gotSite v1alpha3.Site
	require.NoError(t, resources.Get(context.Background(), client.ObjectKey{Name: site.Name}, &gotSite))
	require.Nil(t, gotSite.Spec.Components.Metalman)
}

func TestBootstrapNetbootPreparesAndCleansExternalGatewayResources(t *testing.T) {
	ctx := context.Background()
	resources := fakeclient.NewClientBuilder().WithScheme(buildScheme()).Build()
	kubeClient := fake.NewSimpleClientset()
	h := &siteBootstrapNetbootHandler{
		site: "rack-a", endpointName: "bootstrap-first-node", address: "192.0.2.10",
		gatewayExternalAddress: "198.51.100.10",
		routedCIDRs:            []string{"10.40.0.0/16", "10.50.0.0/24"}, resources: resources, kubeClient: kubeClient,
	}

	require.NoError(t, h.prepareGatewayResources(ctx))
	var pool netv1alpha1.GatewayPool
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &pool))
	require.Equal(t, "External", pool.Spec.Type)
	require.Equal(t, h.routedCIDRs, pool.Spec.RoutedCidrs)
	require.Equal(t, map[string]string{"net.unbounded-cloud.io/bootstrap-gateway": h.endpointName}, pool.Spec.NodeSelector)
	require.Equal(t, netv1alpha1.TunnelProtocolWireGuard, *pool.Spec.TunnelProtocol)

	var assignment netv1alpha1.SiteGatewayPoolAssignment
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &assignment))
	require.Equal(t, []string{h.site}, assignment.Spec.Sites)
	require.Equal(t, []string{h.endpointName}, assignment.Spec.GatewayPools)
	require.True(t, *assignment.Spec.Enabled)
	require.Equal(t, netv1alpha1.TunnelProtocolWireGuard, *assignment.Spec.TunnelProtocol)

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, h.endpointName, metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, h.endpointName, node.Labels["net.unbounded-cloud.io/bootstrap-gateway"])
	require.Equal(t, "true", node.Labels["net.unbounded-cloud.io/external-node"])
	require.Contains(t, node.Spec.Taints, corev1.Taint{
		Key: "net.unbounded-cloud.io/gateway-node", Value: "true", Effect: corev1.TaintEffectNoSchedule,
	})
	require.ElementsMatch(t, []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: h.address},
		{Type: corev1.NodeExternalIP, Address: h.gatewayExternalAddress},
	}, node.Status.Addresses)

	require.NoError(t, h.cleanupGatewayResources(ctx))
	_, err = kubeClient.CoreV1().Nodes().Get(ctx, h.endpointName, metav1.GetOptions{})
	require.Error(t, err)
	require.Error(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &assignment))
	require.Error(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &pool))
}

func TestBootstrapNetbootExecuteRunsOptInGatewayUntilNodeReady(t *testing.T) {
	ctx := context.Background()
	site, machine, resources, kubeClient := bootstrapLifecycleFixture(t, true)
	edgeProcess := &fakeBootstrapEdgeProcess{done: make(chan struct{})}
	gatewayProcess := &fakeBootstrapEdgeProcess{done: make(chan struct{})}
	forward := testBootstrapPortForward(ctx)
	token := testBootstrapEdgeToken(t, ctx)
	runtimeDir := t.TempDir()
	h := &siteBootstrapNetbootHandler{
		site: site.Name, machine: machine.Name, interfaceName: "eno1", address: "192.0.2.10",
		gatewayExternalAddress: "198.51.100.10", routedCIDRs: []string{"10.40.0.0/16"},
		httpPort: 8880, namespace: "unbounded-system", timeout: time.Second,
		resources: resources, kubeClient: kubeClient, restConfig: &rest.Config{}, pollInterval: time.Millisecond,
	}
	h.dependencies = bootstrapNetbootDependencies{
		resolveBinary:    func() (string, error) { return "/usr/bin/metalman", nil },
		preflightNetwork: func() error { return nil },
		preflightGateway: func() error { return nil },
		localPort:        func() (int, error) { return 32123, nil },
		portForward:      func(context.Context, int) (*bootstrapPortForward, error) { return forward, nil },
		edgeToken:        func(context.Context) (*bootstrapEdgeToken, error) { return token, nil },
		startEdge:        func(string, []string) (bootstrapEdgeProcess, error) { return edgeProcess, nil },
		dialEdge:         func(context.Context, string) error { return nil },
		gatewayRuntimeDir: func() (string, error) {
			return runtimeDir, nil
		},
		startGateway: func(_ context.Context, options nodeagent.ExternalGatewayOptions) (bootstrapEdgeProcess, error) {
			require.Equal(t, h.endpointName, options.NodeName)
			require.Equal(t, runtimeDir, options.RuntimeDir)
			require.Same(t, h.restConfig, options.RESTConfig)
			markBootstrapGatewayReady(t, ctx, h, resources, kubeClient)

			return gatewayProcess, nil
		},
	}

	require.NoError(t, h.execute(ctx))
	require.True(t, gatewayProcess.stopped)
	require.True(t, edgeProcess.stopped)
	var pool netv1alpha1.GatewayPool
	require.Error(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &pool))
	_, err := kubeClient.CoreV1().Nodes().Get(ctx, h.endpointName, metav1.GetOptions{})
	require.Error(t, err)
}

func markBootstrapGatewayReady(
	t *testing.T,
	ctx context.Context,
	h *siteBootstrapNetbootHandler,
	resources client.Client,
	kubeClient *fake.Clientset,
) {
	t.Helper()

	node, err := kubeClient.CoreV1().Nodes().Get(ctx, h.endpointName, metav1.GetOptions{})
	require.NoError(t, err)
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations["net.unbounded-cloud.io/wg-pubkey"] = "public-key"
	_, err = kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)

	var pool netv1alpha1.GatewayPool
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: h.endpointName}, &pool))
	pool.Status.NodeCount = 1
	pool.Status.Nodes = []netv1alpha1.GatewayNodeInfo{{Name: h.endpointName, WireGuardPublicKey: "public-key"}}
	pool.Status.ConnectedSites = []string{h.site}
	require.NoError(t, resources.Update(ctx, &pool))

	require.NoError(t, resources.Create(ctx, &netv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: h.endpointName},
		Spec:       netv1alpha1.GatewayNodeSpec{NodeName: h.endpointName, GatewayPool: h.endpointName},
		Status: netv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.Now(),
			Routes: map[string]netv1alpha1.GatewayNodeRoute{
				"10.40.0.0/16": {Type: "RoutedCidr"},
			},
		},
	}))
}

func TestBootstrapNetbootRejectsInvalidRoutedCIDRBeforeCreatingGateway(t *testing.T) {
	resources := fakeclient.NewClientBuilder().WithScheme(buildScheme()).Build()
	h := &siteBootstrapNetbootHandler{
		endpointName: "bootstrap-first-node", routedCIDRs: []string{"not-a-cidr"}, resources: resources,
		kubeClient: fake.NewSimpleClientset(),
	}

	err := h.prepareGatewayResources(context.Background())
	require.ErrorContains(t, err, "invalid routed CIDR")
	var pools netv1alpha1.GatewayPoolList
	require.NoError(t, resources.List(context.Background(), &pools))
	require.Empty(t, pools.Items)
}

func TestBootstrapNetbootExecuteRejectsInvalidRoutedCIDRBeforeClusterMutation(t *testing.T) {
	ctx := context.Background()
	site, machine, resources, kubeClient := bootstrapLifecycleFixture(t, false)
	h := &siteBootstrapNetbootHandler{
		site: site.Name, machine: machine.Name, interfaceName: "eno1", address: "192.0.2.10",
		routedCIDRs: []string{"not-a-cidr"}, httpPort: 8880, namespace: "unbounded-system",
		resources: resources, kubeClient: kubeClient, restConfig: &rest.Config{},
	}
	h.dependencies = bootstrapNetbootDependencies{
		resolveBinary:    func() (string, error) { return "/usr/bin/metalman", nil },
		preflightNetwork: func() error { return nil },
		gatewayRuntimeDir: func() (string, error) {
			t.Fatal("gateway runtime directory must not be created after invalid CIDR preflight")

			return "", nil
		},
	}

	err := h.execute(ctx)
	require.ErrorContains(t, err, "invalid routed CIDR")

	var actualMachine v1alpha3.Machine
	require.NoError(t, resources.Get(ctx, client.ObjectKey{Name: machine.Name}, &actualMachine))
	require.Equal(t, "permanent-edge", actualMachine.Spec.Netboot().EndpointRef)
	var endpoints v1alpha3.NetbootEndpointList
	require.NoError(t, resources.List(ctx, &endpoints))
	require.Empty(t, endpoints.Items)
}

func bootstrapLifecycleFixture(
	t *testing.T,
	nodeReady bool,
) (*v1alpha3.Site, *v1alpha3.Machine, client.Client, *fake.Clientset) {
	t.Helper()

	site := &v1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "first-node", Labels: map[string]string{v1alpha3.MachineSiteLabelKey: site.Name},
		},
		Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
			EndpointRef: "permanent-edge",
		}}},
	}
	resources := fakeclient.NewClientBuilder().
		WithScheme(buildScheme()).
		WithStatusSubresource(&v1alpha3.NetbootEndpoint{}).
		WithObjects(site, machine).
		Build()
	controller := readyDeployment("metalman-controller-rack-a", 1)
	server := readyDeployment("metalman-server-rack-a", 2)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machine.Name}}
	if nodeReady {
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	}

	return site, machine, resources, fake.NewSimpleClientset(controller, server, node)
}

func testBootstrapPortForward(ctx context.Context) *bootstrapPortForward {
	forwardCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		<-forwardCtx.Done()
		close(done)
	}()

	return &bootstrapPortForward{url: "http://127.0.0.1:32123", cancel: cancel, done: done}
}

func testBootstrapEdgeToken(t *testing.T, ctx context.Context) *bootstrapEdgeToken {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "edge-token")
	require.NoError(t, os.WriteFile(path, []byte("token"), 0o600))
	tokenCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		<-tokenCtx.Done()
		close(done)
	}()

	return &bootstrapEdgeToken{path: path, cancel: cancel, done: done}
}

type fakeBootstrapEdgeProcess struct {
	done    chan struct{}
	err     error
	stopped bool
}

func (f *fakeBootstrapEdgeProcess) Done() <-chan struct{} {
	return f.done
}

func (f *fakeBootstrapEdgeProcess) Err() error { return f.err }

func (f *fakeBootstrapEdgeProcess) Stop(context.Context) error {
	f.stopped = true

	return nil
}

type fakeBootstrapPortForwardStarter struct {
	mu       sync.Mutex
	attempts map[string]*fakeBootstrapPortForwardAttempt
	local    []int
	started  chan string
}

func (f *fakeBootstrapPortForwardStarter) start(_ context.Context, podName string, localPort, remotePort int) (bootstrapPortForwardAttempt, error) {
	if remotePort != 8880 {
		return nil, fmt.Errorf("unexpected remote port %d", remotePort)
	}

	attempt := &fakeBootstrapPortForwardAttempt{done: make(chan error, 1)}
	f.mu.Lock()
	if f.attempts == nil {
		f.attempts = map[string]*fakeBootstrapPortForwardAttempt{}
	}
	f.attempts[podName] = attempt
	f.local = append(f.local, localPort)
	f.mu.Unlock()
	f.started <- podName

	return attempt, nil
}

func (f *fakeBootstrapPortForwardStarter) fail(podName string, err error) {
	f.mu.Lock()
	attempt := f.attempts[podName]
	f.mu.Unlock()
	attempt.done <- err
}

func (f *fakeBootstrapPortForwardStarter) ports() []int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int(nil), f.local...)
}

type fakeBootstrapPortForwardAttempt struct {
	done chan error
	once sync.Once
}

func (f *fakeBootstrapPortForwardAttempt) Done() <-chan error {
	return f.done
}

func (f *fakeBootstrapPortForwardAttempt) Stop() {
	f.once.Do(func() { close(f.done) })
}
