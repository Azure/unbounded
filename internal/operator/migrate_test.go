// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func reaperScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
		unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func newReaper(t *testing.T, objs ...client.Object) *LegacyReaper {
	t.Helper()

	cli := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(objs...).Build()

	return &LegacyReaper{
		Client:           cli,
		TargetNamespace:  "unbounded-system",
		LegacyNamespaces: []string{"unbounded-kube", "unbounded-net"},
		SkipSecretNames:  map[string]struct{}{"unbounded-net-serving-cert": {}},
		CopyConfigMaps:   []string{"machina-config"},
	}
}

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestMigrateSecretsCopiesNonAutoManaged(t *testing.T) {
	r := newReaper(t,
		ns("unbounded-kube"),
		ns("unbounded-system"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: "unbounded-kube"}, Data: map[string][]byte{"password": []byte("hunter2")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sa-token", Namespace: "unbounded-kube"}, Type: serviceAccountTokenSecretType},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "helm-state", Namespace: "unbounded-kube"}, Type: helmReleaseSecretType},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-serving-cert", Namespace: "unbounded-kube"}},
	)

	if err := r.migrateSecrets(t.Context(), logr.Discard(), "unbounded-kube", "unbounded-system"); err != nil {
		t.Fatalf("migrateSecrets: %v", err)
	}

	// The user secret must be copied verbatim.
	var copied corev1.Secret
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "redfish-password"}, &copied); err != nil {
		t.Fatalf("expected redfish-password copied: %v", err)
	}

	if string(copied.Data["password"]) != "hunter2" {
		t.Fatalf("secret data not preserved: %q", copied.Data["password"])
	}

	// Auto-managed and skip-listed secrets must NOT be copied.
	for _, name := range []string{"sa-token", "helm-state", "unbounded-net-serving-cert"} {
		err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: name}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected %s NOT copied, got err=%v", name, err)
		}
	}
}

func TestMigrateSecretsIsIdempotentAndDoesNotClobber(t *testing.T) {
	r := newReaper(t,
		ns("unbounded-kube"),
		ns("unbounded-system"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: "unbounded-kube"}, Data: map[string][]byte{"password": []byte("legacy")}},
		// A pre-existing copy in target must win and not be overwritten.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: "unbounded-system"}, Data: map[string][]byte{"password": []byte("already-migrated")}},
	)

	for range 2 {
		if err := r.migrateSecrets(t.Context(), logr.Discard(), "unbounded-kube", "unbounded-system"); err != nil {
			t.Fatalf("migrateSecrets: %v", err)
		}
	}

	var got corev1.Secret
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "redfish-password"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if string(got.Data["password"]) != "already-migrated" {
		t.Fatalf("create-if-absent clobbered existing target secret: %q", got.Data["password"])
	}
}

func TestMigrateConfigMapsCopiesNamedOnly(t *testing.T) {
	r := newReaper(t,
		ns("unbounded-kube"),
		ns("unbounded-system"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: "unbounded-kube"}, Data: map[string]string{"config.yaml": "apiServerEndpoint: https://x"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "unbounded-kube"}, Data: map[string]string{"a": "b"}},
	)

	if err := r.migrateConfigMaps(t.Context(), logr.Discard(), "unbounded-kube", "unbounded-system"); err != nil {
		t.Fatalf("migrateConfigMaps: %v", err)
	}

	var cm corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "machina-config"}, &cm); err != nil {
		t.Fatalf("expected machina-config copied: %v", err)
	}

	if cm.Data["config.yaml"] != "apiServerEndpoint: https://x" {
		t.Fatalf("configmap data not preserved: %q", cm.Data["config.yaml"])
	}

	err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "unrelated"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected unrelated configmap NOT copied, got err=%v", err)
	}
}

func machineWithPasswordNS(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Machine"})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, namespace, "spec", "pxe", "redfish", "passwordRef", "namespace")
	_ = unstructured.SetNestedField(obj.Object, "redfish-password", "spec", "pxe", "redfish", "passwordRef", "name")

	return obj
}

func credentialWithSecretNS(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "MachineOperationCredential"})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, namespace, "spec", "auth", "secretRef", "namespace")

	return obj
}

func TestRewriteClusterScopedRefs(t *testing.T) {
	r := newReaper(t,
		machineWithPasswordNS("m-legacy", "unbounded-kube"),
		machineWithPasswordNS("m-current", "unbounded-system"),
		credentialWithSecretNS("c-legacy", "unbounded-kube"),
	)

	if err := r.rewriteClusterScopedRefs(t.Context(), logr.Discard(), "unbounded-system"); err != nil {
		t.Fatalf("rewriteClusterScopedRefs: %v", err)
	}

	assertNestedString(t, r, "Machine", "m-legacy", "unbounded-system", "spec", "pxe", "redfish", "passwordRef", "namespace")
	assertNestedString(t, r, "Machine", "m-current", "unbounded-system", "spec", "pxe", "redfish", "passwordRef", "namespace")
	assertNestedString(t, r, "MachineOperationCredential", "c-legacy", "unbounded-system", "spec", "auth", "secretRef", "namespace")
}

func assertNestedString(t *testing.T, r *LegacyReaper, kind, name, want string, path ...string) {
	t.Helper()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: kind})

	if err := r.Get(t.Context(), client.ObjectKey{Name: name}, obj); err != nil {
		t.Fatalf("get %s/%s: %v", kind, name, err)
	}

	got, found, err := unstructured.NestedString(obj.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s/%s missing %v: found=%t err=%v", kind, name, path, found, err)
	}

	if got != want {
		t.Fatalf("%s/%s %v = %q, want %q", kind, name, path, got, want)
	}
}

func readyDeployment(namespace, name string) *appsv1.Deployment {
	one := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
}

func labeledDeployment(namespace, name, appName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{appNameLabel: appName}},
	}
}

func TestTargetsReadyGating(t *testing.T) {
	r := newReaper(t, readyDeployment("unbounded-system", "machina-controller"))

	ready, err := r.targetsReady(t.Context(), "unbounded-system", []workloadRef{{kind: "Deployment", name: "machina-controller"}})
	if err != nil {
		t.Fatalf("targetsReady: %v", err)
	}

	if !ready {
		t.Fatalf("expected ready when target deployment is available")
	}

	missing, err := r.targetsReady(t.Context(), "unbounded-system", []workloadRef{{kind: "Deployment", name: "absent"}})
	if err != nil {
		t.Fatalf("targetsReady: %v", err)
	}

	if missing {
		t.Fatalf("expected not-ready when target deployment is absent")
	}
}

func TestReapComponentDeletesByLabelOnly(t *testing.T) {
	r := newReaper(t,
		ns("unbounded-kube"),
		labeledDeployment("unbounded-kube", "machina-controller", "machina-controller"),
		// Same namespace, different app: must survive.
		labeledDeployment("unbounded-kube", "orca", "orca"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: "unbounded-kube", Labels: map[string]string{appNameLabel: "machina-controller"}}},
	)

	component := legacyComponent{
		name:            ComponentMachina,
		legacyNamespace: "unbounded-kube",
		selectors:       []map[string]string{{appNameLabel: "machina-controller"}},
	}

	remaining, err := r.reapComponent(t.Context(), logr.Discard(), component)
	if err != nil {
		t.Fatalf("reapComponent: %v", err)
	}

	if remaining {
		t.Fatalf("expected no machina resources to remain")
	}

	// machina-controller deployment + configmap gone.
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-kube", Name: "machina-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected machina-controller deleted, err=%v", err)
	}

	// Unrelated workload (orca) must NOT be touched.
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-kube", Name: "orca"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("expected orca deployment untouched: %v", err)
	}
}

func TestReapOnceWaitsThenCompletesAndKeepsNamespaces(t *testing.T) {
	// Legacy net controller present but target NOT ready yet: reap must wait.
	r := newReaper(t,
		ns("unbounded-net"),
		ns("unbounded-system"),
		labeledDeployment("unbounded-net", "unbounded-net-controller", "unbounded-net-controller"),
	)

	done, err := r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if done {
		t.Fatalf("expected not done while target net controller is missing")
	}

	// Legacy controller must still be present (not reaped before target ready).
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-net", Name: "unbounded-net-controller"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("legacy controller should remain until target ready: %v", err)
	}

	// Now make the target ready.
	if err := r.Create(t.Context(), readyDeployment("unbounded-system", "unbounded-net-controller")); err != nil {
		t.Fatalf("create target: %v", err)
	}

	if err := r.Create(t.Context(), readyDaemonSet("unbounded-system", "unbounded-net-node")); err != nil {
		t.Fatalf("create target ds: %v", err)
	}

	done, err = r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce(2): %v", err)
	}

	if !done {
		t.Fatalf("expected done once targets ready and legacy reaped")
	}

	// Legacy controller now reaped.
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-net", Name: "unbounded-net-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy controller reaped, err=%v", err)
	}

	// CRITICAL: the legacy Namespace objects must NEVER be deleted by the reaper.
	for _, name := range []string{"unbounded-net", "unbounded-system"} {
		if err := r.Get(t.Context(), client.ObjectKey{Name: name}, &corev1.Namespace{}); err != nil {
			t.Fatalf("namespace %s must still exist (reaper must not delete namespaces): %v", name, err)
		}
	}
}

func readyDaemonSet(namespace, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2},
	}
}

func TestReapOnceSkipsComponentsWithoutLegacyFootprint(t *testing.T) {
	// The legacy unbounded-kube namespace exists but contains only machina (no
	// storage). The reaper must NOT block waiting for a storage target workload
	// that will never exist.
	r := newReaper(t,
		ns("unbounded-kube"),
		ns("unbounded-system"),
		labeledAppDeployment("unbounded-kube", "machina-controller", map[string]string{"app": "machina-controller"}),
		readyDeployment("unbounded-system", "machina-controller"),
	)

	done, err := r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if !done {
		t.Fatalf("expected done: storage has no legacy footprint and must not gate completion")
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-kube", Name: "machina-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected machina reaped, err=%v", err)
	}
}

func labeledAppDeployment(namespace, name string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
	}
}
