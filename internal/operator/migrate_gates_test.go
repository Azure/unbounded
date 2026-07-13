// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
}

// Finding 7: a per-site storage DaemonSet that has not scheduled any pod is not
// "ready"; treating DesiredNumberScheduled==0 as ready would let the legacy
// supervisor be reaped before the replacement runs anywhere.
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

func TestStorageTargetsReadyBlocksOnZeroDesired(t *testing.T) {
	target := "unbounded-system"

	zeroDesired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageDaemonSetName("edge")},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 100, DesiredNumberScheduled: 0},
	}
	r := newReaper(t, storageEnabledSite("edge"), zeroDesired)

	ready, err := r.storageTargetsReady(t.Context(), target)
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if ready {
		t.Fatal("expected storage not ready while the per-site DaemonSet has zero scheduled pods")
	}
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
	r := newReaper(t, foreign)

	got, err := r.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads: %v", err)
	}

	if len(got) != 1 || got[0] != "Deployment/user-app" {
		t.Fatalf("foreignWorkloads = %v, want [Deployment/user-app]", got)
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
