// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func boolPtr(v bool) *bool { return &v }

func metalmanEnabledSite(name string) *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.SiteSpec{
			Components: unboundedv1alpha3.SiteComponents{
				Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
					SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: boolPtr(true)},
				},
			},
		},
	}
}

func targetMetalmanDeployment(target, site string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: metalmanDeploymentName(site)},
	}
}

// Finding 1: metalman legacy resources may only be reaped once the per-site
// replacement Deployment exists (presence, not readiness - host-port contention
// would deadlock a readiness gate). A footprint with no enabled Site blocks reap.
func TestMetalmanTargetsPresent(t *testing.T) {
	target := "unbounded-system"

	t.Run("absent replacement blocks reap", func(t *testing.T) {
		r := newReaper(t, metalmanEnabledSite("edge"))

		present, err := r.metalmanTargetsPresent(t.Context(), target)
		if err != nil {
			t.Fatalf("metalmanTargetsPresent: %v", err)
		}

		if present {
			t.Fatal("expected not present when the per-site metalman Deployment is missing")
		}
	})

	t.Run("present replacement allows reap", func(t *testing.T) {
		r := newReaper(t, metalmanEnabledSite("edge"), targetMetalmanDeployment(target, "edge"))

		present, err := r.metalmanTargetsPresent(t.Context(), target)
		if err != nil {
			t.Fatalf("metalmanTargetsPresent: %v", err)
		}

		if !present {
			t.Fatal("expected present when the per-site metalman Deployment exists")
		}
	})

	t.Run("no enabled site blocks reap", func(t *testing.T) {
		r := newReaper(t) // no Sites enable metalman

		present, err := r.metalmanTargetsPresent(t.Context(), target)
		if err != nil {
			t.Fatalf("metalmanTargetsPresent: %v", err)
		}

		if present {
			t.Fatal("expected not present when no Site enables metalman (detection miss guard)")
		}
	})
}

func TestMachinaTargetReadyRequiresCurrentConfigAndRollout(t *testing.T) {
	const target = "unbounded-system"

	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://api.example:6443\n"},
	}
	replicas := int32(1)
	ready := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "machina-controller", Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				machinaConfigHashAnnotation: machinaConfigHash(config.Data["config.yaml"]),
			}}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}

	t.Run("current config and complete rollout pass", func(t *testing.T) {
		r := newReaper(t, config.DeepCopy(), ready.DeepCopy())

		got, err := r.machinaTargetReady(t.Context(), target)
		if err != nil || !got {
			t.Fatalf("machinaTargetReady = %t, err=%v, want true", got, err)
		}
	})

	t.Run("stale hash blocks", func(t *testing.T) {
		deploy := ready.DeepCopy()
		deploy.Spec.Template.Annotations[machinaConfigHashAnnotation] = "stale"
		r := newReaper(t, config.DeepCopy(), deploy)

		got, err := r.machinaTargetReady(t.Context(), target)
		if err != nil || got {
			t.Fatalf("machinaTargetReady = %t, err=%v, want false", got, err)
		}
	})

	t.Run("incomplete rollout blocks", func(t *testing.T) {
		deploy := ready.DeepCopy()
		deploy.Status.AvailableReplicas = 0
		r := newReaper(t, config.DeepCopy(), deploy)

		got, err := r.machinaTargetReady(t.Context(), target)
		if err != nil || got {
			t.Fatalf("machinaTargetReady = %t, err=%v, want false", got, err)
		}
	})
}

// Finding 2: metalman detection tolerates the deprecated site-label key and
// matches by the deterministic Deployment name, so a real v0.1.19 cluster (which
// happens to use the canonical key) and older shapes both migrate.
func TestLegacyMetalmanDetectionHardening(t *testing.T) {
	t.Run("deprecated label key", func(t *testing.T) {
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace,
			Name:      "metalman-controller-edge",
			Labels:    map[string]string{"app": "unbounded-pxe", deprecatedSiteLabelKey: "edge"},
		}}
		r := newReaper(t, deploy)

		got, err := r.legacyMetalmanExistsForSite(t.Context(), "edge")
		if err != nil {
			t.Fatalf("legacyMetalmanExistsForSite: %v", err)
		}

		if !got {
			t.Fatal("expected metalman detected via deprecated site label")
		}
	})

	t.Run("name-only (no site label)", func(t *testing.T) {
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace,
			Name:      "metalman-controller-edge",
			Labels:    map[string]string{"app": "unbounded-pxe"},
		}}
		r := newReaper(t, deploy)

		got, err := r.legacyMetalmanExistsForSite(t.Context(), "edge")
		if err != nil {
			t.Fatalf("legacyMetalmanExistsForSite: %v", err)
		}

		if !got {
			t.Fatal("expected metalman detected via deterministic Deployment name")
		}
	})

	t.Run("different site not matched", func(t *testing.T) {
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace,
			Name:      "metalman-controller-other",
			Labels:    map[string]string{"app": "unbounded-pxe", unboundedv1alpha3.MachineSiteLabelKey: "other"},
		}}
		r := newReaper(t, deploy)

		got, err := r.legacyMetalmanExistsForSite(t.Context(), "edge")
		if err != nil {
			t.Fatalf("legacyMetalmanExistsForSite: %v", err)
		}

		if got {
			t.Fatal("did not expect a different site's metalman to match")
		}
	})

	t.Run("conflicting matching Deployments fail closed", func(t *testing.T) {
		byName := metalmanDeploymentForSiteWithArgs(legacyKubeNamespace, "edge", "--dhcp-auto-interface")
		byLabel := metalmanDeploymentForSiteWithArgs(legacyNetNamespace, "edge", "--dhcp-auto-interface=false")
		byLabel.Name = "older-metalman-name"
		r := newReaper(t, byName, byLabel)

		if _, _, err := r.legacyMetalmanConfigForSite(t.Context(), "edge"); err == nil {
			t.Fatal("expected conflicting matching Metalman Deployments to fail closed")
		}
	})

	t.Run("absent and enabled arguments conflict", func(t *testing.T) {
		withoutFlag := metalmanDeploymentForSiteWithArgs(legacyKubeNamespace, "edge", "serve-pxe")
		withFlag := metalmanDeploymentForSiteWithArgs(legacyNetNamespace, "edge", "--dhcp-auto-interface")
		withFlag.Name = "older-metalman-name"
		r := newReaper(t, withoutFlag, withFlag)

		if _, _, err := r.legacyMetalmanConfigForSite(t.Context(), "edge"); err == nil {
			t.Fatal("expected absent and enabled Metalman arguments to conflict")
		}
	})
}

// The generic storage readiness predicate remains strict for zero desired; the
// higher-level gate applies the narrow no-matching-Node exception.
func TestStorageDaemonSetReadyRequiresScheduled(t *testing.T) {
	cases := []struct {
		name string
		ds   appsv1.DaemonSet
		want bool
	}{
		{
			name: "zero desired is not ready",
			ds:   appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 0, NumberReady: 0}},
			want: false,
		},
		{
			name: "scheduled and ready",
			ds:   appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2}},
			want: true,
		},
		{
			name: "scheduled but not all ready",
			ds:   appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storageDaemonSetReady(&tc.ds); got != tc.want {
				t.Fatalf("storageDaemonSetReady = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStorageTargetsReadyAllowsZeroDesiredWithoutMatchingNodes(t *testing.T) {
	target := "unbounded-system"

	zeroDesired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageDaemonSetName("edge")},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 100, DesiredNumberScheduled: 0},
	}
	site := storageEnabledSite("edge")
	site.Spec.NodeCidrs = []string{"10.20.0.0/16"}
	r := newReaper(t, site, zeroDesired)

	ready, err := r.storageTargetsReady(t.Context(), target)
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if !ready {
		t.Fatal("expected zero-desired storage ready when no Node matches the Site CIDRs")
	}
}

func TestStorageTargetsReadyZeroDesiredNodeCIDRGate(t *testing.T) {
	target := "unbounded-system"
	zeroDesired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageDaemonSetName("edge")},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 100},
	}

	t.Run("matching unlabeled node blocks", func(t *testing.T) {
		site := storageEnabledSite("edge")
		site.Spec.NodeCidrs = []string{"10.20.0.0/16"}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.20.1.2"},
			}},
		}
		r := newReaper(t, site, zeroDesired.DeepCopy(), node)

		ready, err := r.storageTargetsReady(t.Context(), target)
		if err != nil {
			t.Fatalf("storageTargetsReady: %v", err)
		}

		if ready {
			t.Fatal("expected matching unlabeled Node to block zero-desired storage")
		}
	})

	t.Run("nonmatching node allows", func(t *testing.T) {
		site := storageEnabledSite("edge")
		site.Spec.NodeCidrs = []string{"10.20.0.0/16"}
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.30.1.2"},
			}},
		}
		r := newReaper(t, site, zeroDesired.DeepCopy(), node)

		ready, err := r.storageTargetsReady(t.Context(), target)
		if err != nil {
			t.Fatalf("storageTargetsReady: %v", err)
		}

		if !ready {
			t.Fatal("expected nonmatching Node to allow zero-desired storage")
		}
	})

	t.Run("invalid Site CIDR errors", func(t *testing.T) {
		site := storageEnabledSite("edge")
		site.Spec.NodeCidrs = []string{"not-a-cidr"}
		r := newReaper(t, site, zeroDesired.DeepCopy())

		if _, err := r.storageTargetsReady(t.Context(), target); err == nil {
			t.Fatal("expected invalid Site CIDR to fail the storage gate")
		}
	})
}

// Finding 6: the legacy net-group Site CRD is only deleted once the new net
// controller is Available (so it has re-owned/recreated the SiteNodeSlices).
func TestNetControllerAvailable(t *testing.T) {
	target := "unbounded-system"

	t.Run("absent", func(t *testing.T) {
		r := newReaper(t)

		ok, err := r.netControllerAvailable(t.Context(), target)
		if err != nil {
			t.Fatalf("netControllerAvailable: %v", err)
		}

		if ok {
			t.Fatal("expected not available when the Deployment is missing")
		}
	})

	t.Run("available", func(t *testing.T) {
		replicas := int32(1)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "unbounded-net-controller"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 100, AvailableReplicas: 1},
		}
		r := newReaper(t, deploy)

		ok, err := r.netControllerAvailable(t.Context(), target)
		if err != nil {
			t.Fatalf("netControllerAvailable: %v", err)
		}

		if !ok {
			t.Fatal("expected available")
		}
	})
}

// Finding 5: a legacy namespace that still holds non-operator workloads is
// reported (so the warning path fires) but deletion still proceeds.
func TestForeignWorkloads(t *testing.T) {
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "user-app"}}
	stateful := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "user-database"}}
	r := newReaper(t, foreign, stateful)

	got, err := r.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads: %v", err)
	}

	if len(got) != 2 || got[0] != "Deployment/user-app" || got[1] != "StatefulSet/user-database" {
		t.Fatalf("foreignWorkloads = %v, want [Deployment/user-app StatefulSet/user-database]", got)
	}

	// warnOnForeignWorkloads must be nil-Recorder safe and not error.
	r.warnOnForeignWorkloads(t.Context(), logr.Discard(), legacyKubeNamespace)

	empty := newReaper(t)

	names, err := empty.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads(empty): %v", err)
	}

	if len(names) != 0 {
		t.Fatalf("expected no foreign workloads, got %v", names)
	}
}

// Finding 6: cleanupLegacySiteCRD retains the legacy CRD while the new net
// controller is not yet Available, then deletes it once it is.
func TestCleanupLegacySiteCRDGatesOnNetController(t *testing.T) {
	target := "unbounded-system"
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: legacySiteCRDName}}

	t.Run("retained while net controller unavailable", func(t *testing.T) {
		r := newReaper(t, crd.DeepCopy())

		gone, err := r.cleanupLegacySiteCRD(t.Context(), logr.Discard(), target)
		if err != nil {
			t.Fatalf("cleanupLegacySiteCRD: %v", err)
		}

		if gone {
			t.Fatal("expected legacy CRD retained while net controller is unavailable")
		}
	})

	t.Run("deleted once net controller available", func(t *testing.T) {
		replicas := int32(1)
		netCtrl := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "unbounded-net-controller"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 100, AvailableReplicas: 1},
		}
		r := newReaper(t, crd.DeepCopy(), netCtrl)

		// First pass issues the delete; deleteLegacySiteCRD reports not-gone
		// until the object is observed absent.
		if _, err := r.cleanupLegacySiteCRD(t.Context(), logr.Discard(), target); err != nil {
			t.Fatalf("cleanupLegacySiteCRD (delete pass): %v", err)
		}

		gone, err := r.cleanupLegacySiteCRD(t.Context(), logr.Discard(), target)
		if err != nil {
			t.Fatalf("cleanupLegacySiteCRD (confirm pass): %v", err)
		}

		if !gone {
			t.Fatal("expected legacy CRD deleted once net controller is Available")
		}
	})

	t.Run("retained while a slice has stale ownership", func(t *testing.T) {
		replicas := int32(1)
		netCtrl := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "unbounded-net-controller"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 100, AvailableReplicas: 1},
		}
		site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "current-uid"}}
		slice := siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{{
			APIVersion: legacySiteGVK.GroupVersion().String(),
			Kind:       legacySiteGVK.Kind,
			Name:       "edge",
			UID:        "legacy-uid",
		}})
		r := newReaper(t, crd.DeepCopy(), netCtrl, site, slice)

		gone, err := r.cleanupLegacySiteCRD(t.Context(), logr.Discard(), target)
		if err != nil {
			t.Fatalf("cleanupLegacySiteCRD: %v", err)
		}

		if gone {
			t.Fatal("expected legacy CRD retained while a SiteNodeSlice has stale ownership")
		}

		if exists, err := r.legacySiteCRDExists(t.Context()); err != nil || !exists {
			t.Fatalf("legacy CRD should still exist: exists=%t err=%v", exists, err)
		}
	})

	t.Run("absent CRD reports gone", func(t *testing.T) {
		r := newReaper(t)

		gone, err := r.cleanupLegacySiteCRD(t.Context(), logr.Discard(), target)
		if err != nil {
			t.Fatalf("cleanupLegacySiteCRD: %v", err)
		}

		if !gone {
			t.Fatal("expected gone when the legacy CRD does not exist")
		}
	})
}

func siteNodeSlice(name, siteName string, refs []metav1.OwnerReference) *unstructured.Unstructured {
	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(siteNodeSliceGVK)
	slice.SetName(name)
	slice.SetOwnerReferences(refs)
	slice.Object["siteName"] = siteName

	return slice
}

func TestSiteNodeSlicesOwnedByCurrentSites(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "current-uid"}}
	exact := *metav1.NewControllerRef(site, newSiteGVK())

	tests := []struct {
		name string
		objs []client.Object
		want bool
	}{
		{name: "zero slices pass", want: true},
		{name: "exact sole owner passes", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{exact})}, want: true},
		{name: "wrong UID blocks", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{{APIVersion: exact.APIVersion, Kind: exact.Kind, Name: exact.Name, UID: "old-uid", Controller: exact.Controller, BlockOwnerDeletion: exact.BlockOwnerDeletion}})}},
		{name: "wrong GVK blocks", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{{APIVersion: legacySiteGVK.GroupVersion().String(), Kind: "Site", Name: exact.Name, UID: exact.UID, Controller: exact.Controller, BlockOwnerDeletion: exact.BlockOwnerDeletion}})}},
		{name: "additional owner blocks", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{exact, {APIVersion: "v1", Kind: "Node", Name: "node", UID: "node-uid"}})}},
		{name: "missing matching Site blocks", objs: []client.Object{siteNodeSlice("missing-0", "missing", nil)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{site.DeepCopy()}
			objs = append(objs, tt.objs...)
			r := newReaper(t, objs...)

			got, err := r.siteNodeSlicesOwnedByCurrentSites(t.Context())
			if err != nil {
				t.Fatalf("siteNodeSlicesOwnedByCurrentSites: %v", err)
			}

			if got != tt.want {
				t.Fatalf("siteNodeSlicesOwnedByCurrentSites = %t, want %t", got, tt.want)
			}
		})
	}
}
