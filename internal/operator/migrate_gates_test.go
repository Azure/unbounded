// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func boolPtr(v bool) *bool { return &v }

func configMapSequenceReader(
	t *testing.T,
	base client.WithWatch,
	key client.ObjectKey,
	sequence ...*corev1.ConfigMap,
) client.Reader {
	t.Helper()

	return configMapSequencesReader(t, base, map[client.ObjectKey][]*corev1.ConfigMap{key: sequence})
}

func configMapSequencesReader(
	t *testing.T,
	base client.WithWatch,
	sequences map[client.ObjectKey][]*corev1.ConfigMap,
) client.Reader {
	t.Helper()

	gets := make(map[client.ObjectKey]int, len(sequences))

	t.Cleanup(func() {
		for key, sequence := range sequences {
			if gets[key] != len(sequence) {
				t.Errorf("ConfigMap %s Get count = %d, want %d", key, gets[key], len(sequence))
			}
		}
	})

	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, underlying client.WithWatch, gotKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			sequence, ok := sequences[gotKey]
			if !ok {
				return underlying.Get(ctx, gotKey, obj, opts...)
			}

			if gets[gotKey] >= len(sequence) {
				t.Fatalf("unexpected ConfigMap %s Get %d", gotKey, gets[gotKey]+1)
			}

			config, ok := obj.(*corev1.ConfigMap)
			if !ok {
				t.Fatalf("Get %s wrote into %T, want *corev1.ConfigMap", gotKey, obj)
			}

			*config = *sequence[gets[gotKey]].DeepCopy()
			gets[gotKey]++

			return nil
		},
	})
}

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
				machinaConfigHashAnnotation: configMapPayloadHash(config),
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

func TestMachinaTargetReadyRevalidatesTransformedLegacySource(t *testing.T) {
	const (
		target   = "unbounded-system"
		endpoint = "https://new.example:6443"
	)

	legacyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "machina-config", ResourceVersion: "legacy-1"},
		Data: map[string]string{
			"config.yaml": "apiServerEndpoint: https://old.example:6443\ncustom: retained\n",
		},
		BinaryData: map[string][]byte{"extra": {1, 2, 3}},
	}
	targetConfig := legacyConfig.DeepCopy()
	targetConfig.Namespace = target
	targetConfig.ResourceVersion = "target-1"

	transformed, err := setMachinaAPIServerEndpoint(targetConfig.Data["config.yaml"], endpoint)
	if err != nil {
		t.Fatalf("setMachinaAPIServerEndpoint: %v", err)
	}

	targetConfig.Data["config.yaml"] = transformed
	deploy := readyMachinaDeploymentForConfig(targetConfig)
	changedLegacyConfig := legacyConfig.DeepCopy()
	changedLegacyConfig.ResourceVersion = "legacy-2"

	for _, tc := range []struct {
		name        string
		finalSource *corev1.ConfigMap
		want        bool
	}{
		{name: "source resource version changed during gate blocks", finalSource: changedLegacyConfig},
		{name: "stable transformed source passes", finalSource: legacyConfig, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(deploy.DeepCopy()).Build()
			r := &LegacyReaper{
				Client:            base,
				APIServerEndpoint: endpoint,
				APIReader: configMapSequencesReader(t, base, map[client.ObjectKey][]*corev1.ConfigMap{
					client.ObjectKeyFromObject(targetConfig): {targetConfig, targetConfig},
					client.ObjectKeyFromObject(legacyConfig): {legacyConfig, tc.finalSource},
				}),
			}

			ready, err := r.machinaTargetReady(t.Context(), target)
			if err != nil || ready != tc.want {
				t.Fatalf("machinaTargetReady = %t, err=%v, want %t", ready, err, tc.want)
			}
		})
	}

	t.Run("changed source payload after copy blocks", func(t *testing.T) {
		changed := legacyConfig.DeepCopy()
		changed.ResourceVersion = "legacy-2"
		changed.Data["custom"] = "changed"
		r := newReaper(t, targetConfig.DeepCopy(), deploy.DeepCopy(), changed)
		r.APIServerEndpoint = endpoint

		ready, err := r.machinaTargetReady(t.Context(), target)
		if err != nil || ready {
			t.Fatalf("machinaTargetReady = %t, err=%v, want false", ready, err)
		}
	})
}

func TestMachinaTargetReadyHandlesMissingLegacySource(t *testing.T) {
	const target = "unbounded-system"

	config, deploy := readyMachinaTarget(target, "apiServerEndpoint: https://api.example:6443\n")

	for _, tc := range []struct {
		name      string
		namespace *corev1.Namespace
		want      bool
	}{
		{name: "active namespace blocks", namespace: ns(legacyKubeNamespace)},
		{name: "absent namespace passes", want: true},
		{name: "terminating namespace passes", namespace: terminatingNamespace(legacyKubeNamespace), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{config.DeepCopy(), deploy.DeepCopy()}
			if tc.namespace != nil {
				objects = append(objects, tc.namespace)
			}

			r := newReaper(t, objects...)

			ready, err := r.machinaTargetReady(t.Context(), target)
			if err != nil || ready != tc.want {
				t.Fatalf("machinaTargetReady = %t, err=%v, want %t", ready, err, tc.want)
			}
		})
	}
}

func readyMachinaDeploymentForConfig(config *corev1.ConfigMap) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: config.Namespace, Name: "machina-controller", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				machinaConfigHashAnnotation: configMapPayloadHash(config),
			}}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
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
			ds:   appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberReady: 2}},
			want: true,
		},
		{
			name: "old revision ready",
			ds:   appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 1, NumberReady: 2}},
			want: false,
		},
		{
			name: "stale observed generation",
			ds: appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status:     appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberReady: 2},
			},
			want: false,
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
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageConfigName("edge")},
		Data:       map[string]string{"config.yaml": "version: 7"},
	}

	zeroDesired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageDaemonSetName("edge")},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			storageConfigHashAnnotation: configMapPayloadHash(config),
		}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 100, DesiredNumberScheduled: 0},
	}
	site := storageEnabledSite("edge")
	site.Spec.NodeCidrs = []string{"10.20.0.0/16"}
	r := newReaper(t, site, config, zeroDesired)

	ready, err := r.storageTargetsReady(t.Context(), target)
	if err != nil {
		t.Fatalf("storageTargetsReady: %v", err)
	}

	if !ready {
		t.Fatal("expected zero-desired storage ready when no Node matches the Site CIDRs")
	}
}

func TestStorageTargetsReadyZeroDesiredStillRequiresCurrentHash(t *testing.T) {
	const target = "unbounded-system"

	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageConfigName("edge")},
		Data:       map[string]string{"config.yaml": "version: 7"},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageDaemonSetName("edge")},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			storageConfigHashAnnotation: "stale",
		}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 100},
	}
	site := storageEnabledSite("edge")
	site.Spec.NodeCidrs = []string{"10.20.0.0/16"}

	r := newReaper(t, site, config, ds)

	ready, err := r.storageTargetsReady(t.Context(), target)
	if err != nil || ready {
		t.Fatalf("storageTargetsReady = %t, err=%v, want false for stale zero-desired hash", ready, err)
	}
}

func TestStorageTargetsReadyZeroDesiredNodeCIDRGate(t *testing.T) {
	target := "unbounded-system"
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageConfigName("edge")},
		Data:       map[string]string{"config.yaml": "version: 7"},
	}
	zeroDesired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: storageDaemonSetName("edge")},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			storageConfigHashAnnotation: configMapPayloadHash(config),
		}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 100},
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
		r := newReaper(t, site, config.DeepCopy(), zeroDesired.DeepCopy(), node)

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
		r := newReaper(t, site, config.DeepCopy(), zeroDesired.DeepCopy(), node)

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
		r := newReaper(t, site, config.DeepCopy(), zeroDesired.DeepCopy())

		if _, err := r.storageTargetsReady(t.Context(), target); err == nil {
			t.Fatal("expected invalid Site CIDR to fail the storage gate")
		}
	})
}

func TestStorageTargetsReadyRequiresLiveConfigHash(t *testing.T) {
	const target = "unbounded-system"

	config, ds := storageConfigAndReadyDaemonSet(target, "edge", "version: 7")
	site := storageEnabledSite("edge")

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.ConfigMap, *appsv1.DaemonSet)
		want   bool
	}{
		{name: "matching hash passes", want: true},
		{name: "missing config blocks", mutate: func(cm *corev1.ConfigMap, _ *appsv1.DaemonSet) {
			cm.Name = "other"
		}},
		{name: "stale hash blocks", mutate: func(_ *corev1.ConfigMap, ds *appsv1.DaemonSet) {
			ds.Spec.Template.Annotations[storageConfigHashAnnotation] = "stale"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotConfig := config.DeepCopy()

			gotDS := ds.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(gotConfig, gotDS)
			}

			r := newReaper(t, site.DeepCopy(), gotConfig, gotDS)

			ready, err := r.storageTargetsReady(t.Context(), target)
			if err != nil || ready != tc.want {
				t.Fatalf("storageTargetsReady = %t, err=%v, want %t", ready, err, tc.want)
			}
		})
	}
}

func TestStorageTargetsReadyRejectsConfigMapTOCTOU(t *testing.T) {
	const target = "unbounded-system"

	configA, ds := storageConfigAndReadyDaemonSet(target, "edge", "version: A")
	configA.ResourceVersion = "1"
	configB := configA.DeepCopy()
	configB.ResourceVersion = "2"
	configB.Data = map[string]string{"config.yaml": "version: B"}

	for _, tc := range []struct {
		name  string
		final *corev1.ConfigMap
		want  bool
	}{
		{name: "changed payload blocks", final: configB},
		{name: "stable payload passes", final: configA, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(
				storageEnabledSite("edge"),
				ds.DeepCopy(),
			).Build()
			key := client.ObjectKeyFromObject(configA)
			r := &LegacyReaper{
				Client:    base,
				APIReader: configMapSequenceReader(t, base, key, configA, tc.final),
			}

			ready, err := r.storageTargetsReady(t.Context(), target)
			if err != nil || ready != tc.want {
				t.Fatalf("storageTargetsReady = %t, err=%v, want %t", ready, err, tc.want)
			}
		})
	}
}

func TestStorageTargetsReadyRevalidatesLegacySource(t *testing.T) {
	const target = "unbounded-system"

	legacyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       legacyKubeNamespace,
			Name:            "unbounded-storage-config",
			ResourceVersion: "legacy-1",
		},
		Data:       map[string]string{"config.yaml": "version: A"},
		BinaryData: map[string][]byte{"extra": {1, 2, 3}},
	}
	targetConfig, ds := storageConfigAndReadyDaemonSet(target, "edge", "version: A")
	targetConfig.BinaryData = legacyConfig.BinaryData
	ds.Spec.Template.Annotations[storageConfigHashAnnotation] = configMapPayloadHash(targetConfig)
	targetConfig.ResourceVersion = "target-1"
	changedLegacyConfig := legacyConfig.DeepCopy()
	changedLegacyConfig.ResourceVersion = "legacy-2"
	changedLegacyConfig.Data = map[string]string{"config.yaml": "version: B"}
	newVersionLegacyConfig := legacyConfig.DeepCopy()
	newVersionLegacyConfig.ResourceVersion = "legacy-2"

	t.Run("target copied from stale source blocks", func(t *testing.T) {
		r := newReaper(t,
			storageEnabledSite("edge"),
			targetConfig.DeepCopy(),
			ds.DeepCopy(),
			changedLegacyConfig.DeepCopy(),
		)

		ready, err := r.storageTargetsReady(t.Context(), target)
		if err != nil || ready {
			t.Fatalf("storageTargetsReady = %t, err=%v, want false", ready, err)
		}
	})

	for _, tc := range []struct {
		name        string
		finalSource *corev1.ConfigMap
		want        bool
	}{
		{name: "source changed during gate blocks", finalSource: changedLegacyConfig},
		{name: "source resource version changed during gate blocks", finalSource: newVersionLegacyConfig},
		{name: "stable source passes", finalSource: legacyConfig, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(
				storageEnabledSite("edge"),
				ds.DeepCopy(),
			).Build()
			r := &LegacyReaper{
				Client: base,
				APIReader: configMapSequencesReader(t, base, map[client.ObjectKey][]*corev1.ConfigMap{
					client.ObjectKeyFromObject(targetConfig): {targetConfig, targetConfig},
					client.ObjectKeyFromObject(legacyConfig): {legacyConfig, tc.finalSource},
				}),
			}

			ready, err := r.storageTargetsReady(t.Context(), target)
			if err != nil || ready != tc.want {
				t.Fatalf("storageTargetsReady = %t, err=%v, want %t", ready, err, tc.want)
			}
		})
	}
}

func TestStorageTargetsReadyHandlesMissingLegacySource(t *testing.T) {
	const target = "unbounded-system"

	config, ds := storageConfigAndReadyDaemonSet(target, "edge", "version: A")

	for _, tc := range []struct {
		name      string
		namespace *corev1.Namespace
		want      bool
	}{
		{name: "active namespace blocks", namespace: ns(legacyKubeNamespace)},
		{name: "absent namespace passes", want: true},
		{name: "terminating namespace passes", namespace: terminatingNamespace(legacyKubeNamespace), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{storageEnabledSite("edge"), config.DeepCopy(), ds.DeepCopy()}
			if tc.namespace != nil {
				objects = append(objects, tc.namespace)
			}

			r := newReaper(t, objects...)

			ready, err := r.storageTargetsReady(t.Context(), target)
			if err != nil || ready != tc.want {
				t.Fatalf("storageTargetsReady = %t, err=%v, want %t", ready, err, tc.want)
			}
		})
	}
}

func terminatingNamespace(name string) *corev1.Namespace {
	namespace := ns(name)
	now := metav1.Now()
	namespace.DeletionTimestamp = &now
	namespace.Finalizers = []string{"test.unbounded-cloud.io/hold"}

	return namespace
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
	r := newReaper(t,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "pod"}},
		&corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "rc"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "deployment"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "replicaset"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "daemonset"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "statefulset"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "job"}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "cronjob"}},
	)

	got, err := r.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads: %v", err)
	}

	want := []string{
		"CronJob/cronjob",
		"DaemonSet/daemonset",
		"Deployment/deployment",
		"Job/job",
		"Pod/pod",
		"ReplicaSet/replicaset",
		"ReplicationController/rc",
		"StatefulSet/statefulset",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foreignWorkloads = %v, want %v", got, want)
	}

	// warnOnForeignWorkloads must be nil-Recorder safe and not error.
	if err := r.warnOnForeignWorkloads(t.Context(), logr.Discard(), legacyKubeNamespace); err != nil {
		t.Fatalf("warnOnForeignWorkloads: %v", err)
	}

	empty := newReaper(t)

	names, err := empty.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads(empty): %v", err)
	}

	if len(names) != 0 {
		t.Fatalf("expected no foreign workloads, got %v", names)
	}
}

// Finding 3: the whole-namespace delete also destroys resources the migration
// never copies. The audit surfaces PersistentVolumeClaims and deliberately
// skipped Secrets so operators see the full blast radius before deletion.
func TestDataBearingResourcesAtRiskSurfacesPVCsAndSkippedSecrets(t *testing.T) {
	r := newReaper(t,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "data"}},
		// Skipped by migration (SkipSecretNames): never copied, so at risk.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "unbounded-net-serving-cert"}},
		// Copied by migrateSecrets: not at risk, must not be reported.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "user-secret"}},
	)

	got, err := r.dataBearingResourcesAtRisk(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("dataBearingResourcesAtRisk: %v", err)
	}

	want := []string{
		"PersistentVolumeClaim/data",
		"Secret/unbounded-net-serving-cert",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dataBearingResourcesAtRisk = %v, want %v", got, want)
	}

	// The at-risk resources must be surfaced through the warning Event too.
	recorder := events.NewFakeRecorder(1)
	r.Recorder = recorder

	if err := r.warnOnForeignWorkloads(t.Context(), logr.Discard(), legacyKubeNamespace); err != nil {
		t.Fatalf("warnOnForeignWorkloads: %v", err)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "PersistentVolumeClaim/data") || !strings.Contains(event, "Secret/unbounded-net-serving-cert") {
			t.Fatalf("warning Event missing at-risk resources: %q", event)
		}
	default:
		t.Fatal("at-risk resources did not emit a warning Event")
	}
}

func TestForeignWorkloadsSkipsListedControllerDescendants(t *testing.T) {
	controller := boolPtr(true)
	owner := func(apiVersion, kind, name, uid string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid), Controller: controller}
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "deployment", UID: "deployment-uid",
	}}
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "deployment-rs", UID: "replicaset-uid",
		OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "Deployment", deployment.Name, string(deployment.UID))},
	}}
	daemonSet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "daemonset", UID: "daemonset-uid",
	}}
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "statefulset", UID: "statefulset-uid",
	}}
	replicationController := &corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "rc", UID: "rc-uid",
	}}
	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "cronjob", UID: "cronjob-uid",
	}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "cronjob-run", UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{owner("batch/v1", "CronJob", cronJob.Name, string(cronJob.UID))},
	}}
	standaloneReplicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: legacyKubeNamespace, Name: "standalone-rs", UID: "standalone-rs-uid",
	}}
	standalonePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "standalone-pod"}}

	objects := []client.Object{
		deployment,
		replicaSet,
		daemonSet,
		statefulSet,
		replicationController,
		cronJob,
		job,
		standaloneReplicaSet,
		standalonePod,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace, Name: "deployment-pod",
			OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", replicaSet.Name, string(replicaSet.UID))},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace, Name: "daemonset-pod",
			OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "DaemonSet", daemonSet.Name, string(daemonSet.UID))},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace, Name: "statefulset-pod",
			OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "StatefulSet", statefulSet.Name, string(statefulSet.UID))},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace, Name: "rc-pod",
			OwnerReferences: []metav1.OwnerReference{owner("v1", "ReplicationController", replicationController.Name, string(replicationController.UID))},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace, Name: "job-pod",
			OwnerReferences: []metav1.OwnerReference{owner("batch/v1", "Job", job.Name, string(job.UID))},
		}},
	}
	r := newReaper(t, objects...)

	got, err := r.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads: %v", err)
	}

	want := []string{
		"CronJob/cronjob",
		"DaemonSet/daemonset",
		"Deployment/deployment",
		"Pod/standalone-pod",
		"ReplicaSet/standalone-rs",
		"ReplicationController/rc",
		"StatefulSet/statefulset",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foreignWorkloads = %v, want %v", got, want)
	}
}

func TestForeignWorkloadsOnlySkipsTerminatingLegacyComponentDescendants(t *testing.T) {
	now := metav1.Now()
	r := newReaper(t,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace:         legacyKubeNamespace,
			Name:              "terminating-machina-pod",
			Labels:            map[string]string{"app": "machina-controller"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"test.unbounded-cloud.io/hold"},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace:         legacyNetNamespace,
			Name:              "terminating-net-node-pod",
			Labels:            map[string]string{appNameLabel: "unbounded-net-node"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"test.unbounded-cloud.io/hold"},
		}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Namespace:         legacyKubeNamespace,
			Name:              "terminating-metalman-rs",
			Labels:            map[string]string{"app": "unbounded-pxe"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"test.unbounded-cloud.io/hold"},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace,
			Name:      "active-machina-pod",
			Labels:    map[string]string{"app": "machina-controller"},
		}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace,
			Name:      "active-metalman-rs",
			Labels:    map[string]string{"app": "unbounded-pxe"},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: legacyKubeNamespace,
			Name:      "user-pod",
			Labels:    map[string]string{"app": "user-app"},
		}},
	)

	got, err := r.foreignWorkloads(t.Context(), legacyKubeNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads: %v", err)
	}

	want := []string{"Pod/active-machina-pod", "Pod/user-pod", "ReplicaSet/active-metalman-rs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foreignWorkloads = %v, want %v", got, want)
	}

	got, err = r.foreignWorkloads(t.Context(), legacyNetNamespace)
	if err != nil {
		t.Fatalf("foreignWorkloads(net): %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("foreignWorkloads(net) = %v, want empty", got)
	}
}

func TestNamespaceExistsFailsClosedOnAPIError(t *testing.T) {
	wantErr := errors.New("namespace lookup failed")
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).Build()
	r := &LegacyReaper{Client: base}
	r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return wantErr
		},
	})

	exists, err := r.namespaceExists(t.Context(), legacyKubeNamespace)
	if exists || !errors.Is(err, wantErr) {
		t.Fatalf("namespaceExists = %t, err=%v, want false and %v", exists, err, wantErr)
	}
}

func TestCleanupAbortsOnForeignWorkloadAuditError(t *testing.T) {
	wantErr := errors.New("pod list failed")
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(ns(legacyKubeNamespace)).Build()
	r := &LegacyReaper{Client: base, LegacyNamespaces: []string{legacyKubeNamespace}}
	r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return wantErr
			}

			return underlying.List(ctx, list, opts...)
		},
	})

	if _, err := r.cleanup(t.Context(), logr.Discard(), "unbounded-system"); !errors.Is(err, wantErr) {
		t.Fatalf("cleanup error = %v, want %v", err, wantErr)
	}

	if err := r.Get(t.Context(), client.ObjectKey{Name: legacyKubeNamespace}, &corev1.Namespace{}); err != nil {
		t.Fatalf("namespace deleted after failed audit: %v", err)
	}
}

func TestForeignWorkloadAuditPropagatesEveryListError(t *testing.T) {
	lists := []client.ObjectList{
		&corev1.PodList{},
		&corev1.ReplicationControllerList{},
		&appsv1.DeploymentList{},
		&appsv1.DaemonSetList{},
		&appsv1.StatefulSetList{},
		&appsv1.ReplicaSetList{},
		&batchv1.JobList{},
		&batchv1.CronJobList{},
	}

	for _, failedList := range lists {
		t.Run(reflect.TypeOf(failedList).Elem().Name(), func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).Build()
			wantErr := errors.New("list failed")
			r := &LegacyReaper{Client: base}
			r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if reflect.TypeOf(list) == reflect.TypeOf(failedList) {
						return wantErr
					}

					return underlying.List(ctx, list, opts...)
				},
			})

			if _, err := r.foreignWorkloads(t.Context(), legacyKubeNamespace); !errors.Is(err, wantErr) {
				t.Fatalf("foreignWorkloads error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestCleanupWarnsButDeletesNamespaceWithForeignWorkload(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(
		ns(legacyKubeNamespace),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: legacyKubeNamespace, Name: "user-pod"}},
	).Build()
	recorder := events.NewFakeRecorder(1)
	r := &LegacyReaper{Client: base, LegacyNamespaces: []string{legacyKubeNamespace}, Recorder: recorder}

	done, err := r.cleanup(t.Context(), logr.Discard(), "unbounded-system")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if done {
		t.Fatal("cleanup should wait for namespace deletion observation")
	}

	if err := base.Get(t.Context(), client.ObjectKey{Name: legacyKubeNamespace}, &corev1.Namespace{}); err == nil {
		t.Fatal("foreign workload finding blocked namespace deletion")
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "ForeignWorkloadsDeleted") || !strings.Contains(event, "Pod/user-pod") {
			t.Fatalf("unexpected warning Event: %q", event)
		}
	default:
		t.Fatal("foreign workload did not emit a warning Event")
	}
}

func TestNetTargetGateUsesLiveReader(t *testing.T) {
	const target = "unbounded-system"

	staleConfig, staleDeploy, staleDS := netConfigAndTargets(target, "stale: true", false)
	liveConfig, liveDeploy, liveDS := netConfigAndTargets(target, "live: true", false)
	staleDeploy.Spec.Template.Annotations[netConfigHashAnnotation] = "mismatch"

	cached := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(staleConfig, staleDeploy, staleDS).Build()
	live := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(liveConfig, liveDeploy, liveDS).Build()
	r := &LegacyReaper{Client: cached, APIReader: live}

	present, err := r.netTargetsPresent(t.Context(), target)
	if err != nil || !present {
		t.Fatalf("netTargetsPresent = %t, err=%v, want live-reader match", present, err)
	}
}

func TestNetTargetsPresentRejectsConfigMapTOCTOU(t *testing.T) {
	const target = "unbounded-system"

	configA, deploy, ds := netConfigAndTargets(target, "version: A", false)
	configA.ResourceVersion = "1"
	configB := configA.DeepCopy()
	configB.ResourceVersion = "2"
	configB.Data = map[string]string{"config.yaml": "version: B"}

	for _, tc := range []struct {
		name  string
		final *corev1.ConfigMap
		want  bool
	}{
		{name: "changed payload blocks", final: configB},
		{name: "stable payload passes", final: configA, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(
				deploy.DeepCopy(),
				ds.DeepCopy(),
			).Build()
			key := client.ObjectKeyFromObject(configA)
			r := &LegacyReaper{
				Client:    base,
				APIReader: configMapSequenceReader(t, base, key, configA, tc.final),
			}

			present, err := r.netTargetsPresent(t.Context(), target)
			if err != nil || present != tc.want {
				t.Fatalf("netTargetsPresent = %t, err=%v, want %t", present, err, tc.want)
			}
		})
	}
}

func TestNetTargetsPresentRevalidatesLegacySource(t *testing.T) {
	const target = "unbounded-system"

	targetConfig, deploy, ds := netConfigAndTargets(target, "version: A", false)
	targetConfig.ResourceVersion = "target-1"
	legacyConfig := targetConfig.DeepCopy()
	legacyConfig.Namespace = legacyNetNamespace
	legacyConfig.ResourceVersion = "legacy-1"
	changedLegacyConfig := legacyConfig.DeepCopy()
	changedLegacyConfig.ResourceVersion = "legacy-2"
	changedLegacyConfig.Data = map[string]string{"config.yaml": "version: B"}

	t.Run("source changed after copy blocks", func(t *testing.T) {
		r := newReaper(t,
			targetConfig.DeepCopy(),
			deploy.DeepCopy(),
			ds.DeepCopy(),
			changedLegacyConfig.DeepCopy(),
		)

		present, err := r.netTargetsPresent(t.Context(), target)
		if err != nil || present {
			t.Fatalf("netTargetsPresent = %t, err=%v, want false", present, err)
		}
	})

	for _, tc := range []struct {
		name        string
		finalSource *corev1.ConfigMap
		want        bool
	}{
		{name: "source changed during gate blocks", finalSource: changedLegacyConfig},
		{name: "stable matching source passes", finalSource: legacyConfig, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(
				deploy.DeepCopy(),
				ds.DeepCopy(),
			).Build()
			r := &LegacyReaper{
				Client: base,
				APIReader: configMapSequencesReader(t, base, map[client.ObjectKey][]*corev1.ConfigMap{
					client.ObjectKeyFromObject(targetConfig): {targetConfig, targetConfig},
					client.ObjectKeyFromObject(legacyConfig): {legacyConfig, tc.finalSource},
				}),
			}

			present, err := r.netTargetsPresent(t.Context(), target)
			if err != nil || present != tc.want {
				t.Fatalf("netTargetsPresent = %t, err=%v, want %t", present, err, tc.want)
			}
		})
	}
}

func TestNetTargetsPresentHandlesMissingLegacySource(t *testing.T) {
	const target = "unbounded-system"

	config, deploy, ds := netConfigAndTargets(target, "version: A", false)

	t.Run("active namespace blocks", func(t *testing.T) {
		r := newReaper(t, config.DeepCopy(), deploy.DeepCopy(), ds.DeepCopy(), ns(legacyNetNamespace))

		present, err := r.netTargetsPresent(t.Context(), target)
		if err != nil || present {
			t.Fatalf("netTargetsPresent = %t, err=%v, want false", present, err)
		}
	})

	t.Run("terminating namespace proceeds", func(t *testing.T) {
		namespace := ns(legacyNetNamespace)
		now := metav1.Now()
		namespace.DeletionTimestamp = &now
		namespace.Finalizers = []string{"test.unbounded-cloud.io/hold"}
		r := newReaper(t, config.DeepCopy(), deploy.DeepCopy(), ds.DeepCopy(), namespace)

		present, err := r.netTargetsPresent(t.Context(), target)
		if err != nil || !present {
			t.Fatalf("netTargetsPresent = %t, err=%v, want true", present, err)
		}
	})
}

func TestNetTargetsPresentFailsClosedOnLegacySourceError(t *testing.T) {
	const target = "unbounded-system"

	wantErr := errors.New("legacy source read failed")
	config, deploy, ds := netConfigAndTargets(target, "version: A", false)
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(config, deploy, ds).Build()
	r := &LegacyReaper{Client: base}
	r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key == (client.ObjectKey{Namespace: legacyNetNamespace, Name: "unbounded-net-config"}) {
				return wantErr
			}

			return underlying.Get(ctx, key, obj, opts...)
		},
	})

	present, err := r.netTargetsPresent(t.Context(), target)
	if present || !errors.Is(err, wantErr) {
		t.Fatalf("netTargetsPresent = %t, err=%v, want false and %v", present, err, wantErr)
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

func TestCleanupLegacySiteCRDRechecksLateSites(t *testing.T) {
	const target = "unbounded-system"

	replicas := int32(1)
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: legacySiteCRDName}}
	netController := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: target, Name: "unbounded-net-controller"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 100, AvailableReplicas: 1},
	}
	base := fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(crd, netController).Build()
	late := legacySite("late", map[string]any{"nodeCidrs": []any{"10.30.0.0/16"}})
	legacyLists := 0
	r := &LegacyReaper{Client: base}
	r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			unstructuredList, ok := list.(*unstructured.UnstructuredList)
			if ok && unstructuredList.GroupVersionKind() == (schema.GroupVersionKind{
				Group: legacySiteGVK.Group, Version: legacySiteGVK.Version, Kind: legacySiteGVK.Kind + "List",
			}) {
				legacyLists++
				if legacyLists == 3 {
					unstructuredList.Items = []unstructured.Unstructured{*late.DeepCopy()}

					return nil
				}
			}

			return underlying.List(ctx, list, opts...)
		},
	})

	// The normal migration pass does not see the Site; the cleanup-time live
	// verification must discover and translate it before deleting the CRD.
	if err := r.translateSites(t.Context(), logr.Discard()); err != nil {
		t.Fatalf("initial translateSites: %v", err)
	}

	if _, err := r.cleanupLegacySiteCRD(t.Context(), logr.Discard(), target); err != nil {
		t.Fatalf("cleanupLegacySiteCRD: %v", err)
	}

	translated := &unstructured.Unstructured{}
	translated.SetGroupVersionKind(newSiteGVK())

	if err := base.Get(t.Context(), client.ObjectKey{Name: late.GetName()}, translated); err != nil {
		t.Fatalf("late Site was not translated during cleanup verification: %v", err)
	}

	if legacyLists < 3 {
		t.Fatalf("legacy Site list count = %d, want at least 3", legacyLists)
	}
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
	exact.BlockOwnerDeletion = nil
	blocked := exact
	blocked.BlockOwnerDeletion = boolPtr(true)

	tests := []struct {
		name string
		objs []client.Object
		want bool
	}{
		{name: "zero slices pass", want: true},
		{name: "exact sole owner passes", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{exact})}, want: true},
		{name: "block owner deletion false passes", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{{APIVersion: exact.APIVersion, Kind: exact.Kind, Name: exact.Name, UID: exact.UID, Controller: exact.Controller, BlockOwnerDeletion: boolPtr(false)}})}, want: true},
		{name: "block owner deletion true blocks", objs: []client.Object{siteNodeSlice("edge-0", "edge", []metav1.OwnerReference{blocked})}},
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
