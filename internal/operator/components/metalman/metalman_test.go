// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metalman

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, policyv1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func TestEnabled(t *testing.T) {
	if (Component{}).Enabled(&unboundedv1alpha3.Site{}) {
		t.Fatal("metalman enabled with no component spec")
	}

	enabled := true
	site := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Metalman: &unboundedv1alpha3.MetalmanComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled}},
	}}}

	if !(Component{}).Enabled(site) {
		t.Fatal("metalman not enabled when spec enables it")
	}
}

func TestMutateSupportObject(t *testing.T) {
	keep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}
	if err := mutateSupportObject(keep); err != nil {
		t.Fatalf("mutateSupportObject returned error: %v", err)
	}

	if keep.Object == nil {
		t.Fatalf("metalman support object was dropped")
	}

	drop := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   map[string]any{"name": "machina-controller"},
	}}
	if err := mutateSupportObject(drop); err != nil {
		t.Fatalf("mutateSupportObject returned error: %v", err)
	}

	if drop.Object != nil {
		t.Fatalf("non-metalman object was not dropped")
	}
}

func TestRBACSeparatesControllerAndServerIdentities(t *testing.T) {
	data, err := os.ReadFile("../../../../deploy/machina/06-metalman-rbac.yaml.tmpl")
	if err != nil {
		t.Fatalf("read Metalman RBAC template: %v", err)
	}
	manifest := string(data)

	for _, required := range []string{
		"name: metalman-controller",
		"name: metalman-server",
		"name: metalman-edge",
		"resources: [\"tokenreviews\"]",
		"resources: [\"serviceaccounts/token\"]",
	} {
		if !strings.Contains(manifest, required) {
			t.Fatalf("RBAC template missing %q", required)
		}
	}

	serverNamespaceRole := manifest[strings.Index(manifest, "kind: Role\nmetadata:\n  name: metalman-server"):]
	serverNamespaceRole = serverNamespaceRole[:strings.Index(serverNamespaceRole, "\n---")]
	if !strings.Contains(serverNamespaceRole, "resources: [\"serviceaccounts/token\"]") {
		t.Fatalf("metalman-server Role lacks bootstrap-token issuance:\n%s", serverNamespaceRole)
	}

	serverClusterRole := manifest[strings.Index(manifest, "kind: ClusterRole\nmetadata:\n  name: metalman-server"):]
	serverClusterRole = serverClusterRole[:strings.Index(serverClusterRole, "\n---")]
	if !strings.Contains(serverClusterRole, "resources: [\"tokenreviews\"]") {
		t.Fatalf("metalman-server ClusterRole lacks TokenReview permission:\n%s", serverClusterRole)
	}

	controllerRole := manifest[strings.Index(manifest, "kind: ClusterRole\nmetadata:\n  name: metalman-controller\nrules:"):]
	controllerRole = controllerRole[:strings.Index(controllerRole, "\n---")]
	if strings.Contains(controllerRole, "resources: [\"tokenreviews\"]") {
		t.Fatalf("metalman-controller retains server TokenReview permission:\n%s", controllerRole)
	}
}

func TestOperatorRBACCanReconcileNetbootEndpoints(t *testing.T) {
	data, err := os.ReadFile("../../../../deploy/unbounded-operator/02-rbac.yaml.tmpl")
	if err != nil {
		t.Fatalf("read operator RBAC template: %v", err)
	}
	manifest := string(data)
	for _, required := range []string{
		"resources: [\"netbootendpoints\"]",
		"resources: [\"netbootendpoints/status\"]",
		"resources: [\"poddisruptionbudgets\"]",
	} {
		if !strings.Contains(manifest, required) {
			t.Fatalf("operator RBAC template missing %q", required)
		}
	}
}

func TestReconcileManagedEndpointStatusFromDeploymentAvailability(t *testing.T) {
	scheme := testScheme(t)
	now := metav1.Now()
	endpoint := &unboundedv1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "public-http", Generation: 3},
		Spec: unboundedv1alpha3.NetbootEndpointSpec{
			SiteRef: "rack-a",
			Type:    unboundedv1alpha3.NetbootEndpointTypeHTTP,
		},
		Status: unboundedv1alpha3.NetbootEndpointStatus{Claim: &unboundedv1alpha3.NetbootEndpointClaim{
			HolderIdentity: "edge-pod-1",
			RenewedAt:      now,
		}},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: EdgeName(endpoint.Name), Namespace: component.DefaultNamespace, Generation: 7},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 7, AvailableReplicas: 1},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(endpoint).WithObjects(endpoint, deployment).Build()

	if err := reconcileManagedEndpointStatus(t.Context(), kubeClient, endpoint, deployment); err != nil {
		t.Fatalf("reconcileManagedEndpointStatus: %v", err)
	}
	updated := &unboundedv1alpha3.NetbootEndpoint{}
	if err := kubeClient.Get(t.Context(), client.ObjectKey{Name: endpoint.Name}, updated); err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if updated.Status.ObservedGeneration != endpoint.Generation {
		t.Fatalf("observed generation = %d, want %d", updated.Status.ObservedGeneration, endpoint.Generation)
	}
	ready := findCondition(updated.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != "EdgeAvailable" {
		t.Fatalf("Ready condition = %#v", ready)
	}
	if updated.Status.Claim == nil || updated.Status.Claim.HolderIdentity != "edge-pod-1" || updated.Status.Claim.RenewedAt.Time.Unix() != now.Time.Unix() {
		t.Fatalf("claim was not preserved: %#v", updated.Status.Claim)
	}

	deployment.Status.AvailableReplicas = 0
	if err := kubeClient.Status().Update(t.Context(), deployment); err != nil {
		t.Fatalf("update Deployment status: %v", err)
	}
	if err := reconcileManagedEndpointStatus(t.Context(), kubeClient, updated, deployment); err != nil {
		t.Fatalf("reconcile unavailable endpoint: %v", err)
	}
	if err := kubeClient.Get(t.Context(), client.ObjectKey{Name: endpoint.Name}, updated); err != nil {
		t.Fatalf("get unavailable endpoint: %v", err)
	}
	ready = findCondition(updated.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "EdgeUnavailable" {
		t.Fatalf("unavailable Ready condition = %#v", ready)
	}
}

func TestReconcileManagedEndpointStatusLeavesExternalClaimUntouched(t *testing.T) {
	scheme := testScheme(t)
	endpoint := &unboundedv1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-laptop", Generation: 2},
		Spec:       unboundedv1alpha3.NetbootEndpointSpec{SiteRef: "rack-a", Type: unboundedv1alpha3.NetbootEndpointTypeExternalL2},
		Status: unboundedv1alpha3.NetbootEndpointStatus{
			ObservedGeneration: 1,
			Conditions:         []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ExternalEdgeReady"}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(endpoint).WithObjects(endpoint).Build()

	if err := reconcileManagedEndpointStatus(t.Context(), kubeClient, endpoint, nil); err != nil {
		t.Fatalf("reconcileManagedEndpointStatus: %v", err)
	}
	updated := &unboundedv1alpha3.NetbootEndpoint{}
	if err := kubeClient.Get(t.Context(), client.ObjectKey{Name: endpoint.Name}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ObservedGeneration != 1 || updated.Status.Conditions[0].Reason != "ExternalEdgeReady" {
		t.Fatalf("external endpoint status changed: %#v", updated.Status)
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}

	return nil
}

func TestDeployment(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	d := controllerDeployment(site, component.DefaultNamespace, component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3", APIServerEndpoint: "https://api.example:6443"})
	if d.Name != "metalman-controller-rack-a" {
		t.Fatalf("name = %q", d.Name)
	}

	if d.Namespace != component.DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", d.Namespace, component.DefaultNamespace)
	}

	container := d.Spec.Template.Spec.Containers[0]
	if container.Image != "registry.example.com/azure/metalman:v1.2.3" {
		t.Fatalf("image = %q", container.Image)
	}

	if got := container.Args; len(got) != 3 || got[0] != "controller" || got[1] != "--site=rack-a" || got[2] != "--cache-dir=/var/cache/metalman" {
		t.Fatalf("args = %#v", got)
	}

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", d.Spec.Replicas)
	}

	for _, path := range []string{"deployment", "selector", "pod"} {
		var got string

		switch path {
		case "deployment":
			got = d.Labels[unboundedv1alpha3.MachineSiteLabelKey]
		case "selector":
			got = d.Spec.Selector.MatchLabels[unboundedv1alpha3.MachineSiteLabelKey]
		case "pod":
			got = d.Spec.Template.Labels[unboundedv1alpha3.MachineSiteLabelKey]
		}

		if got != "rack-a" {
			t.Fatalf("%s site label = %q, want rack-a", path, got)
		}
	}

	podNS := findEnv(container.Env, "POD_NAMESPACE")
	if podNS == nil || podNS.ValueFrom == nil || podNS.ValueFrom.FieldRef == nil ||
		podNS.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Fatalf("POD_NAMESPACE env = %#v, want Downward API metadata.namespace", podNS)
	}

	if got := findEnv(container.Env, "METALMAN_APISERVER_URL"); got == nil || got.Value != "https://api.example:6443" {
		t.Fatalf("METALMAN_APISERVER_URL env = %#v, want https://api.example:6443", got)
	}

	assertSiteOwnerRef(t, d.OwnerReferences, "rack-a", "site-uid")
	assertOrdinaryPodNetworking(t, &d.Spec.Template.Spec)

	strategy := d.Spec.Strategy
	if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || strategy.RollingUpdate == nil {
		t.Fatalf("expected RollingUpdate strategy, got %+v", strategy)
	}

	if got := strategy.RollingUpdate.MaxSurge; got == nil || got.IntValue() != 1 {
		t.Fatalf("expected maxSurge=1, got %v", got)
	}

	if got := strategy.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 0 {
		t.Fatalf("expected maxUnavailable=0, got %v", got)
	}
}

func TestControllerAndServerWorkloadsAreSeparated(t *testing.T) {
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"},
	}
	cfg := component.Config{
		ImageRegistry:     "registry.example.com",
		ImageTag:          "v1.2.3",
		APIServerEndpoint: "https://api.example:6443",
	}

	controller := controllerDeployment(site, component.DefaultNamespace, cfg)
	if controller.Spec.Replicas == nil || *controller.Spec.Replicas != 1 {
		t.Fatalf("controller replicas = %v, want 1", controller.Spec.Replicas)
	}
	assertOrdinaryPodNetworking(t, &controller.Spec.Template.Spec)

	controllerContainer := controller.Spec.Template.Spec.Containers[0]
	if got := controllerContainer.Args; len(got) < 2 || got[0] != "controller" || got[1] != "--site=rack-a" {
		t.Fatalf("controller args = %#v", got)
	}
	if controller.Spec.Template.Spec.ServiceAccountName != "metalman-controller" {
		t.Fatalf("controller service account = %q", controller.Spec.Template.Spec.ServiceAccountName)
	}
	assertCapabilityKeyMount(t, &controller.Spec.Template.Spec, &controllerContainer, "rack-a")
	for _, port := range controllerContainer.Ports {
		if port.Name == "http" || port.Name == "dhcp" || port.Name == "tftp" {
			t.Fatalf("controller exposes data-plane port %#v", port)
		}
	}

	server := serverDeployment(site, component.DefaultNamespace, cfg)
	if server.Spec.Replicas == nil || *server.Spec.Replicas != 2 {
		t.Fatalf("server replicas = %v, want 2", server.Spec.Replicas)
	}
	assertOrdinaryPodNetworking(t, &server.Spec.Template.Spec)

	serverContainer := server.Spec.Template.Spec.Containers[0]
	if got := serverContainer.Args; len(got) < 3 || got[0] != "server" || got[1] != "--site=rack-a" || got[2] != "--cache-dir=/var/cache/metalman" {
		t.Fatalf("server args = %#v", got)
	}
	if server.Spec.Template.Spec.ServiceAccountName != "metalman-server" {
		t.Fatalf("server service account = %q", server.Spec.Template.Spec.ServiceAccountName)
	}
	assertCapabilityKeyMount(t, &server.Spec.Template.Spec, &serverContainer, "rack-a")
	if !hasContainerPort(serverContainer.Ports, "http", 8880) {
		t.Fatalf("server ports = %#v, want HTTP 8880", serverContainer.Ports)
	}
	assertWorkloadHealthAndResources(t, &serverContainer)
	if len(server.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
		t.Fatalf("server topology spread constraints = %#v", server.Spec.Template.Spec.TopologySpreadConstraints)
	}
	spread := server.Spec.Template.Spec.TopologySpreadConstraints[0]
	if spread.TopologyKey != corev1.LabelHostname || spread.MaxSkew != 1 || spread.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Fatalf("server topology spread = %#v", spread)
	}
	if got := server.Spec.Strategy.RollingUpdate; got == nil || got.MaxUnavailable == nil || got.MaxUnavailable.IntValue() != 0 {
		t.Fatalf("server maxUnavailable = %#v, want 0", got)
	}

	service := serverService(site, component.DefaultNamespace)
	if service.Name != ServerName("rack-a") {
		t.Fatalf("service name = %q, want %q", service.Name, ServerName("rack-a"))
	}
	if service.Spec.Selector["app.kubernetes.io/name"] != "metalman-server" || service.Spec.Selector[unboundedv1alpha3.MachineSiteLabelKey] != "rack-a" {
		t.Fatalf("service selector = %#v", service.Spec.Selector)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 8880 || service.Spec.Ports[0].TargetPort.IntValue() != 8880 {
		t.Fatalf("service ports = %#v", service.Spec.Ports)
	}

	pdb := serverPodDisruptionBudget(site, component.DefaultNamespace)
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Fatalf("PDB minAvailable = %#v, want 1", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.Selector == nil || pdb.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "metalman-server" {
		t.Fatalf("PDB selector = %#v", pdb.Spec.Selector)
	}
}

func TestEndpointEdgeWorkloadMatrix(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	cfg := component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"}

	t.Run("managed L2 is the only host-network edge", func(t *testing.T) {
		endpoint := &unboundedv1alpha3.NetbootEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "rack-a-lan"},
			Spec: unboundedv1alpha3.NetbootEndpointSpec{
				SiteRef:     site.Name,
				Type:        unboundedv1alpha3.NetbootEndpointTypeManagedL2,
				ExternalURL: "http://192.0.2.10:8880",
				ManagedL2: &unboundedv1alpha3.NetbootManagedL2Spec{
					NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"provisioning-lan": "rack-a"}},
					Interface:    "eno2",
					Address:      "192.0.2.10",
				},
			},
		}

		deployment, service, err := endpointEdgeObjects(endpoint, site, component.DefaultNamespace, cfg)
		if err != nil {
			t.Fatalf("endpointEdgeObjects: %v", err)
		}
		if deployment == nil || service != nil {
			t.Fatalf("managed L2 objects = deployment %v, service %v", deployment, service)
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
			t.Fatalf("managed L2 replicas = %v, want 1", deployment.Spec.Replicas)
		}
		pod := &deployment.Spec.Template.Spec
		if !pod.HostNetwork || pod.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
			t.Fatalf("managed L2 networking = hostNetwork %v, dnsPolicy %q", pod.HostNetwork, pod.DNSPolicy)
		}
		if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil ||
			pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			t.Fatalf("managed L2 edge lacks required placement: %#v", pod.Affinity)
		}
		container := &pod.Containers[0]
		for _, arg := range []string{
			"edge",
			"--backend-url=http://metalman-server-rack-a." + component.DefaultNamespace + ".svc:8880",
			"--endpoint=rack-a-lan",
			"--dhcp-enabled",
			"--dhcp-interface=eno2",
			"--dhcp-server-ip=192.0.2.10",
			"--tftp-enabled",
		} {
			if !containsString(container.Args, arg) {
				t.Fatalf("managed L2 args %#v lack %q", container.Args, arg)
			}
		}
		if pod.ServiceAccountName != "metalman-edge" {
			t.Fatalf("managed L2 service account = %q", pod.ServiceAccountName)
		}
		assertProjectedEdgeToken(t, pod, container)
		if !hasContainerPort(container.Ports, "http", 8880) ||
			!hasContainerPort(container.Ports, "dhcp", 67) ||
			!hasContainerPort(container.Ports, "tftp", 69) {
			t.Fatalf("managed L2 ports = %#v", container.Ports)
		}
	})

	t.Run("HTTP edge uses ordinary replicated pods and a Service", func(t *testing.T) {
		endpoint := &unboundedv1alpha3.NetbootEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "public-http"},
			Spec: unboundedv1alpha3.NetbootEndpointSpec{
				SiteRef:     site.Name,
				Type:        unboundedv1alpha3.NetbootEndpointTypeHTTP,
				ExternalURL: "https://boot.example.com",
				TLS: unboundedv1alpha3.NetbootEndpointTLS{
					Trust: unboundedv1alpha3.NetbootEndpointTrustPublic,
					Mode:  unboundedv1alpha3.NetbootEndpointTLSExternal,
				},
				HTTP: &unboundedv1alpha3.NetbootHTTPEndpointSpec{ServiceType: corev1.ServiceTypeNodePort},
			},
		}

		deployment, service, err := endpointEdgeObjects(endpoint, site, component.DefaultNamespace, cfg)
		if err != nil {
			t.Fatalf("endpointEdgeObjects: %v", err)
		}
		if deployment == nil || service == nil {
			t.Fatalf("HTTP objects = deployment %v, service %v", deployment, service)
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 {
			t.Fatalf("HTTP replicas = %v, want 2", deployment.Spec.Replicas)
		}
		assertOrdinaryPodNetworking(t, &deployment.Spec.Template.Spec)
		args := deployment.Spec.Template.Spec.Containers[0].Args
		if containsString(args, "--dhcp-enabled") || containsString(args, "--tftp-enabled") {
			t.Fatalf("HTTP edge enables L2 protocols: %#v", args)
		}
		if service.Spec.Type != corev1.ServiceTypeNodePort || service.Spec.Selector[netbootEndpointLabel] != endpoint.Name {
			t.Fatalf("HTTP edge Service = %#v", service.Spec)
		}
	})

	t.Run("Secret TLS mounts the mirrored certificate", func(t *testing.T) {
		endpoint := &unboundedv1alpha3.NetbootEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "public-https"},
			Spec: unboundedv1alpha3.NetbootEndpointSpec{
				SiteRef:     site.Name,
				Type:        unboundedv1alpha3.NetbootEndpointTypeHTTP,
				ExternalURL: "https://boot.example.com",
				TLS: unboundedv1alpha3.NetbootEndpointTLS{
					Trust: unboundedv1alpha3.NetbootEndpointTrustPublic,
					Mode:  unboundedv1alpha3.NetbootEndpointTLSSecret,
					SecretRef: &unboundedv1alpha3.NamespacedSecretReference{
						Namespace: "certificates",
						Name:      "boot-example-com",
					},
				},
				HTTP: &unboundedv1alpha3.NetbootHTTPEndpointSpec{},
			},
		}

		deployment, service, err := endpointEdgeObjects(endpoint, site, component.DefaultNamespace, cfg)
		if err != nil {
			t.Fatalf("endpointEdgeObjects: %v", err)
		}
		container := &deployment.Spec.Template.Spec.Containers[0]
		for _, arg := range []string{
			"--tls-cert-file=/var/run/secrets/metalman-tls/tls.crt",
			"--tls-key-file=/var/run/secrets/metalman-tls/tls.key",
		} {
			if !containsString(container.Args, arg) {
				t.Fatalf("Secret TLS args %#v lack %q", container.Args, arg)
			}
		}
		if len(deployment.Spec.Template.Spec.Volumes) != 1 ||
			deployment.Spec.Template.Spec.Volumes[0].Secret == nil ||
			deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName != EdgeTLSSecretName(endpoint.Name) {
			t.Fatalf("Secret TLS volumes = %#v", deployment.Spec.Template.Spec.Volumes)
		}
		if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != "/var/run/secrets/metalman-tls" || !container.VolumeMounts[0].ReadOnly {
			t.Fatalf("Secret TLS mounts = %#v", container.VolumeMounts)
		}
		if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Name != "https" || service.Spec.Ports[0].Port != 443 {
			t.Fatalf("Secret TLS Service ports = %#v", service.Spec.Ports)
		}
	})

	t.Run("external L2 has no in-cluster workload", func(t *testing.T) {
		endpoint := &unboundedv1alpha3.NetbootEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-laptop"},
			Spec: unboundedv1alpha3.NetbootEndpointSpec{
				SiteRef: site.Name,
				Type:    unboundedv1alpha3.NetbootEndpointTypeExternalL2,
			},
		}

		deployment, service, err := endpointEdgeObjects(endpoint, site, component.DefaultNamespace, cfg)
		if err != nil {
			t.Fatalf("endpointEdgeObjects: %v", err)
		}
		if deployment != nil || service != nil {
			t.Fatalf("external L2 objects = deployment %v, service %v", deployment, service)
		}
	})
}

func TestReconcileEndpointEdgesMirrorsRotatesAndRemovesTLSSecret(t *testing.T) {
	scheme := testScheme(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-example-com", Namespace: "certificates"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("certificate-v1"), corev1.TLSPrivateKeyKey: []byte("private-key-v1")},
	}
	endpoint := &unboundedv1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "public-https"},
		Spec: unboundedv1alpha3.NetbootEndpointSpec{
			SiteRef:     site.Name,
			Type:        unboundedv1alpha3.NetbootEndpointTypeHTTP,
			ExternalURL: "https://boot.example.com",
			TLS: unboundedv1alpha3.NetbootEndpointTLS{
				Trust:     unboundedv1alpha3.NetbootEndpointTrustPublic,
				Mode:      unboundedv1alpha3.NetbootEndpointTLSSecret,
				SecretRef: &unboundedv1alpha3.NamespacedSecretReference{Namespace: source.Namespace, Name: source.Name},
			},
			HTTP: &unboundedv1alpha3.NetbootHTTPEndpointSpec{},
		},
	}
	env := &component.Env{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(endpoint).WithObjects(source, endpoint).Build(),
		Scheme: scheme, Namespace: component.DefaultNamespace,
		Config: component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"},
	}

	assertMirror := func(wantCert string) string {
		t.Helper()

		if err := reconcileEndpointEdges(t.Context(), env, site); err != nil {
			t.Fatalf("reconcileEndpointEdges: %v", err)
		}
		mirror := &corev1.Secret{}
		key := client.ObjectKey{Namespace: component.DefaultNamespace, Name: EdgeTLSSecretName(endpoint.Name)}
		if err := env.Client.Get(t.Context(), key, mirror); err != nil {
			t.Fatalf("get mirrored TLS Secret: %v", err)
		}
		if got := string(mirror.Data[corev1.TLSCertKey]); got != wantCert {
			t.Fatalf("mirrored certificate = %q, want %q", got, wantCert)
		}
		if got := string(mirror.Data[corev1.TLSPrivateKeyKey]); got != "private-key-v1" {
			t.Fatalf("mirrored private key = %q", got)
		}
		deployment := &appsv1.Deployment{}
		if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: EdgeName(endpoint.Name)}, deployment); err != nil {
			t.Fatalf("get TLS edge Deployment: %v", err)
		}
		checksum := deployment.Spec.Template.Annotations[edgeTLSChecksumAnnotation]
		if checksum == "" {
			t.Fatal("TLS edge pod template lacks certificate checksum")
		}

		return checksum
	}

	firstChecksum := assertMirror("certificate-v1")
	source.Data[corev1.TLSCertKey] = []byte("certificate-v2")
	if err := env.Client.Update(t.Context(), source); err != nil {
		t.Fatalf("update source TLS Secret: %v", err)
	}
	if secondChecksum := assertMirror("certificate-v2"); secondChecksum == firstChecksum {
		t.Fatalf("TLS edge checksum did not change after certificate rotation: %q", secondChecksum)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKey{Name: endpoint.Name}, endpoint); err != nil {
		t.Fatalf("refresh endpoint: %v", err)
	}
	endpoint.Spec.TLS = unboundedv1alpha3.NetbootEndpointTLS{
		Trust: unboundedv1alpha3.NetbootEndpointTrustPublic,
		Mode:  unboundedv1alpha3.NetbootEndpointTLSExternal,
	}
	if err := env.Client.Update(t.Context(), endpoint); err != nil {
		t.Fatalf("update endpoint TLS mode: %v", err)
	}
	if err := reconcileEndpointEdges(t.Context(), env, site); err != nil {
		t.Fatalf("reconcile external TLS endpoint: %v", err)
	}
	err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: EdgeTLSSecretName(endpoint.Name)}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("mirrored TLS Secret still exists: %v", err)
	}
}

func TestTLSSecretChangeEnqueuesReferencingSite(t *testing.T) {
	scheme := testScheme(t)
	endpoint := &unboundedv1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "public-https"},
		Spec: unboundedv1alpha3.NetbootEndpointSpec{
			SiteRef: "rack-a",
			TLS: unboundedv1alpha3.NetbootEndpointTLS{
				Mode:      unboundedv1alpha3.NetbootEndpointTLSSecret,
				SecretRef: &unboundedv1alpha3.NamespacedSecretReference{Namespace: "certificates", Name: "boot-example-com"},
			},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(endpoint).Build()

	requests := requestsForTLSSecret(t.Context(), kubeClient, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "certificates",
		Name:      "boot-example-com",
	}})
	if len(requests) != 1 || requests[0].Name != "rack-a" {
		t.Fatalf("requests = %#v, want rack-a", requests)
	}
}

func TestReconcileEndpointEdgesDeletesStaleManagedWorkloads(t *testing.T) {
	scheme := testScheme(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	staleLabels := map[string]string{
		"app.kubernetes.io/component":         metalmanEdgeRole,
		unboundedv1alpha3.MachineSiteLabelKey: site.Name,
		netbootEndpointLabel:                  "removed-endpoint",
	}
	staleDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      EdgeName("removed-endpoint"),
		Namespace: component.DefaultNamespace,
		Labels:    staleLabels,
	}}
	staleService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      EdgeName("removed-endpoint"),
		Namespace: component.DefaultNamespace,
		Labels:    staleLabels,
	}}
	external := &unboundedv1alpha3.NetbootEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "external-endpoint"},
		Spec: unboundedv1alpha3.NetbootEndpointSpec{
			SiteRef: site.Name,
			Type:    unboundedv1alpha3.NetbootEndpointTypeExternalL2,
		},
	}
	env := &component.Env{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(staleDeployment, staleService, external).Build(),
		Scheme: scheme, Namespace: component.DefaultNamespace,
		Config: component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"},
	}

	if err := reconcileEndpointEdges(t.Context(), env, site); err != nil {
		t.Fatalf("reconcileEndpointEdges: %v", err)
	}
	for _, object := range []client.Object{staleDeployment, staleService} {
		err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(object), object)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("stale %T still exists: err=%v", object, err)
		}
	}
}

func TestEnsureCapabilitySecretPreservesExistingKey(t *testing.T) {
	scheme := testScheme(t)
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme:    scheme,
		Namespace: component.DefaultNamespace,
	}
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	if err := ensureCapabilitySecret(t.Context(), env, site); err != nil {
		t.Fatalf("first ensureCapabilitySecret: %v", err)
	}

	key := client.ObjectKey{Namespace: component.DefaultNamespace, Name: CapabilitySecretName("rack-a")}
	first := &corev1.Secret{}
	if err := env.Client.Get(t.Context(), key, first); err != nil {
		t.Fatalf("get first capability Secret: %v", err)
	}
	if len(first.Data[capabilitySecretKey]) != 32 {
		t.Fatalf("capability key length = %d, want 32", len(first.Data[capabilitySecretKey]))
	}

	if err := ensureCapabilitySecret(t.Context(), env, site); err != nil {
		t.Fatalf("second ensureCapabilitySecret: %v", err)
	}

	second := &corev1.Secret{}
	if err := env.Client.Get(t.Context(), key, second); err != nil {
		t.Fatalf("get second capability Secret: %v", err)
	}
	if string(second.Data[capabilitySecretKey]) != string(first.Data[capabilitySecretKey]) {
		t.Fatal("capability key changed across reconciliation")
	}
}

func TestDeploymentRespectsNamespaceAndDefaults(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	d := controllerDeployment(site, "custom-ns", component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"})
	if d.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", d.Namespace)
	}

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want default 1", d.Spec.Replicas)
	}

	if got := findEnv(d.Spec.Template.Spec.Containers[0].Env, "METALMAN_APISERVER_URL"); got != nil {
		t.Fatalf("METALMAN_APISERVER_URL env = %#v, want unset when APIServerEndpoint is empty", got)
	}
}

func TestDeploymentAllowsZeroReplicas(t *testing.T) {
	// The split roles have fixed availability semantics; the former Site-level
	// replica knob no longer scales the controller and data plane together.
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	d := serverDeployment(site, component.DefaultNamespace, component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"})
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Fatalf("server replicas = %v, want 2", d.Spec.Replicas)
	}
}

func TestCleanupDeletesDeployment(t *testing.T) {
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "metalman-controller-rack-a", Namespace: component.DefaultNamespace}}
	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(deploy).Build(),
		Namespace: component.DefaultNamespace,
	}

	if err := (Component{}).Cleanup(t.Context(), env, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(deploy), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected Deployment deleted, got err=%v", err)
	}
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}

	return nil
}

func assertOrdinaryPodNetworking(t *testing.T, podSpec *corev1.PodSpec) {
	t.Helper()

	if podSpec.HostNetwork {
		t.Fatal("pod unexpectedly uses host networking")
	}
	if podSpec.Affinity != nil && podSpec.Affinity.NodeAffinity != nil &&
		podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Fatalf("pod unexpectedly has required node affinity: %#v", podSpec.Affinity)
	}
}

func hasContainerPort(ports []corev1.ContainerPort, name string, port int32) bool {
	for _, candidate := range ports {
		if candidate.Name == name && candidate.ContainerPort == port {
			return true
		}
	}

	return false
}

func assertCapabilityKeyMount(t *testing.T, podSpec *corev1.PodSpec, container *corev1.Container, site string) {
	t.Helper()

	for _, mount := range container.VolumeMounts {
		if mount.Name == "capability-key" && mount.MountPath == "/var/run/secrets/metalman" && mount.ReadOnly {
			for _, volume := range podSpec.Volumes {
				if volume.Name == mount.Name && volume.Secret != nil && volume.Secret.SecretName == CapabilitySecretName(site) {
					return
				}
			}
		}
	}

	t.Fatalf("missing read-only capability key mount: mounts=%#v volumes=%#v", container.VolumeMounts, podSpec.Volumes)
}

func assertWorkloadHealthAndResources(t *testing.T, container *corev1.Container) {
	t.Helper()

	for name, probe := range map[string]*corev1.Probe{"liveness": container.LivenessProbe, "readiness": container.ReadinessProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Port.IntValue() != 8081 {
			t.Fatalf("%s probe = %#v, want HTTP probe on 8081", name, probe)
		}
	}
	if container.LivenessProbe.HTTPGet.Path != "/healthz" || container.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("probe paths = %q, %q", container.LivenessProbe.HTTPGet.Path, container.ReadinessProbe.HTTPGet.Path)
	}
	if container.Resources.Requests.Cpu().IsZero() || container.Resources.Requests.Memory().IsZero() ||
		container.Resources.Limits.Cpu().IsZero() || container.Resources.Limits.Memory().IsZero() {
		t.Fatalf("resources are incomplete: %#v", container.Resources)
	}
}

func assertSiteOwnerRef(t *testing.T, refs []metav1.OwnerReference, siteName, uid string) {
	t.Helper()

	if len(refs) != 1 {
		t.Fatalf("ownerReferences len = %d, want 1: %#v", len(refs), refs)
	}

	ref := refs[0]
	if ref.APIVersion != unboundedv1alpha3.GroupVersion.String() || ref.Kind != "Site" || ref.Name != siteName {
		t.Fatalf("unexpected ownerRef: %#v", ref)
	}

	if uid != "" && string(ref.UID) != uid {
		t.Fatalf("ownerRef UID = %q, want %q", ref.UID, uid)
	}

	// The reference must be a controller reference; Owns() enqueues only via
	// metav1.GetControllerOf, so a non-controller ref breaks per-site self-heal.
	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("ownerRef is not a controller reference: %#v", ref)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func assertProjectedEdgeToken(t *testing.T, pod *corev1.PodSpec, container *corev1.Container) {
	t.Helper()

	for _, mount := range container.VolumeMounts {
		if mount.Name != "edge-token" || mount.MountPath != "/var/run/secrets/metalman" || !mount.ReadOnly {
			continue
		}
		for _, volume := range pod.Volumes {
			if volume.Name != mount.Name || volume.Projected == nil || len(volume.Projected.Sources) != 1 {
				continue
			}
			token := volume.Projected.Sources[0].ServiceAccountToken
			if token != nil && token.Audience == "metalman-edge" && token.Path == "token" {
				return
			}
		}
	}

	t.Fatalf("missing audience-bound projected edge token: mounts=%#v volumes=%#v", container.VolumeMounts, pod.Volumes)
}

func assertSiteAffinity(t *testing.T, affinity *corev1.Affinity, siteName string) {
	t.Helper()

	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("missing node affinity: %#v", affinity)
	}

	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("node selector terms len = %d, want 2: %#v", len(terms), terms)
	}

	want := map[string]bool{component.SiteLabelKey: false, component.DeprecatedSiteLabelKey: false}

	for _, term := range terms {
		if len(term.MatchExpressions) != 1 {
			t.Fatalf("term must have one expression: %#v", term)
		}

		expr := term.MatchExpressions[0]
		if expr.Operator != corev1.NodeSelectorOpIn || len(expr.Values) != 1 || expr.Values[0] != siteName {
			t.Fatalf("unexpected site affinity expression: %#v", expr)
		}

		if _, ok := want[expr.Key]; !ok {
			t.Fatalf("unexpected site affinity key %q", expr.Key)
		}

		want[expr.Key] = true
	}

	for key, seen := range want {
		if !seen {
			t.Fatalf("site affinity missing key %q", key)
		}
	}
}
