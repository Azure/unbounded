// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metalman

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
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
