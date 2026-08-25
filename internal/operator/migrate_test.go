// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func reaperScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		batchv1.AddToScheme,
		rbacv1.AddToScheme,
		apiextensionsv1.AddToScheme,
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
	scheme.AddKnownTypeWithName(siteNodeSliceGVK, &unstructured.Unstructured{})
	sliceListGVK := siteNodeSliceGVK
	sliceListGVK.Kind += "List"
	scheme.AddKnownTypeWithName(sliceListGVK, &unstructured.UnstructuredList{})

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
		CopyConfigMaps:   []string{"machina-config", "unbounded-net-config"},
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
			// Match the v0.1.19 deploy-pxe Deployment label shape. This is the
			// supported released-version upgrade baseline for the operator reaper.
			Labels: map[string]string{"app": "unbounded-pxe", unboundedv1alpha3.MachineSiteLabelKey: site},
		},
	}
}

func metalmanDeploymentForSiteWithArgs(namespace, site string, args ...string) *appsv1.Deployment {
	deploy := metalmanDeploymentForSite(namespace, site)
	deploy.Spec.Template.Spec.Containers = []corev1.Container{{Name: "metalman", Args: args}}

	return deploy
}

func TestDetectComponentsMatchesV019MetalmanLabels(t *testing.T) {
	r := newReaper(t, metalmanDeploymentForSite(legacyKubeNamespace, "edge"))

	components, err := r.detectComponents(t.Context(), "edge")
	if err != nil {
		t.Fatalf("detectComponents: %v", err)
	}

	if !componentEnabledInMap(components, "metalman") {
		t.Fatalf("expected v0.1.19 metalman labels to enable metalman: %#v", components)
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

func TestTranslateSitesPreservesMetalmanDHCPAutoInterface(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want bool
	}{
		{name: "bare", arg: "--dhcp-auto-interface", want: true},
		{name: "explicit true", arg: "--dhcp-auto-interface=true", want: true},
		{name: "explicit false", arg: "--dhcp-auto-interface=false", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReaper(t,
				legacySite("edge", map[string]any{"nodeCidrs": []any{"10.0.0.0/16"}}),
				metalmanDeploymentForSiteWithArgs(legacyKubeNamespace, "edge", "serve-pxe", tt.arg),
			)

			if err := r.translateSites(t.Context(), logr.Discard()); err != nil {
				t.Fatalf("translateSites: %v", err)
			}

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(newSiteGVK())

			if err := r.Get(t.Context(), client.ObjectKey{Name: "edge"}, got); err != nil {
				t.Fatalf("get translated Site: %v", err)
			}

			value, found, err := unstructured.NestedBool(got.Object, "spec", "components", "metalman", "dhcpAutoInterface")
			if err != nil || !found || value != tt.want {
				t.Fatalf("dhcpAutoInterface = %t, found=%t, err=%v; want %t", value, found, err, tt.want)
			}
		})
	}
}

func TestMetalmanDHCPAutoInterface(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    *bool
		wantErr bool
	}{
		{name: "absent", args: []string{"serve-pxe"}},
		{name: "bare", args: []string{"--dhcp-auto-interface"}, want: boolPtr(true)},
		{name: "explicit true", args: []string{"--dhcp-auto-interface=true"}, want: boolPtr(true)},
		{name: "explicit false", args: []string{"--dhcp-auto-interface=false"}, want: boolPtr(false)},
		{name: "repeated same value", args: []string{"--dhcp-auto-interface", "--dhcp-auto-interface=true"}, want: boolPtr(true)},
		{name: "invalid", args: []string{"--dhcp-auto-interface=yes"}, wantErr: true},
		{name: "conflicting", args: []string{"--dhcp-auto-interface", "--dhcp-auto-interface=false"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := metalmanDeploymentForSiteWithArgs(legacyKubeNamespace, "edge", tt.args...)

			got, err := metalmanDHCPAutoInterface(deploy)
			if (err != nil) != tt.wantErr {
				t.Fatalf("metalmanDHCPAutoInterface error = %v, wantErr %t", err, tt.wantErr)
			}

			if tt.wantErr {
				return
			}

			if got == nil || tt.want == nil {
				if got != nil || tt.want != nil {
					t.Fatalf("metalmanDHCPAutoInterface = %v, want %v", got, tt.want)
				}

				return
			}

			if *got != *tt.want {
				t.Fatalf("metalmanDHCPAutoInterface = %t, want %t", *got, *tt.want)
			}
		})
	}
}

func TestTranslateSiteValidatesExistingNetworkingSpec(t *testing.T) {
	for _, tc := range []struct {
		name         string
		targetCIDR   string
		wantConflict bool
	}{
		{name: "matching target succeeds", targetCIDR: "10.0.0.0/16"},
		{name: "mismatched target conflicts", targetCIDR: "172.16.0.0/16", wantConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := legacySite("edge", map[string]any{
				"nodeCidrs":          []any{tc.targetCIDR},
				"podCidrAssignments": []any{},
				"components": map[string]any{
					"storage": map[string]any{"enabled": true},
				},
			})
			target.SetGroupVersionKind(newSiteGVK())

			source := legacySite("edge", map[string]any{
				"nodeCidrs":          []any{"10.0.0.0/16"},
				"podCidrAssignments": []any{},
			})
			r := newReaper(t, target, source)

			err := r.translateSites(t.Context(), logr.Discard())
			if tc.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("translateSites error = %v, want conflict", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("translateSites: %v", err)
			}
		})
	}
}

func TestTranslateSiteCreateRaceValidatesSpec(t *testing.T) {
	for _, tc := range []struct {
		name         string
		targetCIDR   string
		wantConflict bool
	}{
		{name: "matching target succeeds", targetCIDR: "10.0.0.0/16"},
		{name: "mismatched target conflicts", targetCIDR: "172.16.0.0/16", wantConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := legacySite("edge", map[string]any{
				"nodeCidrs":          []any{tc.targetCIDR},
				"podCidrAssignments": []any{},
				"components":         map[string]any{},
			})
			target.SetGroupVersionKind(newSiteGVK())
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(target).Build()
			gets := 0
			r := &LegacyReaper{Client: base}
			r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == "edge" && obj.GetObjectKind().GroupVersionKind() == newSiteGVK() {
						gets++
						if gets == 1 {
							return apierrors.NewNotFound(schema.GroupResource{Group: newSiteGVK().Group, Resource: "sites"}, key.Name)
						}
					}

					return underlying.Get(ctx, key, obj, opts...)
				},
			})

			err := r.translateSite(t.Context(), logr.Discard(), legacySite("edge", map[string]any{
				"nodeCidrs":          []any{"10.0.0.0/16"},
				"podCidrAssignments": []any{},
			}))
			if tc.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("translateSite error = %v, want conflict", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("translateSite: %v", err)
			}
		})
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

func TestMigrateSecretsValidatesExistingTarget(t *testing.T) {
	immutable := true

	for _, tc := range []struct {
		name         string
		target       *corev1.Secret
		wantConflict bool
	}{
		{
			name: "matching",
			target: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: "unbounded-system"},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{"password": []byte("legacy")},
				Immutable:  &immutable,
			},
		},
		{
			name: "conflicting",
			target: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: "unbounded-system"},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{"password": []byte("different")},
				Immutable:  &immutable,
			},
			wantConflict: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "redfish-password", Namespace: legacyKubeNamespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{"password": []byte("legacy")},
				Immutable:  &immutable,
			}
			r := newReaper(t, source, tc.target)

			err := r.migrateSecrets(t.Context(), logr.Discard(), legacyKubeNamespace, "unbounded-system")
			if tc.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("migrateSecrets error = %v, want conflict", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("migrateSecrets: %v", err)
			}
		})
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

func TestMigrateConfigMapsPreservesNetConfigPayload(t *testing.T) {
	legacy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-config", Namespace: legacyNetNamespace},
		Data:       map[string]string{"config.yaml": "sentinel: legacy", "LOG_LEVEL": "7"},
		BinaryData: map[string][]byte{"routes.bin": {0, 4, 2}},
	}
	defaultTarget := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: legacy.Name, Namespace: "unbounded-system"},
		Data:       map[string]string{"config.yaml": "embedded: default"},
	}
	r := newReaper(t, legacy, defaultTarget)

	if err := r.migrateConfigMaps(t.Context(), logr.Discard(), legacyNetNamespace, "unbounded-system"); err != nil {
		t.Fatalf("migrateConfigMaps: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(defaultTarget), &got); err != nil {
		t.Fatalf("get migrated net config: %v", err)
	}

	if got.Data["config.yaml"] != "sentinel: legacy" || got.Data["LOG_LEVEL"] != "7" || string(got.BinaryData["routes.bin"]) != string([]byte{0, 4, 2}) {
		t.Fatalf("migrated net payload = data=%#v binaryData=%#v", got.Data, got.BinaryData)
	}
}

func TestMigrateConfigMapsReadsLiveSource(t *testing.T) {
	stale := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-config", Namespace: legacyNetNamespace},
		Data:       map[string]string{"config.yaml": "stale: true"},
	}
	live := stale.DeepCopy()
	live.Data["config.yaml"] = "live: true"

	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(stale).Build()
	r := &LegacyReaper{
		Client: base,
		APIReader: interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key == client.ObjectKeyFromObject(live) {
					live.DeepCopyInto(obj.(*corev1.ConfigMap))

					return nil
				}

				return underlying.Get(ctx, key, obj, opts...)
			},
		}),
		CopyConfigMaps: []string{"unbounded-net-config"},
	}

	if err := r.migrateConfigMaps(t.Context(), logr.Discard(), legacyNetNamespace, "unbounded-system"); err != nil {
		t.Fatalf("migrateConfigMaps: %v", err)
	}

	var got corev1.ConfigMap
	if err := base.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: live.Name}, &got); err != nil {
		t.Fatalf("get copied config: %v", err)
	}

	if got.Data["config.yaml"] != "live: true" {
		t.Fatalf("copied config = %q, want live source payload", got.Data["config.yaml"])
	}
}

func TestCreateIfAbsentConfirmsAlreadyExistingTargetLive(t *testing.T) {
	wantErr := errors.New("live target lookup failed")
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "unbounded-system"},
	}).Build()
	r := &LegacyReaper{Client: base}
	r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return wantErr
		},
	})

	err := r.createIfAbsent(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "unbounded-system"},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("createIfAbsent error = %v, want live-reader error %v", err, wantErr)
	}
}

// TestCreateIfAbsentDetectsSubsetLiveTarget guards against a target whose live
// payload is a strict subset of the desired payload. The real controller-runtime
// client decodes a Get into the caller's object by merging into (not resetting)
// its maps, so createIfAbsent must Get into a freshly zeroed object. The fake
// client zeros the target on Get and would hide the bug, so the interceptor here
// reproduces the real client's merge-into-existing-maps semantics.
func TestCreateIfAbsentDetectsSubsetLiveTarget(t *testing.T) {
	liveTarget := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-config", Namespace: "unbounded-system"},
		Data:       map[string]string{"config.yaml": "live"},
	}
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(liveTarget).Build()
	r := &LegacyReaper{Client: base}
	r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key != client.ObjectKeyFromObject(liveTarget) {
				return underlying.Get(ctx, key, obj, opts...)
			}
			// Mimic JSON unmarshal: merge the live object's fields into obj's
			// pre-existing maps without clearing keys the live object omits.
			dst, ok := obj.(*corev1.ConfigMap)
			if !ok {
				return underlying.Get(ctx, key, obj, opts...)
			}

			dst.ObjectMeta = *liveTarget.ObjectMeta.DeepCopy()
			if dst.Data == nil {
				dst.Data = map[string]string{}
			}

			for k, v := range liveTarget.Data {
				dst.Data[k] = v
			}

			return nil
		},
	})

	// The desired (migrated) payload carries an extra key the live target lacks.
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: liveTarget.Name, Namespace: liveTarget.Namespace},
		Data:       map[string]string{"config.yaml": "live", "extra": "desired-only"},
	}

	err := r.createIfAbsent(t.Context(), desired)
	if !apierrors.IsConflict(err) {
		t.Fatalf("createIfAbsent error = %v, want conflict for subset live target", err)
	}
}

func TestCopyConfigMapByNameValidatesExistingTarget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		targetData   string
		wantConflict bool
	}{
		{name: "matching", targetData: "#cloud-config"},
		{name: "conflicting", targetData: "different", wantConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "user-data", Namespace: legacyKubeNamespace},
				Data:       map[string]string{"user-data": "#cloud-config"},
				BinaryData: map[string][]byte{"extra": {1, 2, 3}},
			}
			target := source.DeepCopy()
			target.Namespace = "unbounded-system"
			target.Data["user-data"] = tc.targetData
			r := newReaper(t, source, target)

			err := r.copyConfigMapByName(t.Context(), legacyKubeNamespace, source.Name, target.Namespace)
			if tc.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("copyConfigMapByName error = %v, want conflict", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("copyConfigMapByName: %v", err)
			}
		})
	}
}

func TestCopyConfigMapByNameMissingSourceRequiresTarget(t *testing.T) {
	t.Run("existing target succeeds", func(t *testing.T) {
		r := newReaper(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "user-data", Namespace: "unbounded-system",
		}})

		if err := r.copyConfigMapByName(t.Context(), legacyKubeNamespace, "user-data", "unbounded-system"); err != nil {
			t.Fatalf("copyConfigMapByName: %v", err)
		}
	})

	t.Run("missing target errors", func(t *testing.T) {
		r := newReaper(t)

		if err := r.copyConfigMapByName(t.Context(), legacyKubeNamespace, "user-data", "unbounded-system"); err == nil {
			t.Fatal("expected missing source and target to error")
		}
	})
}

func TestMigrateMachinaConfigUsesConfiguredEndpoint(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: legacyKubeNamespace},
			Data: map[string]string{
				"config.yaml": "apiServerEndpoint: https://old.example:6443\ncustom: keep\n",
			},
		},
	)
	r.APIServerEndpoint = "https://new.example:6443"

	if err := r.migrateConfigMaps(t.Context(), logr.Discard(), legacyKubeNamespace, "unbounded-system"); err != nil {
		t.Fatalf("migrateConfigMaps: %v", err)
	}

	var cm corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "machina-config"}, &cm); err != nil {
		t.Fatalf("get migrated machina config: %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		t.Fatalf("unmarshal migrated config: %v", err)
	}

	if config["apiServerEndpoint"] != r.APIServerEndpoint || config["custom"] != "keep" {
		t.Fatalf("migrated config = %#v", config)
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

func readyMachinaTarget(namespace, config string) (*corev1.ConfigMap, *appsv1.Deployment) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": config},
	}
	deploy := readyDeployment(namespace, "machina-controller")
	deploy.Generation = 1
	deploy.Spec.Template.Annotations = map[string]string{
		machinaConfigHashAnnotation: configMapPayloadHash(cm),
	}
	deploy.Status.ObservedGeneration = 1
	deploy.Status.Replicas = 1
	deploy.Status.UpdatedReplicas = 1

	return cm, deploy
}

func readyDaemonSet(namespace, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     1 << 30,
			DesiredNumberScheduled: 2,
			UpdatedNumberScheduled: 2,
			NumberReady:            2,
		},
	}
}

func storageConfigAndReadyDaemonSet(namespace, site, config string) (*corev1.ConfigMap, *appsv1.DaemonSet) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: storageConfigName(site)},
		Data:       map[string]string{"config.yaml": config},
	}
	ds := readyDaemonSet(namespace, storageDaemonSetName(site))
	ds.Spec.Template.Annotations = map[string]string{storageConfigHashAnnotation: configMapPayloadHash(cm)}

	return cm, ds
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

func netConfigAndTargets(namespace, config string, ready bool) (*corev1.ConfigMap, *appsv1.Deployment, *appsv1.DaemonSet) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "unbounded-net-config"},
		Data:       map[string]string{"config.yaml": config},
	}
	deploy := notReadyDeployment(namespace, "unbounded-net-controller")

	ds := notReadyDaemonSet(namespace, "unbounded-net-node")
	if ready {
		deploy = readyDeployment(namespace, "unbounded-net-controller")
		ds = readyDaemonSet(namespace, "unbounded-net-node")
	}

	hash := configMapPayloadHash(cm)
	deploy.Spec.Template.Annotations = map[string]string{netConfigHashAnnotation: hash}
	ds.Spec.Template.Annotations = map[string]string{netConfigHashAnnotation: hash}

	return cm, deploy, ds
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
	legacyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyNetNamespace, Name: "unbounded-net-config"},
		Data:       map[string]string{"config.yaml": "sentinel: current"},
	}
	r := newReaper(t,
		ns(legacyNetNamespace),
		ns("unbounded-system"),
		labeledDeployment(legacyNetNamespace, "unbounded-net-controller", "unbounded-net-controller"),
		legacyConfig,
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

	// Make both targets carry the current live config hash.
	_, deploy, ds := netConfigAndTargets("unbounded-system", "sentinel: current", true)
	for _, obj := range []client.Object{deploy, ds} {
		if err := r.Create(t.Context(), obj); err != nil {
			t.Fatalf("create target %T: %v", obj, err)
		}
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

	cm, deploy, ds := netConfigAndTargets("unbounded-system", "sentinel: current", false)
	for _, obj := range []client.Object{cm, deploy} {
		if err := r.Create(t.Context(), obj); err != nil {
			t.Fatalf("create net target %T: %v", obj, err)
		}
	}

	present, err = r.netTargetsPresent(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("netTargetsPresent: %v", err)
	}

	if present {
		t.Fatalf("expected not present with only the net controller")
	}

	// Both carry the live hash but are NOT Ready: present (readiness is
	// deliberately ignored because the legacy workloads hold the host ports).
	if err := r.Create(t.Context(), ds); err != nil {
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

func TestNetTargetsPresentRequiresCurrentHashOnBothWorkloads(t *testing.T) {
	const target = "unbounded-system"

	cm, deploy, ds := netConfigAndTargets(target, "sentinel: current", false)

	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.Deployment, *appsv1.DaemonSet)
		want   bool
	}{
		{name: "both match", want: true},
		{name: "controller mismatch", mutate: func(d *appsv1.Deployment, _ *appsv1.DaemonSet) {
			d.Spec.Template.Annotations[netConfigHashAnnotation] = "stale"
		}},
		{name: "node mismatch", mutate: func(_ *appsv1.Deployment, ds *appsv1.DaemonSet) {
			ds.Spec.Template.Annotations[netConfigHashAnnotation] = "stale"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotDeploy := deploy.DeepCopy()

			gotDS := ds.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(gotDeploy, gotDS)
			}

			r := newReaper(t, cm.DeepCopy(), gotDeploy, gotDS)

			got, err := r.netTargetsPresent(t.Context(), target)
			if err != nil || got != tc.want {
				t.Fatalf("netTargetsPresent = %t, err=%v, want %t", got, err, tc.want)
			}
		})
	}
}

func TestReapOnceReapsNetWhenNewNetNotReady(t *testing.T) {
	// The new net workloads exist but are NOT Ready (they stay Pending until the
	// old net frees the shared host ports). Net must still be reaped: gating net
	// on readiness here would deadlock the cutover.
	legacyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyNetNamespace, Name: "unbounded-net-config"},
		Data:       map[string]string{"config.yaml": "sentinel: current"},
	}
	r := newReaper(t,
		ns(legacyNetNamespace),
		ns("unbounded-system"),
		labeledDeployment(legacyNetNamespace, "unbounded-net-controller", "unbounded-net-controller"),
		legacyConfig,
	)

	cm, deploy, ds := netConfigAndTargets("unbounded-system", "sentinel: current", false)
	for _, obj := range []client.Object{cm, deploy, ds} {
		if err := r.Create(t.Context(), obj); err != nil {
			t.Fatalf("create target %T: %v", obj, err)
		}
	}

	if _, err := r.reapOnce(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	if err := r.Get(t.Context(), client.ObjectKey{Namespace: legacyNetNamespace, Name: "unbounded-net-controller"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected legacy net reaped even though the new net is not Ready, err=%v", err)
	}
}

func TestStorageTargetsReadyGatesOnPerSiteDaemonSets(t *testing.T) {
	r := newReaper(t, ns("unbounded-system"))

	config, readyTarget := storageConfigAndReadyDaemonSet("unbounded-system", "cluster", "version: 7")
	if err := r.Create(t.Context(), config); err != nil {
		t.Fatalf("create storage config: %v", err)
	}

	// No per-site storage DaemonSet yet: not ready.
	ready, err := r.storageTargetsReady(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if ready {
		t.Fatalf("expected not ready with no per-site storage DaemonSet")
	}

	// A per-site DaemonSet that is not yet Ready keeps the gate closed.
	notReady := readyTarget.DeepCopy()
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

	if err := r.Create(t.Context(), readyTarget); err != nil {
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

func storageEnabledSite(name string) *unboundedv1alpha3.Site {
	enabled := true

	return &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			Storage: &unboundedv1alpha3.StorageComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled}},
		}},
	}
}

func TestStorageTargetsReadyRequiresEveryStorageEnabledSite(t *testing.T) {
	// Two storage-enabled Sites, but only the cluster Site has a Ready
	// DaemonSet. The gate must stay closed until edge has one too, so a
	// multi-site cluster never loses the legacy supervisor early.
	clusterConfig, clusterDS := storageConfigAndReadyDaemonSet("unbounded-system", "cluster", "version: 7")
	edgeConfig, edgeDS := storageConfigAndReadyDaemonSet("unbounded-system", "edge", "version: 7")
	r := newReaper(t,
		ns("unbounded-system"),
		storageEnabledSite("cluster"),
		storageEnabledSite("edge"),
		clusterConfig,
		clusterDS,
		edgeConfig,
	)

	ready, err := r.storageTargetsReady(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if ready {
		t.Fatalf("expected not ready while the edge Site has no storage DaemonSet")
	}

	// Add edge's Ready DaemonSet: every storage-enabled Site now has one.
	if err := r.Create(t.Context(), edgeDS); err != nil {
		t.Fatalf("create edge ds: %v", err)
	}

	ready, err = r.storageTargetsReady(t.Context(), "unbounded-system")
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if !ready {
		t.Fatalf("expected ready once every storage-enabled Site has a Ready DaemonSet")
	}
}

func TestMigrateStorageConfigMapsCreatesPerSiteConfigs(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		storageEnabledSite("cluster"),
		storageEnabledSite("edge"),
		storageDaemonSet(legacyKubeNamespace),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config", Namespace: legacyKubeNamespace},
			Data:       map[string]string{"config.yaml": "version: 7"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-edge", Namespace: "unbounded-system"},
			Data:       map[string]string{"config.yaml": "default: true"},
		},
	)

	if err := r.migrateStorageConfigMaps(t.Context(), logr.Discard(), "unbounded-system"); err != nil {
		t.Fatalf("migrateStorageConfigMaps: %v", err)
	}

	for _, name := range []string{"unbounded-storage-config-cluster", "unbounded-storage-config-edge"} {
		var cm corev1.ConfigMap
		if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: name}, &cm); err != nil {
			t.Fatalf("expected migrated storage config %s: %v", name, err)
		}

		if cm.Data["config.yaml"] != "version: 7" {
			t.Fatalf("%s config.yaml = %q, want version: 7", name, cm.Data["config.yaml"])
		}
	}
}

func TestMigrateConfigMapsUpsertsOverReconcilerDefault(t *testing.T) {
	// The reconciler already created a default machina-config in the target;
	// the reaper must overwrite it with the migrated (legacy) config.
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: legacyKubeNamespace}, Data: map[string]string{"config.yaml": "migrated: true"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: "unbounded-system"}, Data: map[string]string{"config.yaml": "default: true"}},
	)

	if err := r.migrateConfigMaps(t.Context(), logr.Discard(), legacyKubeNamespace, "unbounded-system"); err != nil {
		t.Fatalf("migrateConfigMaps: %v", err)
	}

	var cm corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "machina-config"}, &cm); err != nil {
		t.Fatalf("get: %v", err)
	}

	if cm.Data["config.yaml"] != "migrated: true" {
		t.Fatalf("expected migrated config to win, got %q", cm.Data["config.yaml"])
	}
}

func machineWithCloudInitConfigMap(name, cmName, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "Machine"})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, cmName, "spec", "pxe", "cloudInit", "userDataConfigMapRef", "name")
	_ = unstructured.SetNestedField(obj.Object, namespace, "spec", "pxe", "cloudInit", "userDataConfigMapRef", "namespace")

	return obj
}

func TestMigrateMachineCloudInitConfigMaps(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		machineWithCloudInitConfigMap("m1", "user-data", legacyKubeNamespace),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "user-data", Namespace: legacyKubeNamespace},
			Data:       map[string]string{"user-data": "#cloud-config"},
		},
	)

	if err := r.migrateMachineCloudInitConfigMaps(t.Context(), logr.Discard(), "unbounded-system"); err != nil {
		t.Fatalf("migrateMachineCloudInitConfigMaps: %v", err)
	}

	// ConfigMap copied to target with data preserved.
	var cm corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "user-data"}, &cm); err != nil {
		t.Fatalf("expected cloud-init configmap copied: %v", err)
	}

	if cm.Data["user-data"] != "#cloud-config" {
		t.Fatalf("cloud-init configmap data not preserved: %q", cm.Data["user-data"])
	}

	// The Machine ref namespace was rewritten to the target.
	assertNestedString(t, r, "Machine", "m1", "unbounded-system",
		"spec", "pxe", "cloudInit", "userDataConfigMapRef", "namespace")
}

func TestReapOnceStorageGatedOnPerSiteDaemonSet(t *testing.T) {
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		storageDaemonSet(legacyKubeNamespace),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config", Namespace: legacyKubeNamespace},
			Data:       map[string]string{"config.yaml": "version: 7"},
		},
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

	// Bring up the per-site storage config and DaemonSet (Ready): storage reaps.
	config, ds := storageConfigAndReadyDaemonSet("unbounded-system", "cluster", "version: 7")
	for _, obj := range []client.Object{config, ds} {
		if err := r.Create(t.Context(), obj); err != nil {
			t.Fatalf("create per-site storage target %T: %v", obj, err)
		}
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
	config, target := readyMachinaTarget("unbounded-system", "apiServerEndpoint: https://api.example:6443\n")
	r := newReaper(t,
		ns(legacyKubeNamespace),
		ns("unbounded-system"),
		labeledAppDeployment(legacyKubeNamespace, "machina-controller", map[string]string{"app": "machina-controller"}),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: legacyKubeNamespace},
			Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://api.example:6443\n"},
		},
		config,
		target,
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

func TestClearLegacyNetSiteFinalizers(t *testing.T) {
	// A translated-but-abandoned legacy net Site still carries the net
	// controller's protection finalizer; the reaper must clear it so the CRD can
	// be deleted (the old net controller that owned it is gone).
	site := legacySite("cluster", map[string]any{"nodeCidrs": []any{"10.0.0.0/16"}})
	site.SetFinalizers([]string{"net.unbounded-cloud.io/protection"})

	r := newReaper(t, site)

	if err := r.clearLegacyNetSiteFinalizers(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("clearLegacyNetSiteFinalizers: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(legacySiteGVK)

	if err := r.Get(t.Context(), client.ObjectKey{Name: "cluster"}, got); err != nil {
		t.Fatalf("get legacy site: %v", err)
	}

	if fin := got.GetFinalizers(); len(fin) != 0 {
		t.Fatalf("expected finalizers cleared, got %#v", fin)
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

func TestApplyDefaultsFiltersTargetFromLegacy(t *testing.T) {
	// Installing into a legacy namespace must not make the reaper drain and
	// delete the very namespace it migrated into.
	r := &LegacyReaper{
		TargetNamespace:  legacyKubeNamespace,
		LegacyNamespaces: []string{legacyKubeNamespace, legacyNetNamespace},
	}
	r.applyDefaults()

	for _, got := range r.LegacyNamespaces {
		if got == legacyKubeNamespace {
			t.Fatalf("target namespace %q must be filtered out of LegacyNamespaces, got %#v",
				legacyKubeNamespace, r.LegacyNamespaces)
		}
	}

	if len(r.LegacyNamespaces) != 1 || r.LegacyNamespaces[0] != legacyNetNamespace {
		t.Fatalf("expected only %q to remain, got %#v", legacyNetNamespace, r.LegacyNamespaces)
	}

	// The shared package-level var must never be mutated.
	if len(LegacyNamespaces) != 2 {
		t.Fatalf("shared LegacyNamespaces was mutated: %#v", LegacyNamespaces)
	}
}

func TestApplyDefaultsRetainsLegacyForNonLegacyTarget(t *testing.T) {
	// The normal case (target unbounded-system) keeps both legacy namespaces.
	r := &LegacyReaper{
		TargetNamespace:  "unbounded-system",
		LegacyNamespaces: []string{legacyKubeNamespace, legacyNetNamespace},
	}
	r.applyDefaults()

	if len(r.LegacyNamespaces) != 2 {
		t.Fatalf("expected both legacy namespaces retained, got %#v", r.LegacyNamespaces)
	}
}

func TestApplyDefaultsFiltersDefaultTargetWhenTargetEmpty(t *testing.T) {
	// An empty TargetNamespace defaults to DefaultNamespace; if that happens to
	// be a legacy namespace it must still be filtered.
	original := DefaultNamespace

	t.Cleanup(func() { DefaultNamespace = original })

	DefaultNamespace = legacyNetNamespace

	r := &LegacyReaper{
		LegacyNamespaces: []string{legacyKubeNamespace, legacyNetNamespace},
	}
	r.applyDefaults()

	for _, got := range r.LegacyNamespaces {
		if got == legacyNetNamespace {
			t.Fatalf("defaulted target %q must be filtered out, got %#v",
				legacyNetNamespace, r.LegacyNamespaces)
		}
	}
}

// addOnlyManager satisfies ctrl.Manager by embedding it, and implements only
// the one method SetupWithManager calls. Any other call panics, which is the
// point: it pins what setup is allowed to touch.
type addOnlyManager struct {
	ctrl.Manager

	added []manager.Runnable
}

func (m *addOnlyManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)

	return nil
}

// TestReaperSetupRequiresAnAPIReader guards the fallback in liveReader.
//
// Every read the reaper makes targets a legacy namespace, and the operator
// scopes its manager cache to its own namespace. liveReader falls back to the
// cached client when APIReader is nil, which is harmless in a unit test against
// a fake client and is not harmless under a manager: those reads would fail
// deep inside a migration that deletes things.
//
// Failing at startup is the difference between a pod that will not start and a
// pod that half-migrates a cluster.
func TestReaperSetupRequiresAnAPIReader(t *testing.T) {
	reaper := &LegacyReaper{
		Client:          fake.NewClientBuilder().WithScheme(reaperScheme(t)).Build(),
		TargetNamespace: "unbounded-system",
	}

	// The check runs before the manager is touched, so a nil manager is safe
	// here and proves the check comes first.
	err := reaper.SetupWithManager(nil)
	if err == nil {
		t.Fatal("a reaper with no APIReader must be refused at setup")
	}

	if !strings.Contains(err.Error(), "APIReader") {
		t.Fatalf("error = %q, want it to name the missing field", err)
	}

	if !strings.Contains(err.Error(), "cache") {
		t.Fatalf("error = %q, want it to say why the cache makes this necessary", err)
	}
}

// TestReaperSetupAcceptsAnAPIReader confirms the guard is not simply refusing
// everything, and that a correctly wired reaper is registered as a runnable.
func TestReaperSetupAcceptsAnAPIReader(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(reaperScheme(t)).Build()

	reaper := &LegacyReaper{
		Client:          cl,
		APIReader:       cl,
		TargetNamespace: "unbounded-system",
	}

	mgr := &addOnlyManager{}
	if err := reaper.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}

	if len(mgr.added) != 1 {
		t.Fatalf("added %d runnables, want the reaper itself", len(mgr.added))
	}
}

// TestMigrationGatesRejectZeroReplicas is a regression test for the most
// destructive defect in this series.
//
// These gates decide whether the replacement controller is healthy enough for
// the reaper to delete the legacy one. They were written as equality against
// the desired replica count, so a Deployment scaled to zero satisfied every
// one of them: nothing updated, nothing running, nothing available, all equal
// to nothing desired. The reaper then deleted a working legacy controller and
// left the cluster with neither.
//
// It became reachable when overrides gained spec.replicas. A Site cannot scale
// these controllers to zero through its typed fields, but an override can, and
// it is exactly the sort of thing someone does while debugging.
func TestMigrationGatesRejectZeroReplicas(t *testing.T) {
	scaledToZero := func() *appsv1.Deployment {
		zero := int32(0)

		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "unbounded-system", Name: "machina-controller", Generation: 3},
			Spec:       appsv1.DeploymentSpec{Replicas: &zero},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 3,
				// Every count agrees with the desired zero, which is exactly
				// what made the equality checks pass.
				UpdatedReplicas:   0,
				Replicas:          0,
				AvailableReplicas: 0,
			},
		}
	}

	if deploymentAvailable(scaledToZero()) {
		t.Fatal("a Deployment running no pods must not be reported available")
	}

	if deploymentRolloutComplete(scaledToZero()) {
		t.Fatal("a Deployment running no pods must not be reported rolled out")
	}
}

// TestMigrationGatesAcceptRunningReplicas confirms the check is a floor on the
// desired count rather than a blanket refusal.
func TestMigrationGatesAcceptRunningReplicas(t *testing.T) {
	two := int32(2)
	healthy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "unbounded-system", Name: "machina-controller", Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3,
			UpdatedReplicas:    2,
			Replicas:           2,
			AvailableReplicas:  2,
		},
	}

	if !deploymentAvailable(healthy) {
		t.Fatal("a Deployment with all replicas available must be reported available")
	}

	if !deploymentRolloutComplete(healthy) {
		t.Fatal("a fully rolled out Deployment must be reported complete")
	}
}

// TestMachinaGateRejectsZeroReplicaTarget drives the same defect through the
// gate that actually authorizes the delete, rather than the helper alone.
func TestMachinaGateRejectsZeroReplicaTarget(t *testing.T) {
	const target = "unbounded-system"

	zero := int32(0)

	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://example:6443"},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "machina-controller", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{machinaConfigHashAnnotation: configMapPayloadHash(config)},
				},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1},
	}

	r := newReaper(t, config, deploy)

	ready, err := r.machinaTargetReady(t.Context(), target)
	if err != nil {
		t.Fatalf("machinaTargetReady: %v", err)
	}

	if ready {
		t.Fatal("machina must not be reported ready to reap while its replacement runs no pods")
	}
}
