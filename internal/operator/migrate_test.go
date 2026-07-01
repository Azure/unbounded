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

	// Register the pre-redesign net-group Site as unstructured so the fake
	// client can list and translate it.
	scheme.AddKnownTypeWithName(legacySiteGVK, &unstructured.Unstructured{})
	listGVK := legacySiteGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	return scheme
}

func newReaper(t *testing.T, objs ...client.Object) *LegacyReaper {
	t.Helper()

	cli := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(objs...).Build()

	return &LegacyReaper{
		Client:           cli,
		TargetNamespace:  "unbounded-system",
		LegacyNamespaces: []string{legacyKubeNamespace, legacyNetNamespace},
		SkipSecretNames:  map[string]struct{}{"unbounded-net-serving-cert": {}},
		CopyConfigMaps:   []string{"machina-config", "unbounded-storage-config"},
	}
}

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func legacySite(name string, spec map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(legacySiteGVK)
	obj.SetName(name)
	obj.Object["spec"] = spec

	return obj
}

func machinaDeployment(namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "machina-controller", Labels: map[string]string{"app": "machina-controller"}},
	}
}

func storageDaemonSet(namespace string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "unbounded-storage-supervisor", Labels: map[string]string{appNameLabel: "unbounded-storage-supervisor"}},
	}
}

func metalmanDeploymentForSite(namespace, site string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "metalman-controller-" + site,
			Labels:    map[string]string{"app": "unbounded-pxe", unboundedv1alpha3.MachineSiteLabelKey: site},
		},
	}
}

func TestDetectComponents(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		machinaDeployment(legacyKubeNamespace),
		storageDaemonSet(legacyKubeNamespace),
		metalmanDeploymentForSite(legacyKubeNamespace, "edge"),
	)

	// Cluster site: machina + storage; no metalman (its metalman is for "edge").
	cluster, err := r.detectComponents(t.Context(), clusterSiteName)
	if err != nil {
		t.Fatalf("detectComponents(cluster): %v", err)
	}

	if !componentEnabledInMap(cluster, "machina") {
		t.Fatalf("expected machina enabled on cluster site: %#v", cluster)
	}

	if !componentEnabledInMap(cluster, "storage") {
		t.Fatalf("expected storage enabled on cluster site: %#v", cluster)
	}

	if _, ok := cluster["metalman"]; ok {
		t.Fatalf("did not expect metalman on cluster site: %#v", cluster)
	}

	// Edge site: storage (every site) + metalman; NOT machina (cluster only).
	edge, err := r.detectComponents(t.Context(), "edge")
	if err != nil {
		t.Fatalf("detectComponents(edge): %v", err)
	}

	if _, ok := edge["machina"]; ok {
		t.Fatalf("did not expect machina on non-cluster site: %#v", edge)
	}

	if !componentEnabledInMap(edge, "storage") {
		t.Fatalf("expected storage enabled on edge site: %#v", edge)
	}

	if !componentEnabledInMap(edge, "metalman") {
		t.Fatalf("expected metalman enabled on edge site: %#v", edge)
	}
}

func componentEnabledInMap(components map[string]any, name string) bool {
	comp, ok := components[name].(map[string]any)
	if !ok {
		return false
	}

	enabled, _ := comp["enabled"].(bool)

	return enabled
}

func TestTranslateSitesCreatesMachinaSite(t *testing.T) {
	spec := map[string]any{
		"nodeCidrs":       []any{"10.0.0.0/16"},
		"manageCniPlugin": false,
	}

	r := newReaper(t,
		ns(legacyKubeNamespace),
		legacySite(clusterSiteName, spec),
		machinaDeployment(legacyKubeNamespace),
		storageDaemonSet(legacyKubeNamespace),
	)

	if err := r.translateSites(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("translateSites: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(newSiteGVK())

	if err := r.Get(t.Context(), client.ObjectKey{Name: clusterSiteName}, got); err != nil {
		t.Fatalf("expected translated machina site: %v", err)
	}

	// Networking spec copied verbatim.
	nodeCidrs, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "nodeCidrs")
	if len(nodeCidrs) != 1 || nodeCidrs[0] != "10.0.0.0/16" {
		t.Fatalf("nodeCidrs not preserved: %#v", nodeCidrs)
	}

	manage, found, _ := unstructured.NestedBool(got.Object, "spec", "manageCniPlugin")
	if !found || manage {
		t.Fatalf("manageCniPlugin not preserved: found=%t val=%t", found, manage)
	}

	// Components detected from running workloads.
	if enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "components", "machina", "enabled"); !enabled {
		t.Fatalf("expected machina enabled on cluster site")
	}

	if enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "components", "storage", "enabled"); !enabled {
		t.Fatalf("expected storage enabled on cluster site")
	}
}

func TestTranslateSitesDoesNotClobberExisting(t *testing.T) {
	existing := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: clusterSiteName},
		Spec:       unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"172.16.0.0/16"}},
	}

	r := newReaper(t,
		ns(legacyKubeNamespace),
		existing,
		legacySite(clusterSiteName, map[string]any{"nodeCidrs": []any{"10.0.0.0/16"}}),
	)

	if err := r.translateSites(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("translateSites: %v", err)
	}

	got := &unboundedv1alpha3.Site{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: clusterSiteName}, got); err != nil {
		t.Fatalf("get site: %v", err)
	}

	if len(got.Spec.NodeCidrs) != 1 || got.Spec.NodeCidrs[0] != "172.16.0.0/16" {
		t.Fatalf("translate clobbered an existing machina site: %#v", got.Spec.NodeCidrs)
	}
}

func TestMigrateSecretsCopiesNonAutoManaged(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: legacyKubeNamespace}, Data: map[string][]byte{"password": []byte("hunter2")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sa-token", Namespace: legacyKubeNamespace}, Type: serviceAccountTokenSecretType},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "helm-state", Namespace: legacyKubeNamespace}, Type: helmReleaseSecretType},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-serving-cert", Namespace: legacyKubeNamespace}},
	)

	if err := r.migrateSecrets(t.Context(), logr.Discard(), legacyKubeNamespace, "unbounded-system"); err != nil {
		t.Fatalf("migrateSecrets: %v", err)
	}

	var copied corev1.Secret
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "redfish-password"}, &copied); err != nil {
		t.Fatalf("expected redfish-password copied: %v", err)
	}

	if string(copied.Data["password"]) != "hunter2" {
		t.Fatalf("secret data not preserved: %q", copied.Data["password"])
	}

	for _, name := range []string{"sa-token", "helm-state", "unbounded-net-serving-cert"} {
		err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: name}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected %s NOT copied, got err=%v", name, err)
		}
	}
}

func TestMigrateSecretsIsIdempotentAndDoesNotClobber(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: legacyKubeNamespace}, Data: map[string][]byte{"password": []byte("legacy")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: "unbounded-system"}, Data: map[string][]byte{"password": []byte("already-migrated")}},
	)

	for range 2 {
		if err := r.migrateSecrets(t.Context(), logr.Discard(), legacyKubeNamespace, "unbounded-system"); err != nil {
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
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: legacyKubeNamespace}, Data: map[string]string{"config.yaml": "apiServerEndpoint: https://x"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: legacyKubeNamespace}, Data: map[string]string{"a": "b"}},
	)

	if err := r.migrateConfigMaps(t.Context(), logr.Discard(), legacyKubeNamespace, "unbounded-system"); err != nil {
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
		machineWithPasswordNS("m-legacy", legacyKubeNamespace),
		machineWithPasswordNS("m-current", "unbounded-system"),
		credentialWithSecretNS("c-legacy", legacyKubeNamespace),
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

func readyDaemonSet(namespace, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 1 << 30, DesiredNumberScheduled: 2, NumberReady: 2},
	}
}

func labeledDeployment(namespace, name, appName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{appNameLabel: appName}},
	}
}

func labeledAppDeployment(namespace, name string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
	}
}

func notReadyDeployment(namespace, name string) *appsv1.Deployment {
	one := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 0},
	}
}

func notReadyDaemonSet(namespace, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 0},
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
		ns(legacyKubeNamespace),
		labeledDeployment(legacyKubeNamespace, "machina-controller", "machina-controller"),
		labeledDeployment(legacyKubeNamespace, "orca", "orca"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: legacyKubeNamespace, Labels: map[string]string{appNameLabel: "machina-controller"}}},
	)

	component := legacyComponent{
		name:            ComponentMachina,
		legacyNamespace: legacyKubeNamespace,
		selectors:       []map[string]string{{appNameLabel: "machina-controller"}},
	}

	remaining, err := r.reapComponent(t.Context(), logr.Discard(), component)
	if err != nil {
		t.Fatalf("reapComponent: %v", err)
	}

	if remaining {
		t.Fatalf("expected no machina resources to remain")
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyKubeNamespace, Name: "machina-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected machina-controller deleted, err=%v", err)
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyKubeNamespace, Name: "orca"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("expected orca deployment untouched: %v", err)
	}
}

func TestReapOnceWaitsThenCompletesAndDeletesNamespaces(t *testing.T) {
	// Legacy net controller present but target NOT ready yet: reap must wait.
	r := newReaper(t,
		ns(legacyNetNamespace),
		ns("unbounded-system"),
		labeledDeployment(legacyNetNamespace, "unbounded-net-controller", "unbounded-net-controller"),
	)

	done, err := r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if done {
		t.Fatalf("expected not done while target net controller is missing")
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyNetNamespace, Name: "unbounded-net-controller"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("legacy controller should remain until target ready: %v", err)
	}

	// Make the target ready.
	if err := r.Create(t.Context(), readyDeployment("unbounded-system", "unbounded-net-controller")); err != nil {
		t.Fatalf("create target: %v", err)
	}

	if err := r.Create(t.Context(), readyDaemonSet("unbounded-system", "unbounded-net-node")); err != nil {
		t.Fatalf("create target ds: %v", err)
	}

	// Second pass reaps the legacy controller and issues namespace deletion, so
	// it is not yet done.
	done, err = r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce(2): %v", err)
	}

	if done {
		t.Fatalf("expected not done on the pass that deletes the legacy namespace")
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyNetNamespace, Name: "unbounded-net-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy controller reaped, err=%v", err)
	}

	// The legacy namespace must have been deleted by the reaper.
	if err := r.Get(t.Context(), client.ObjectKey{Name: legacyNetNamespace}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy namespace deleted, err=%v", err)
	}

	// The target namespace must NOT be touched.
	if err := r.Get(t.Context(), client.ObjectKey{Name: "unbounded-system"}, &corev1.Namespace{}); err != nil {
		t.Fatalf("target namespace must still exist: %v", err)
	}

	// Final pass: nothing legacy remains, so the reaper reports completion.
	done, err = r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce(3): %v", err)
	}

	if !done {
		t.Fatalf("expected done once the legacy namespace is gone")
	}
}

func TestNetTargetsPresent(t *testing.T) {
	r := newReaper(t, ns("unbounded-system"))

	present, err := r.netTargetsPresent(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("netTargetsPresent: %v", err)
	}

	if present {
		t.Fatalf("expected not present with no net workloads")
	}

	// Only the controller present: still not present.
	if err := r.Create(t.Context(), notReadyDeployment("unbounded-system", "unbounded-net-controller")); err != nil {
		t.Fatalf("create net controller: %v", err)
	}

	present, err = r.netTargetsPresent(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("netTargetsPresent: %v", err)
	}

	if present {
		t.Fatalf("expected not present with only the net controller")
	}

	// Both present but NOT Ready: present (readiness is deliberately ignored,
	// since the new net cannot become Ready until the old net frees the shared
	// host ports).
	if err := r.Create(t.Context(), notReadyDaemonSet("unbounded-system", "unbounded-net-node")); err != nil {
		t.Fatalf("create net node: %v", err)
	}

	present, err = r.netTargetsPresent(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("netTargetsPresent: %v", err)
	}

	if !present {
		t.Fatalf("expected present once both net workloads exist, even if not Ready")
	}
}

func TestReapOnceReapsNetWhenNewNetNotReady(t *testing.T) {
	// The new net workloads exist but are NOT Ready (they stay Pending until the
	// old net frees the shared host ports). Net must still be reaped: gating net
	// on readiness here would deadlock the cutover.
	r := newReaper(t,
		ns(legacyNetNamespace),
		ns("unbounded-system"),
		labeledDeployment(legacyNetNamespace, "unbounded-net-controller", "unbounded-net-controller"),
		notReadyDeployment("unbounded-system", "unbounded-net-controller"),
		notReadyDaemonSet("unbounded-system", "unbounded-net-node"),
	)

	if _, err := r.reapOnce(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyNetNamespace, Name: "unbounded-net-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy net reaped even though the new net is not Ready, err=%v", err)
	}
}

func TestStorageTargetsReadyGatesOnPerSiteDaemonSets(t *testing.T) {
	r := newReaper(t, ns("unbounded-system"))

	// No per-site storage DaemonSet yet: not ready.
	ready, err := r.storageTargetsReady(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if ready {
		t.Fatalf("expected not ready with no per-site storage DaemonSet")
	}

	// A per-site DaemonSet that is not yet Ready keeps the gate closed.
	notReady := readyDaemonSet("unbounded-system", "unbounded-storage-supervisor-cluster")
	notReady.Status.NumberReady = 0

	if err := r.Create(t.Context(), notReady); err != nil {
		t.Fatalf("create not-ready ds: %v", err)
	}

	ready, err = r.storageTargetsReady(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if ready {
		t.Fatalf("expected not ready while a per-site storage DaemonSet is not Ready")
	}

	// Once Ready, the gate opens.
	if err := r.Delete(t.Context(), notReady); err != nil {
		t.Fatalf("delete not-ready ds: %v", err)
	}

	if err := r.Create(t.Context(), readyDaemonSet("unbounded-system", "unbounded-storage-supervisor-cluster")); err != nil {
		t.Fatalf("create ready ds: %v", err)
	}

	ready, err = r.storageTargetsReady(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if !ready {
		t.Fatalf("expected ready once the per-site storage DaemonSet is Ready")
	}
}

func TestReapOnceStorageGatedOnPerSiteDaemonSet(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		storageDaemonSet(legacyKubeNamespace),
	)

	// No per-site storage DaemonSet in the target yet: storage must not reap.
	done, err := r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if done {
		t.Fatalf("expected not done while the per-site storage DaemonSet is absent")
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyKubeNamespace, Name: "unbounded-storage-supervisor"}, &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("legacy storage DaemonSet should remain until target ready: %v", err)
	}

	// Bring up the per-site storage DaemonSet (Ready): storage reaps.
	if err := r.Create(t.Context(), readyDaemonSet("unbounded-system", "unbounded-storage-supervisor-cluster")); err != nil {
		t.Fatalf("create per-site storage ds: %v", err)
	}

	if _, err := r.reapOnce(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("reapOnce(2): %v", err)
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyKubeNamespace, Name: "unbounded-storage-supervisor"}, &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy storage DaemonSet reaped, err=%v", err)
	}
}

func TestReapOnceSkipsComponentsWithoutLegacyFootprint(t *testing.T) {
	// The legacy unbounded-kube namespace contains only machina (no storage).
	// The reaper must NOT block waiting for a storage target that never exists.
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		labeledAppDeployment(legacyKubeNamespace, "machina-controller", map[string]string{"app": "machina-controller"}),
		readyDeployment("unbounded-system", "machina-controller"),
	)

	done, err := r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if done {
		t.Fatalf("expected not done on the pass that issues namespace deletion")
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyKubeNamespace, Name: "machina-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected machina reaped, err=%v", err)
	}

	if err := r.Get(t.Context(), client.ObjectKey{Name: legacyKubeNamespace}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy namespace deleted, err=%v", err)
	}
}

func TestReapOnceCompletesWhenDrained(t *testing.T) {
	// No legacy namespaces and no legacy Site CRD: the reaper is done.
	r := newReaper(t, ns("unbounded-system"))

	done, err := r.reapOnce(t.Context(), logr.Discard())
	if err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if !done {
		t.Fatalf("expected done when nothing legacy remains")
	}
}
