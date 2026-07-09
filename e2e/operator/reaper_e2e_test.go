//go:build e2e

// Package operatore2e holds the kind-based integration test for the
// unbounded-operator legacy reaper.
//
// It is guarded by `//go:build e2e` so the default `go test ./...` skips it.
// Run via `go test -tags=e2e ./e2e/operator/...`.
//
// Rather than deploy the whole operator image (whose Site reconcile would apply
// the real net/machina/storage workloads that cannot run in vanilla kind), the
// test drives the real LegacyReaper in-process against a real kube-apiserver.
// The pre-redesign install is staged as inert `pause` workloads and the new
// target workloads are staged Ready, so the migration choreography (Site
// translation, secret/configmap copy, cluster-scoped ref rewrite, health-gated
// reaping, and namespace + CRD teardown) is validated against real API
// semantics (finalizers, cascading namespace deletion, DaemonSet readiness).
package operatore2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator"
)

const (
	clusterName = "operator-reaper-e2e"
	pauseImage  = "registry.k8s.io/pause:3.10"
	targetNS    = "unbounded-system"
	legacyKube  = "unbounded-kube"
	legacyNet   = "unbounded-net"
)

var (
	legacySiteGVK     = schema.GroupVersionKind{Group: "net.unbounded-cloud.io", Version: "v1alpha1", Kind: "Site"}
	machinaSiteGVK    = schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Site"}
	machineGVK        = schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Machine"}
	legacySiteCRD     = "sites.net.unbounded-cloud.io"
	machinaSiteCRD    = "sites.unbounded-cloud.io"
	machineCRDName    = "machines.unbounded-cloud.io"
	legacySiteCRDYAML = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: sites.net.unbounded-cloud.io
spec:
  group: net.unbounded-cloud.io
  scope: Cluster
  names:
    plural: sites
    singular: site
    kind: Site
    listKind: SiteList
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              x-kubernetes-preserve-unknown-fields: true
`
)

func TestOperatorReaperMigration(t *testing.T) {
	requireBins(t, "kind", "kubectl", "docker")

	if err := run(context.Background(), "docker", "info"); err != nil {
		t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}

	repoRoot := repoRootFromWD(t)
	kubeconfig := createCluster(t)

	applyCRDs(t, kubeconfig, repoRoot)

	cli := newClient(t, kubeconfig)
	ctx := log.IntoContext(context.Background(), zap.New(zap.UseDevMode(true)))

	stageNamespaces(ctx, t, cli)
	stageLegacyWorkloads(ctx, t, cli)
	stageLegacyState(ctx, t, cli)
	stageLegacySites(ctx, t, cli)
	stageMachine(ctx, t, cli)
	stageTargetWorkloads(ctx, t, cli)

	reaper := &operator.LegacyReaper{
		Client:           cli,
		TargetNamespace:  targetNS,
		LegacyNamespaces: operator.LegacyNamespaces,
		SkipSecretNames:  map[string]struct{}{"unbounded-net-serving-cert": {}},
		CopyConfigMaps:   []string{"machina-config"},
		Interval:         2 * time.Second,
	}

	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	if err := reaper.RunToCompletion(runCtx); err != nil {
		t.Fatalf("reaper RunToCompletion: %v", err)
	}

	assertTranslatedSites(ctx, t, cli)
	assertStateMigrated(ctx, t, cli)
	assertMachineRefRewritten(ctx, t, cli)
	assertLegacyReaped(ctx, t, cli)
}

// ---------------------------------------------------------------------------
// staging
// ---------------------------------------------------------------------------

func stageNamespaces(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	for _, name := range []string{targetNS, legacyKube, legacyNet} {
		mustCreate(ctx, t, cli, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
}

func stageLegacyWorkloads(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	// Legacy workloads only need to EXIST to have a footprint; keep them inert
	// (0 replicas / unschedulable DaemonSets) so kind stays light.
	mustCreate(ctx, t, cli, inertDeployment(legacyKube, "machina-controller", map[string]string{"app": "machina-controller"}))
	mustCreate(ctx, t, cli, inertDeployment(legacyKube, "metalman-controller-edge", map[string]string{"app": "unbounded-pxe", unboundedv1alpha3.MachineSiteLabelKey: "edge"}))
	mustCreate(ctx, t, cli, unschedulableDaemonSet(legacyKube, "unbounded-storage-supervisor", map[string]string{"app.kubernetes.io/name": "unbounded-storage-supervisor"}))
	mustCreate(ctx, t, cli, inertDeployment(legacyNet, "unbounded-net-controller", map[string]string{"app.kubernetes.io/name": "unbounded-net-controller"}))
	mustCreate(ctx, t, cli, unschedulableDaemonSet(legacyNet, "unbounded-net-node", map[string]string{"app.kubernetes.io/name": "unbounded-net-node"}))
}

func stageLegacyState(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	mustCreate(ctx, t, cli, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "redfish-password"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	})
	// A regenerable secret that must NOT be copied.
	mustCreate(ctx, t, cli, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyNet, Name: "unbounded-net-serving-cert"},
		Data:       map[string][]byte{"tls.crt": []byte("x")},
	})
	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://api.example:6443"},
	})
	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "unbounded-storage-config"},
		Data:       map[string]string{"config.yaml": "log_level: info"},
	})
}

func stageLegacySites(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	mustCreate(ctx, t, cli, legacySite("cluster", "10.0.0.0/16", "10.244.0.0/16"))
	mustCreate(ctx, t, cli, legacySite("edge", "10.1.0.0/16", "10.245.0.0/16"))
}

func stageMachine(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)
	m.SetName("m1")
	mustSetNested(t, m, "example/pxe-image:v1", "spec", "pxe", "image")
	mustSetNested(t, m, "https://bmc.example", "spec", "pxe", "redfish", "url")
	mustSetNested(t, m, "admin", "spec", "pxe", "redfish", "username")
	mustSetNested(t, m, legacyKube, "spec", "pxe", "redfish", "passwordRef", "namespace")
	mustSetNested(t, m, "redfish-password", "spec", "pxe", "redfish", "passwordRef", "name")
	// Cloud-init user-data ConfigMap ref in a legacy namespace: the reaper must
	// copy the ConfigMap into the target and repoint this ref.
	mustSetNested(t, m, "cloud-init-user-data", "spec", "pxe", "cloudInit", "userDataConfigMapRef", "name")
	mustSetNested(t, m, legacyKube, "spec", "pxe", "cloudInit", "userDataConfigMapRef", "namespace")
	mustCreate(ctx, t, cli, m)

	mustCreate(ctx, t, cli, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKube, Name: "cloud-init-user-data"},
		Data:       map[string]string{"user-data": "#cloud-config"},
	})
}

func stageTargetWorkloads(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	// New target workloads run as pause pods so they actually become Ready in
	// kind, satisfying the reaper's per-component health gate.
	mustCreate(ctx, t, cli, readyDeployment(targetNS, "machina-controller"))
	mustCreate(ctx, t, cli, readyDeployment(targetNS, "unbounded-net-controller"))
	mustCreate(ctx, t, cli, readyDaemonSet(targetNS, "unbounded-net-node"))
	mustCreate(ctx, t, cli, readyDaemonSet(targetNS, "unbounded-storage-supervisor-cluster"))
	mustCreate(ctx, t, cli, readyDaemonSet(targetNS, "unbounded-storage-supervisor-edge"))
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

func assertTranslatedSites(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	cluster := getMachinaSite(ctx, t, cli, "cluster")
	if cidrs, _, _ := unstructured.NestedStringSlice(cluster.Object, "spec", "nodeCidrs"); len(cidrs) != 1 || cidrs[0] != "10.0.0.0/16" {
		t.Fatalf("cluster site nodeCidrs not preserved: %#v", cidrs)
	}

	if !nestedBool(cluster, "spec", "components", "machina", "enabled") {
		t.Fatalf("expected machina enabled on cluster site")
	}

	if !nestedBool(cluster, "spec", "components", "storage", "enabled") {
		t.Fatalf("expected storage enabled on cluster site")
	}

	edge := getMachinaSite(ctx, t, cli, "edge")
	if nestedBool(edge, "spec", "components", "machina", "enabled") {
		t.Fatalf("did not expect machina enabled on edge site")
	}

	if !nestedBool(edge, "spec", "components", "storage", "enabled") {
		t.Fatalf("expected storage enabled on edge site")
	}

	if !nestedBool(edge, "spec", "components", "metalman", "enabled") {
		t.Fatalf("expected metalman enabled on edge site")
	}
}

func assertStateMigrated(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	var secret corev1.Secret
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "redfish-password"}, &secret); err != nil {
		t.Fatalf("expected redfish-password copied to target: %v", err)
	}

	if string(secret.Data["password"]) != "hunter2" {
		t.Fatalf("secret data not preserved: %q", secret.Data["password"])
	}

	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-net-serving-cert"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("regenerable serving cert must NOT be copied, err=%v", err)
	}

	// machina-config is copied by name; storage config is copied into the
	// operator-managed per-site ConfigMaps that storage DaemonSets mount.
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "machina-config"}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("expected machina-config copied to target: %v", err)
	}

	for _, name := range []string{"unbounded-storage-config-cluster", "unbounded-storage-config-edge"} {
		var cm corev1.ConfigMap
		if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: name}, &cm); err != nil {
			t.Fatalf("expected per-site storage config %s copied to target: %v", name, err)
		}

		if cm.Data["config.yaml"] != "log_level: info" {
			t.Fatalf("storage config %s data not preserved: %q", name, cm.Data["config.yaml"])
		}
	}

	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "unbounded-storage-config"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("shared storage config must NOT be copied under the legacy name, err=%v", err)
	}

	// The Machine cloud-init ConfigMap is copied out of the legacy namespace.
	var cloudInit corev1.ConfigMap
	if err := cli.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: "cloud-init-user-data"}, &cloudInit); err != nil {
		t.Fatalf("expected cloud-init configmap copied to target: %v", err)
	}

	if cloudInit.Data["user-data"] != "#cloud-config" {
		t.Fatalf("cloud-init configmap data not preserved: %q", cloudInit.Data["user-data"])
	}
}

func assertMachineRefRewritten(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)

	if err := cli.Get(ctx, client.ObjectKey{Name: "m1"}, m); err != nil {
		t.Fatalf("get machine: %v", err)
	}

	got, _, _ := unstructured.NestedString(m.Object, "spec", "pxe", "redfish", "passwordRef", "namespace")
	if got != targetNS {
		t.Fatalf("machine passwordRef namespace = %q, want %q", got, targetNS)
	}

	cloudInitNS, _, _ := unstructured.NestedString(m.Object, "spec", "pxe", "cloudInit", "userDataConfigMapRef", "namespace")
	if cloudInitNS != targetNS {
		t.Fatalf("machine cloud-init configmap namespace = %q, want %q", cloudInitNS, targetNS)
	}
}

func assertLegacyReaped(ctx context.Context, t *testing.T, cli client.Client) {
	t.Helper()

	for _, name := range []string{legacyKube, legacyNet} {
		if err := cli.Get(ctx, client.ObjectKey{Name: name}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected legacy namespace %s deleted, err=%v", name, err)
		}
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := cli.Get(ctx, client.ObjectKey{Name: legacySiteCRD}, crd); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy site CRD deleted, err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// object builders
// ---------------------------------------------------------------------------

func inertDeployment(namespace, name string, labels map[string]string) *appsv1.Deployment {
	zero := int32(0)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTemplate(labels),
		},
	}
}

func unschedulableDaemonSet(namespace, name string, labels map[string]string) *appsv1.DaemonSet {
	tmpl := podTemplate(labels)
	tmpl.Spec.NodeSelector = map[string]string{"operator-reaper-e2e/absent": "true"}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: tmpl,
		},
	}
}

func readyDeployment(namespace, name string) *appsv1.Deployment {
	one := int32(1)
	labels := map[string]string{"app.kubernetes.io/name": name}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTemplate(labels),
		},
	}
}

func readyDaemonSet(namespace, name string) *appsv1.DaemonSet {
	labels := map[string]string{"app.kubernetes.io/name": name}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTemplate(labels),
		},
	}
}

func podTemplate(labels map[string]string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "pause", Image: pauseImage}},
		},
	}
}

func legacySite(name, nodeCidr, podCidr string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(legacySiteGVK)
	obj.SetName(name)
	obj.Object["spec"] = map[string]any{
		"nodeCidrs": []any{nodeCidr},
		"podCidrAssignments": []any{
			map[string]any{"cidrBlocks": []any{podCidr}},
		},
	}

	return obj
}

func getMachinaSite(ctx context.Context, t *testing.T, cli client.Client, name string) *unstructured.Unstructured {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(machinaSiteGVK)

	if err := cli.Get(ctx, client.ObjectKey{Name: name}, obj); err != nil {
		t.Fatalf("expected translated machina site %s: %v", name, err)
	}

	return obj
}

func nestedBool(obj *unstructured.Unstructured, path ...string) bool {
	v, _, _ := unstructured.NestedBool(obj.Object, path...)

	return v
}

// ---------------------------------------------------------------------------
// cluster + client lifecycle
// ---------------------------------------------------------------------------

func createCluster(t *testing.T) string {
	t.Helper()

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")

	if err := run(context.Background(), "kind", "create", "cluster", "--name", clusterName, "--wait", "120s", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("kind create cluster: %v", err)
	}

	t.Cleanup(func() {
		if os.Getenv("E2E_KEEP") == "1" {
			t.Logf("E2E_KEEP=1; leaving kind cluster %q", clusterName)
			return
		}

		_ = run(context.Background(), "kind", "delete", "cluster", "--name", clusterName)
	})

	return kubeconfig
}

func applyCRDs(t *testing.T, kubeconfig, repoRoot string) {
	t.Helper()

	crdFile := filepath.Join(t.TempDir(), "legacy-site-crd.yaml")
	if err := os.WriteFile(crdFile, []byte(legacySiteCRDYAML), 0o600); err != nil {
		t.Fatalf("write legacy site crd: %v", err)
	}

	files := []string{
		filepath.Join(repoRoot, "deploy", "machina", "crd", "unbounded-cloud.io_sites.yaml"),
		filepath.Join(repoRoot, "deploy", "machina", "crd", "unbounded-cloud.io_machines.yaml"),
		crdFile,
	}

	for _, f := range files {
		if err := run(context.Background(), "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", f); err != nil {
			t.Fatalf("kubectl apply %s: %v", f, err)
		}
	}

	if err := run(context.Background(), "kubectl", "--kubeconfig", kubeconfig, "wait", "--for=condition=established", "--timeout=60s",
		"crd/"+machinaSiteCRD, "crd/"+machineCRDName, "crd/"+legacySiteCRD); err != nil {
		t.Fatalf("kubectl wait for CRDs: %v", err)
	}
}

func newClient(t *testing.T, kubeconfig string) client.Client {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build rest config: %v", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		apiextensionsv1.AddToScheme,
		unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	return cli
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustCreate(ctx context.Context, t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()

	if err := cli.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func mustSetNested(t *testing.T, obj *unstructured.Unstructured, value string, path ...string) {
	t.Helper()

	if err := unstructured.SetNestedField(obj.Object, value, path...); err != nil {
		t.Fatalf("set nested %v: %v", path, err)
	}
}

func requireBins(t *testing.T, bins ...string) {
	t.Helper()

	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("e2e prereq %q missing on PATH; skipping suite", bin)
		}
	}
}

func repoRootFromWD(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	// e2e/operator -> repo root
	return filepath.Dir(filepath.Dir(wd))
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
