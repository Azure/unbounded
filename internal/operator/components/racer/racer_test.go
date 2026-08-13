// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerctrl"
)

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		appsv1.AddToScheme,
		corev1.AddToScheme,
		storagev1.AddToScheme,
		unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Namespace: component.DefaultNamespace,
	}
}

// enrolledNode builds a ready node carrying the enrollment and zone labels the
// allocator keys off, with whatever identity annotations the test wants.
func enrolledNode(name, zone string, annotations map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{EnrollmentLabel: "true", ZoneLabel: zone},
			Annotations: annotations,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// membershipMap builds the per-zone membership ConfigMap the operator writes
// and the node agents read.
func membershipMap(universe, zone uint32, members string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      racerctrl.MembershipConfigMapName(universe, zone),
			Namespace: component.DefaultNamespace,
			Labels:    racerctrl.MembershipLabels(universe, zone),
		},
		Data: map[string]string{racerctrl.MembershipDataKey: members},
	}
}

// zoneMembership reads a zone's published membership back out of its ConfigMap.
func zoneMembership(ctx context.Context, t *testing.T, env *component.Env, universe, zone uint32) string {
	t.Helper()

	return membershipData(ctx, t, env, universe, zone)[racerctrl.MembershipDataKey]
}

// zoneMembershipEpoch reads the epoch a zone's membership was published at.
func zoneMembershipEpoch(ctx context.Context, t *testing.T, env *component.Env, universe, zone uint32) string {
	t.Helper()

	return membershipData(ctx, t, env, universe, zone)[racerctrl.MembershipEpochKey]
}

func membershipData(ctx context.Context, t *testing.T, env *component.Env, universe, zone uint32) map[string]string {
	t.Helper()

	found := &corev1.ConfigMap{}
	key := client.ObjectKey{
		Namespace: env.Namespace,
		Name:      racerctrl.MembershipConfigMapName(universe, zone),
	}

	if err := env.Client.Get(ctx, key, found); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		t.Fatalf("get membership configmap: %v", err)
	}

	return found.Data
}

func racerClass(name string, annotations map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Annotations: annotations},
		Provisioner: racerctrl.DriverName,
	}
}

func racerVolume(name, class, capacity string, attributes map[string]string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: class,
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           racerctrl.DriverName,
					VolumeHandle:     name,
					VolumeAttributes: attributes,
				},
			},
		},
	}
}

func TestEnabledForDefaultsDisabled(t *testing.T) {
	if EnabledFor(&unboundedv1alpha3.Site{}) {
		t.Fatal("racer enabled with no component spec")
	}

	disabled := false
	off := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Racer: &unboundedv1alpha3.RacerComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &disabled},
		},
	}}}

	if EnabledFor(off) {
		t.Fatal("racer enabled when the spec disables it")
	}

	enabled := true
	on := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Racer: &unboundedv1alpha3.RacerComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		},
	}}}

	if !EnabledFor(on) {
		t.Fatal("racer not enabled when the spec enables it")
	}
}

func TestReconcileDisabledWithNothingInstalled(t *testing.T) {
	env := testEnv(t)

	result := (Component{}).Reconcile(context.Background(), env, nil)
	if result.Reason != component.ReasonDisabled {
		t.Fatalf("expected the disabled reason, got %+v", result)
	}
}

// A racer installation holds data. Once the DaemonSet exists, disabling the
// component must keep reconciling rather than uninstall it out from under the
// extents it is serving.
func TestReconcileRetainsInstallationWhenDisabled(t *testing.T) {
	env := testEnv(t, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName},
	})

	result := (Component{}).Reconcile(context.Background(), env, nil)
	if result.Reason == component.ReasonDisabled {
		t.Fatal("racer uninstalled itself while its DaemonSet was still deployed")
	}
}

func TestApplyMutatorStampsBothImages(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"initContainers": []any{map[string]any{"name": "preflight", "image": "old:preflight"}},
					"containers": []any{
						map[string]any{"name": ctrlContainerName, "image": "old:ctrl"},
						map[string]any{"name": racerContainerName, "image": "old:racer"},
						map[string]any{"name": "registrar", "image": "pinned:registrar"},
					},
				},
			},
		},
	}}

	if err := applyMutator("reg/racer-ctrl:v1", "reg/racer:v1")(obj); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatalf("read containers: %v", err)
	}

	images := map[string]string{}

	for _, entry := range containers {
		container, _ := entry.(map[string]any)
		name, _ := container["name"].(string)
		image, _ := container["image"].(string)
		images[name] = image
	}

	if images[ctrlContainerName] != "reg/racer-ctrl:v1" {
		t.Fatalf("racer-ctrl image not stamped: %q", images[ctrlContainerName])
	}

	if images[racerContainerName] != "reg/racer:v1" {
		t.Fatalf("racer image not stamped: %q", images[racerContainerName])
	}

	if images["registrar"] != "pinned:registrar" {
		t.Fatalf("pinned sidecar image was rewritten: %q", images["registrar"])
	}
}

// A StorageClass is a universe, and its annotations are written by patch. Server
// side applying the manifest copy would strip them, so the mutator drops it.
func TestApplyMutatorSkipsStorageClass(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata":   map[string]any{"name": defaultClassName},
	}}

	if err := applyMutator("a", "b")(obj); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	if obj.Object != nil {
		t.Fatal("StorageClass was not skipped")
	}
}

func TestEnsureDefaultClassCreatesOneUniverse(t *testing.T) {
	ctx := context.Background()
	env := testEnv(t)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.ensureDefaultClass(ctx); err != nil {
		t.Fatalf("ensure default class: %v", err)
	}

	class := &storagev1.StorageClass{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: defaultClassName}, class); err != nil {
		t.Fatalf("get default class: %v", err)
	}

	if class.Provisioner != racerctrl.DriverName {
		t.Fatalf("default class provisioner is %q", class.Provisioner)
	}

	if class.AllowVolumeExpansion == nil || *class.AllowVolumeExpansion {
		t.Fatal("default class allows expansion; extents are frozen for life")
	}

	// A second pass must not create a second universe.
	second, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}

	if err := second.ensureDefaultClass(ctx); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	classes := &storagev1.StorageClassList{}
	if err := env.Client.List(ctx, classes); err != nil {
		t.Fatalf("list classes: %v", err)
	}

	if len(classes.Items) != 1 {
		t.Fatalf("expected one storage class, got %d", len(classes.Items))
	}
}

func TestAllocateNodeIdentitiesIsUniqueAndBalanced(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{}
	for _, name := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		objects = append(objects, enrolledNode(name, "east", nil))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.allocateNodeIdentities(ctx); err != nil {
		t.Fatalf("allocate identities: %v", err)
	}

	nodes := &corev1.NodeList{}
	if err := env.Client.List(ctx, nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}

	ids := map[string]struct{}{}
	cohorts := map[string]int{}

	for i := range nodes.Items {
		annotations := nodes.Items[i].Annotations

		id := annotations[racerctrl.NodeIDAnnotation]
		if id == "" || id == "0" {
			t.Fatalf("node %s has no id", nodes.Items[i].Name)
		}

		if _, seen := ids[id]; seen {
			t.Fatalf("node id %s handed out twice", id)
		}

		ids[id] = struct{}{}

		if annotations[racerctrl.NodeZoneAnnotation] == "" {
			t.Fatalf("node %s has no zone", nodes.Items[i].Name)
		}

		cohorts[annotations[racerctrl.NodeCohortAnnotation]]++
	}

	if len(cohorts) != racerctrl.Cohorts {
		t.Fatalf("expected %d cohorts, got %d: %v", racerctrl.Cohorts, len(cohorts), cohorts)
	}

	for cohort, count := range cohorts {
		if count != 2 {
			t.Fatalf("cohort %s holds %d of six nodes; trios need equal cohorts", cohort, count)
		}
	}
}

// Identity is idempotent: a node that already has one keeps it, because the id
// is what every other node's catalog names.
func TestAllocateNodeIdentitiesLeavesExistingAlone(t *testing.T) {
	ctx := context.Background()
	env := testEnv(t, enrolledNode("n1", "east", map[string]string{
		racerctrl.NodeIDAnnotation:     "7",
		racerctrl.NodeZoneAnnotation:   "3",
		racerctrl.NodeCohortAnnotation: "2",
	}))

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.allocateNodeIdentities(ctx); err != nil {
		t.Fatalf("allocate identities: %v", err)
	}

	node := &corev1.Node{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "n1"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}

	if node.Annotations[racerctrl.NodeIDAnnotation] != "7" {
		t.Fatalf("existing node id was rewritten to %q", node.Annotations[racerctrl.NodeIDAnnotation])
	}
}

func TestAllocateUniversesStampsCursorsOnce(t *testing.T) {
	ctx := context.Background()
	env := testEnv(t, racerClass("fast", nil), racerClass("bulk", nil))

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.allocateUniverses(ctx); err != nil {
		t.Fatalf("allocate universes: %v", err)
	}

	classes := &storagev1.StorageClassList{}
	if err := env.Client.List(ctx, classes); err != nil {
		t.Fatalf("list classes: %v", err)
	}

	seen := map[string]string{}

	for i := range classes.Items {
		annotations := classes.Items[i].Annotations

		id := annotations[racerctrl.UniverseIDAnnotation]
		if id == "" || id == "0" {
			t.Fatalf("class %s has no universe id", classes.Items[i].Name)
		}

		if other, clash := seen[id]; clash {
			t.Fatalf("universe id %s shared by %s and %s", id, other, classes.Items[i].Name)
		}

		seen[id] = classes.Items[i].Name

		if annotations[racerctrl.CatalogSizeAnnotation] == "" {
			t.Fatalf("class %s has no catalog size", classes.Items[i].Name)
		}

		if annotations[racerctrl.EpochAnnotation] == "" {
			t.Fatalf("class %s has no epoch", classes.Items[i].Name)
		}

		if _, ok := annotations[racerctrl.NextLBAAnnotation]; !ok {
			t.Fatalf("class %s has no LBA cursor", classes.Items[i].Name)
		}
	}

	// A second pass must be a no-op, not a reallocation.
	before := seen

	second, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}

	if err := second.allocateUniverses(ctx); err != nil {
		t.Fatalf("second allocate: %v", err)
	}

	if err := env.Client.List(ctx, classes); err != nil {
		t.Fatalf("relist classes: %v", err)
	}

	for i := range classes.Items {
		id := classes.Items[i].Annotations[racerctrl.UniverseIDAnnotation]
		if before[id] != classes.Items[i].Name {
			t.Fatalf("class %s changed universe id to %s", classes.Items[i].Name, id)
		}
	}
}

func TestPlaceVolumeStampsCompositionAndFinalizer(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
		membershipMap(1, 1, "1?cohort=0,2?cohort=1,3?cohort=2"),
		racerVolume("pv-a", "fast", "68Mi", map[string]string{
			racerctrl.AttrMutableBytes:      "4Mi",
			racerctrl.AttrMutableKind:       "OCC",
			racerctrl.AttrImmutablePageSize: "4Mi",
		}),
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
		}))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.allocateVolumes(ctx); err != nil {
		t.Fatalf("allocate volumes: %v", err)
	}

	pv := &corev1.PersistentVolume{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "pv-a"}, pv); err != nil {
		t.Fatalf("get volume: %v", err)
	}

	raw := pv.Annotations[racerctrl.CompositionAnnotation]
	if raw == "" {
		t.Fatalf("volume has no composition: %v (waiting: %v)", pv.Annotations, p.waiting)
	}

	composition, err := racerctrl.ParseComposition(raw)
	if err != nil {
		t.Fatalf("parse composition %q: %v", raw, err)
	}

	if len(composition) != 2 {
		t.Fatalf("expected a mutable head and an immutable tail, got %d segments", len(composition))
	}

	if composition.Bytes() != 68<<20 {
		t.Fatalf("composition covers %d bytes, want %d", composition.Bytes(), 68<<20)
	}

	if pv.Annotations[racerctrl.VolumeZoneAnnotation] != "1" {
		t.Fatalf("volume home zone is %q", pv.Annotations[racerctrl.VolumeZoneAnnotation])
	}

	if pv.Annotations[racerctrl.PhaseAnnotation] != racerctrl.PhaseActive {
		t.Fatalf("volume phase is %q", pv.Annotations[racerctrl.PhaseAnnotation])
	}

	if !hasFinalizer(pv.Finalizers, racerctrl.VolumeFinalizer) {
		t.Fatal("volume has no collection finalizer; its extents could be forgotten")
	}

	// The class cursor must have moved past the allocation, or the next volume
	// would overlap this one.
	class := &storagev1.StorageClass{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fast"}, class); err != nil {
		t.Fatalf("get class: %v", err)
	}

	next, err := racerctrl.NextLBA(class.Annotations)
	if err != nil {
		t.Fatalf("parse next lba: %v", err)
	}

	last := composition[len(composition)-1]
	if next < last.BaseLBA+last.Blocks() {
		t.Fatalf("lba cursor %d does not clear the allocation ending at %d", next, last.BaseLBA+last.Blocks())
	}
}

// Composition is stamped once. Re-running the allocator must not re-place a
// volume, because base_lba, pages and kind are frozen for the extent's life.
func TestPlaceVolumeIsIdempotent(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
		membershipMap(1, 1, "1?cohort=0,2?cohort=1,3?cohort=2"),
		racerVolume("pv-a", "fast", "64Mi", nil),
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
		}))
	}

	env := testEnv(t, objects...)

	first, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := first.allocateVolumes(ctx); err != nil {
		t.Fatalf("allocate volumes: %v", err)
	}

	pv := &corev1.PersistentVolume{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "pv-a"}, pv); err != nil {
		t.Fatalf("get volume: %v", err)
	}

	stamped := pv.Annotations[racerctrl.CompositionAnnotation]
	if stamped == "" {
		t.Fatalf("volume was not placed: %v", first.waiting)
	}

	second, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}

	if err := second.allocateVolumes(ctx); err != nil {
		t.Fatalf("second allocate: %v", err)
	}

	if err := env.Client.Get(ctx, client.ObjectKey{Name: "pv-a"}, pv); err != nil {
		t.Fatalf("re-get volume: %v", err)
	}

	if pv.Annotations[racerctrl.CompositionAnnotation] != stamped {
		t.Fatalf("composition changed from %q to %q", stamped, pv.Annotations[racerctrl.CompositionAnnotation])
	}
}

func TestReconcileMembershipPublishesABalancedZone(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
			// A quiescent node, so the healing gate does not hold the step.
			racerctrl.NodeHealthAnnotation: "generation=1",
		}))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	raw := zoneMembership(ctx, t, env, 1, 1)
	if raw == "" {
		t.Fatalf("zone 1 has no membership (waiting: %v)", p.waiting)
	}

	members, err := racerctrl.ParseMembership(raw)
	if err != nil {
		t.Fatalf("parse membership %q: %v", raw, err)
	}

	if len(members) != 3 {
		t.Fatalf("expected three members, got %d", len(members))
	}

	// The membership must build a legal catalog, or the nodes will reject it.
	if _, err := racerctrl.BuildCatalog(members, 3); err != nil {
		t.Fatalf("published membership does not build a catalog: %v", err)
	}
}

// A membership change is a new configuration, and the epoch is what orders
// configurations. Publishing one without bumping the epoch loses that ordering.
func TestReconcileMembershipAdvancesTheEpoch(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "4",
			racerctrl.NextLBAAnnotation:     "0",
		}),
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
			racerctrl.NodeHealthAnnotation: "generation=1",
		}))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	class := &storagev1.StorageClass{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fast"}, class); err != nil {
		t.Fatalf("get class: %v", err)
	}

	if class.Annotations[racerctrl.EpochAnnotation] != "5" {
		t.Fatalf("epoch is %q, want 5", class.Annotations[racerctrl.EpochAnnotation])
	}
}

// Replacement is one node at a time and only while nothing is healing. A member
// still replaying must hold the next step, or two groups lose a replica at once.
func TestReconcileMembershipWaitsWhileHealing(t *testing.T) {
	ctx := context.Background()

	const seeded = "1?cohort=0,2?cohort=1,3?cohort=2"

	env := testEnv(t, replacementObjects(seeded, map[string]string{
		"n2": "generation=1&replaying=2",
	})...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	if got := zoneMembership(ctx, t, env, 1, 1); got != seeded {
		t.Fatalf("membership became %q while a member was still replaying", got)
	}

	if len(p.waiting) == 0 {
		t.Fatal("healing gate did not record a reason to wait")
	}
}

// The healing gate is a question about the nodes holding groups. A candidate has
// no health to report until a membership names it and something hands it a
// config, so gating on the candidate's silence would make it impossible to ever
// add a node.
func TestReconcileMembershipAddsANodeThatHasNeverReported(t *testing.T) {
	ctx := context.Background()

	const seeded = "1?cohort=0,2?cohort=1,3?cohort=2"

	// n4 carries no health annotation at all, which is what a node that has
	// never run racer looks like.
	env := testEnv(t, replacementObjects(seeded, nil)...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	raw := zoneMembership(ctx, t, env, 1, 1)
	if raw == seeded {
		t.Fatalf("membership is still %q; the candidate blocked its own addition (waiting: %v)", raw, p.waiting)
	}

	members, err := racerctrl.ParseMembership(raw)
	if err != nil {
		t.Fatalf("parse membership %q: %v", raw, err)
	}

	if !members.Contains(4) {
		t.Fatalf("membership %q does not name the candidate", raw)
	}
}

// A cluster where nothing has ever run racer has to be able to publish its first
// membership: until one exists no node has a config, and until a node has a
// config none of them can report any health at all.
func TestReconcileMembershipBootstrapsWithNoHealthReported(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
		}))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	if zoneMembership(ctx, t, env, 1, 1) == "" {
		t.Fatalf("no first membership was published (waiting: %v)", p.waiting)
	}
}

// appliedAt is the annotation an agent writes to say which generation it
// installed and which universe epoch that generation carried. Every gate needs
// it: racer reports only which generation is in force, never what is in it.
func appliedAt(generation uint64, universe, epoch uint32) string {
	return racerctrl.FormatApplied(racerctrl.Applied{
		Generation: generation,
		Epochs:     map[uint32]uint32{universe: epoch},
	})
}

// replacementObjects builds a zone whose membership is already published and
// whose first member has gone away, so exactly one swap is wanted. health names
// the health annotation each node reports; a node absent from it reports none.
func replacementObjects(seeded string, health map[string]string) []client.Object {
	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
		membershipMap(1, 1, seeded),
	}

	// n1 holds cohort 0 and is no longer Ready, so n4 is the only candidate for
	// its slot and the desired membership differs from the published one by
	// exactly that id.
	for i, name := range []string{"n1", "n2", "n3", "n4"} {
		annotations := map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i) % 3),
		}

		if value, ok := health[name]; ok {
			annotations[racerctrl.NodeHealthAnnotation] = value
			annotations[racerctrl.NodeAppliedAnnotation] = appliedAt(1, 1, 1)
		} else if name != "n4" {
			annotations[racerctrl.NodeHealthAnnotation] = "generation=1"
			annotations[racerctrl.NodeAppliedAnnotation] = appliedAt(1, 1, 1)
		}

		node := enrolledNode(name, "east", annotations)
		if name == "n1" {
			node.Status.Conditions = []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			}
		}

		objects = append(objects, node)
	}

	return objects
}

// settledObjects is a zone whose published membership is already the one the
// planner wants, so nothing steps and only the epoch bookkeeping runs.
func settledObjects(classEpoch string, membership *corev1.ConfigMap) []client.Object {
	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       classEpoch,
			racerctrl.NextLBAAnnotation:     "0",
		}),
		membership,
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
			racerctrl.NodeHealthAnnotation: "generation=1",
		}))
	}

	return objects
}

// datedMembershipMap is a membership published with the epoch that names it.
func datedMembershipMap(universe, zone uint32, members, epoch string) *corev1.ConfigMap {
	item := membershipMap(universe, zone, members)
	item.Data[racerctrl.MembershipEpochKey] = epoch

	return item
}

// A new catalog and the epoch that names it are one write. Publishing the
// catalog and leaving the epoch to a second write that may never land would
// leave a configuration the cluster cannot tell apart from the one before it.
func TestReconcileMembershipPublishesTheEpochWithTheMembership(t *testing.T) {
	ctx := context.Background()

	env := testEnv(t, replacementObjects("1?cohort=0,2?cohort=1,3?cohort=2", nil)...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	if got := zoneMembership(ctx, t, env, 1, 1); !strings.Contains(got, "4?cohort=0") {
		t.Fatalf("membership is %q, want node 4 to have taken the slot", got)
	}

	if got := zoneMembershipEpoch(ctx, t, env, 1, 1); got != "2" {
		t.Fatalf("membership was published at epoch %q, want 2", got)
	}

	class := &storagev1.StorageClass{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fast"}, class); err != nil {
		t.Fatalf("get class: %v", err)
	}

	if got := class.Annotations[racerctrl.EpochAnnotation]; got != "2" {
		t.Fatalf("epoch cursor is %q, want the 2 it handed out", got)
	}
}

// The cursor moves after the catalog, so a crash in between leaves a catalog
// dated ahead of the class. The catalog is the published one and the nodes are
// already running it, so the cursor catches up rather than the catalog being
// rewritten: a cursor that lagged would hand the same epoch out twice.
func TestReconcileMembershipRecoversALostEpochCursor(t *testing.T) {
	ctx := context.Background()

	const seeded = "1?cohort=0,2?cohort=1,3?cohort=2"

	env := testEnv(t, settledObjects("1", datedMembershipMap(1, 1, seeded, "2"))...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	class := &storagev1.StorageClass{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fast"}, class); err != nil {
		t.Fatalf("get class: %v", err)
	}

	if got := class.Annotations[racerctrl.EpochAnnotation]; got != "2" {
		t.Fatalf("epoch cursor is %q, want it caught up to the published 2", got)
	}

	if got := zoneMembership(ctx, t, env, 1, 1); got != seeded {
		t.Fatalf("membership became %q, want the published catalog left alone", got)
	}

	if got := zoneMembershipEpoch(ctx, t, env, 1, 1); got != "2" {
		t.Fatalf("membership epoch became %q, want the 2 it was published at", got)
	}
}

// A membership written before the epoch travelled with it is stamped with the
// class's epoch, which is the epoch the nodes reading it are already running.
func TestReconcileMembershipDatesAnUndatedMembership(t *testing.T) {
	ctx := context.Background()

	const seeded = "1?cohort=0,2?cohort=1,3?cohort=2"

	env := testEnv(t, settledObjects("4", membershipMap(1, 1, seeded))...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileMembership(ctx); err != nil {
		t.Fatalf("reconcile membership: %v", err)
	}

	if got := zoneMembershipEpoch(ctx, t, env, 1, 1); got != "4" {
		t.Fatalf("membership epoch is %q, want the class's 4", got)
	}

	if got := zoneMembership(ctx, t, env, 1, 1); got != seeded {
		t.Fatalf("membership became %q, want stamping to change nothing else", got)
	}

	class := &storagev1.StorageClass{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fast"}, class); err != nil {
		t.Fatalf("get class: %v", err)
	}

	if got := class.Annotations[racerctrl.EpochAnnotation]; got != "4" {
		t.Fatalf("epoch cursor moved to %q, want stamping to spend no epoch", got)
	}
}

func TestReconcileStateReportsBlockedGatesAsNotReady(t *testing.T) {
	ctx := context.Background()

	// A lone enrolled node cannot form a balanced membership, and the pass has
	// to create the default storage class before it can do anything at all, so
	// there is something to wait on.
	node := enrolledNode("n1", "", nil)
	delete(node.Labels, ZoneLabel)

	env := testEnv(t, node)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	result := p.reconcileState(ctx)
	if result.Ready {
		t.Fatalf("expected not ready, got %+v", result)
	}

	if result.RequeueAfter != requeueInterval {
		t.Fatalf("expected a %s requeue, got %s", requeueInterval, result.RequeueAfter)
	}

	if result.Err != nil {
		t.Fatalf("a blocked gate is not an error, got %v", result.Err)
	}
}

func TestReconcileStateIsReadyWhenSettled(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
			racerctrl.GatewaysAnnotation(1): "1,2,3",
		}),
		membershipMap(1, 1, "1?cohort=0,2?cohort=1,3?cohort=2"),
	}

	for i, name := range []string{"n1", "n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 1),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i)),
			racerctrl.NodeHealthAnnotation: "generation=1",
		}))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	result := p.reconcileState(ctx)
	if !result.Ready {
		t.Fatalf("expected ready, got %+v", result)
	}
}

// nodeLabels reads a node's labels back from the API server.
func nodeLabels(ctx context.Context, t *testing.T, env *component.Env, name string) map[string]string {
	t.Helper()

	found := &corev1.Node{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: name}, found); err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}

	return found.Labels
}

// decommissioningNode is a node that has been un-enrolled but still holds the
// identity it was allocated, which is what every node in the middle of a
// decommission looks like.
func decommissioningNode(name string, id uint32) *corev1.Node {
	node := enrolledNode(name, "east", map[string]string{
		racerctrl.NodeIDAnnotation:      formatUint(uint64(id)),
		racerctrl.NodeZoneAnnotation:    "1",
		racerctrl.NodeCohortAnnotation:  "0",
		racerctrl.NodeHealthAnnotation:  "generation=4",
		racerctrl.NodeAppliedAnnotation: appliedAt(4, 1, 4),
	})

	delete(node.Labels, EnrollmentLabel)
	node.Labels[WorkloadLabel] = "true"

	return node
}

func TestWorkloadLabelIsAddedToEnrolledNodes(t *testing.T) {
	ctx := context.Background()

	env := testEnv(t, enrolledNode("n1", "east", nil))

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileWorkloadLabels(ctx); err != nil {
		t.Fatalf("reconcile workload labels: %v", err)
	}

	if labels := nodeLabels(ctx, t, env, "n1"); labels[WorkloadLabel] != "true" {
		t.Fatalf("enrolled node is not labelled for the workload: %v", labels)
	}
}

// Un-enrolling is a request to decommission, not an instruction to stop. The
// node has to keep running racer to accept the configs that step it out of each
// catalog and to shed what it holds, so the label the DaemonSet selects on has
// to outlive the enrollment label.
func TestWorkloadLabelSurvivesUnenrollment(t *testing.T) {
	ctx := context.Background()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: "3",
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
		membershipMap(1, 1, "1?cohort=0,2?cohort=1,3?cohort=2"),
		decommissioningNode("n1", 1),
	}

	for i, name := range []string{"n2", "n3"} {
		objects = append(objects, enrolledNode(name, "east", map[string]string{
			racerctrl.NodeIDAnnotation:     formatUint(uint64(i) + 2),
			racerctrl.NodeZoneAnnotation:   "1",
			racerctrl.NodeCohortAnnotation: formatUint(uint64(i) + 1),
			racerctrl.NodeHealthAnnotation: "generation=4",
			WorkloadLabel:                  "true",
		}))
	}

	env := testEnv(t, objects...)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileWorkloadLabels(ctx); err != nil {
		t.Fatalf("reconcile workload labels: %v", err)
	}

	if err := p.sequence(ctx); err != nil {
		t.Fatalf("sequence: %v", err)
	}

	labels := nodeLabels(ctx, t, env, "n1")
	if labels[WorkloadLabel] != "true" {
		t.Fatalf("the departing node lost its workload label while the catalog still holds it: %v", labels)
	}

	found := &corev1.Node{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "n1"}, found); err != nil {
		t.Fatalf("get node: %v", err)
	}

	if found.Annotations[racerctrl.NodeIDAnnotation] != "1" {
		t.Fatalf("the departing node lost its identity while the catalog still holds it: %v", found.Annotations)
	}
}

// Retirement is the first moment there is nothing left for the node to serve, so
// it is the first moment the pod can go, and the identity and the label go
// together.
func TestRetirementDropsTheWorkloadLabelWithTheIdentity(t *testing.T) {
	ctx := context.Background()

	env := testEnv(t, decommissioningNode("n1", 7))

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileWorkloadLabels(ctx); err != nil {
		t.Fatalf("reconcile workload labels: %v", err)
	}

	if err := p.sequence(ctx); err != nil {
		t.Fatalf("sequence: %v", err)
	}

	found := &corev1.Node{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "n1"}, found); err != nil {
		t.Fatalf("get node: %v", err)
	}

	if _, ok := found.Annotations[racerctrl.NodeIDAnnotation]; ok {
		t.Fatalf("retired node kept its id: %v", found.Annotations)
	}

	if _, ok := found.Labels[WorkloadLabel]; ok {
		t.Fatalf("retired node is still selected by the DaemonSet: %v", found.Labels)
	}
}

// A node that is neither enrolled nor holds an identity has no business running
// racer, however its label got there.
func TestWorkloadLabelIsRemovedFromStrayNodes(t *testing.T) {
	ctx := context.Background()

	stray := enrolledNode("n1", "east", nil)
	delete(stray.Labels, EnrollmentLabel)
	stray.Labels[WorkloadLabel] = "true"

	env := testEnv(t, stray)

	p, err := loadState(ctx, env)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	if err := p.reconcileWorkloadLabels(ctx); err != nil {
		t.Fatalf("reconcile workload labels: %v", err)
	}

	if _, ok := nodeLabels(ctx, t, env, "n1")[WorkloadLabel]; ok {
		t.Fatalf("a node with no enrollment and no identity is still labelled for the workload")
	}
}

// The DaemonSet must select on the operator-owned label rather than on
// enrollment. This reads the template rather than the rendered output because
// the rendered tree is generated and gitignored, and the template is the source
// of truth either way.
func TestDaemonSetSelectsTheWorkloadLabel(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "racer", "03-daemonset.yaml.tmpl"))
	if err != nil {
		t.Fatalf("read daemonset template: %v", err)
	}

	manifest := string(raw)

	if !strings.Contains(manifest, WorkloadLabel+`: "true"`) {
		t.Fatalf("the daemonset does not select on %s", WorkloadLabel)
	}

	if strings.Contains(manifest, EnrollmentLabel+`: "true"`) {
		t.Fatalf("the daemonset still selects on %s, so un-enrolling a node would delete its pod", EnrollmentLabel)
	}
}

// scalingZone drives the membership planner the way the reconciler does: a
// pass, then every node reporting that it loaded what the pass published. It
// exists so a test can widen and narrow a zone and watch every catalog the
// operator publishes on the way.
type scalingZone struct {
	t   *testing.T
	ctx context.Context
	env *component.Env
}

// newScalingZone builds a zone of catalogSize groups with seven nodes given
// identities, of which the first three are enrolled. A node's cohort follows
// its id, because a cohort is frozen for a node's life.
func newScalingZone(t *testing.T, ctx context.Context, catalogSize int) *scalingZone {
	t.Helper()

	objects := []client.Object{
		racerClass("fast", map[string]string{
			racerctrl.UniverseIDAnnotation:  "1",
			racerctrl.CatalogSizeAnnotation: formatUint(uint64(catalogSize)),
			racerctrl.EpochAnnotation:       "1",
			racerctrl.NextLBAAnnotation:     "0",
		}),
	}

	for id := 1; id <= 7; id++ {
		node := enrolledNode(scalingName(id), "east", map[string]string{
			racerctrl.NodeIDAnnotation:      formatUint(uint64(id)),
			racerctrl.NodeZoneAnnotation:    "1",
			racerctrl.NodeCohortAnnotation:  formatUint(uint64(id-1) % 3),
			racerctrl.NodeHealthAnnotation:  "generation=1",
			racerctrl.NodeAppliedAnnotation: appliedAt(1, 1, 1),
		})

		if id > 3 {
			delete(node.Labels, EnrollmentLabel)
		}

		objects = append(objects, node)
	}

	return &scalingZone{t: t, ctx: ctx, env: testEnv(t, objects...)}
}

func scalingName(id int) string {
	return "n" + formatUint(uint64(id))
}

// widen enrolls or unenrolls nodes so the zone belongs to exactly width nodes.
func (z *scalingZone) widen(width int) {
	z.t.Helper()

	for id := 1; id <= 7; id++ {
		node := &corev1.Node{}
		if err := z.env.Client.Get(z.ctx, client.ObjectKey{Name: scalingName(id)}, node); err != nil {
			z.t.Fatalf("get node: %v", err)
		}

		if id <= width {
			node.Labels[EnrollmentLabel] = "true"
		} else {
			delete(node.Labels, EnrollmentLabel)
		}

		if err := z.env.Client.Update(z.ctx, node); err != nil {
			z.t.Fatalf("update node: %v", err)
		}
	}
}

// settle reconciles until the zone stops asking to be reconciled again,
// checking every catalog it publishes against the rules on the way.
func (z *scalingZone) settle() racerctrl.Catalog {
	z.t.Helper()

	const passes = 60

	for range passes {
		before := z.catalog()
		wasMembers := z.members()

		p, err := loadState(z.ctx, z.env)
		if err != nil {
			z.t.Fatalf("load state: %v", err)
		}

		if err := p.reconcileMembership(z.ctx); err != nil {
			z.t.Fatalf("reconcile membership: %v", err)
		}

		z.assertStepIsLegal(before, z.catalog(), wasMembers, z.members())
		z.report()

		if len(p.waiting) == 0 {
			return z.catalog()
		}
	}

	z.t.Fatalf("zone never settled in %d passes, catalog %v", passes, z.catalog())

	return nil
}

// assertStepIsLegal holds one published step to the two rules: a group keeps a
// quorum of the nodes it had, and the zone gains at most one id and loses at
// most one.
func (z *scalingZone) assertStepIsLegal(before, after racerctrl.Catalog, was, is racerctrl.Membership) {
	z.t.Helper()

	if len(before) == 0 {
		return
	}

	if len(before) != len(after) {
		z.t.Fatalf("catalog length changed from %d to %d", len(before), len(after))
	}

	for i := range before {
		if kept := after[i].Survivors(before[i]); kept < racerctrl.Quorum {
			z.t.Fatalf("group %d kept only %d of %v, became %v", i, kept, before[i], after[i])
		}
	}

	var joins, departures int

	for _, member := range is {
		if !was.Contains(member.NodeID) {
			joins++
		}
	}

	for _, member := range was {
		if !is.Contains(member.NodeID) {
			departures++
		}
	}

	if joins > 1 || departures > 1 {
		z.t.Fatalf("membership changed by %d joins and %d departures in one step", joins, departures)
	}
}

// report has every node say it loaded the configuration the last pass
// published, which is what the gates wait for.
func (z *scalingZone) report() {
	z.t.Helper()

	epoch := z.epoch()

	for id := 1; id <= 7; id++ {
		node := &corev1.Node{}
		if err := z.env.Client.Get(z.ctx, client.ObjectKey{Name: scalingName(id)}, node); err != nil {
			z.t.Fatalf("get node: %v", err)
		}

		node.Annotations[racerctrl.NodeHealthAnnotation] = "generation=" + formatUint(uint64(epoch))
		node.Annotations[racerctrl.NodeAppliedAnnotation] = appliedAt(uint64(epoch), 1, epoch)

		if err := z.env.Client.Update(z.ctx, node); err != nil {
			z.t.Fatalf("update node: %v", err)
		}
	}
}

func (z *scalingZone) epoch() uint32 {
	z.t.Helper()

	raw := zoneMembershipEpoch(z.ctx, z.t, z.env, 1, 1)
	if raw == "" {
		return 1
	}

	epoch, err := racerctrl.ParseMembershipEpoch(map[string]string{racerctrl.MembershipEpochKey: raw})
	if err != nil {
		z.t.Fatalf("parse epoch %q: %v", raw, err)
	}

	return epoch
}

func (z *scalingZone) catalog() racerctrl.Catalog {
	z.t.Helper()

	catalog, err := racerctrl.ParseCatalog(membershipData(z.ctx, z.t, z.env, 1, 1)[racerctrl.MembershipCatalogKey])
	if err != nil {
		z.t.Fatalf("parse catalog: %v", err)
	}

	return catalog
}

func (z *scalingZone) members() racerctrl.Membership {
	z.t.Helper()

	raw := zoneMembership(z.ctx, z.t, z.env, 1, 1)
	if raw == "" {
		return nil
	}

	members, err := racerctrl.ParseMembership(raw)
	if err != nil {
		z.t.Fatalf("parse membership %q: %v", raw, err)
	}

	return members
}

func (z *scalingZone) draining() racerctrl.Membership {
	z.t.Helper()

	raw := membershipData(z.ctx, z.t, z.env, 1, 1)[racerctrl.MembershipDrainingKey]
	if raw == "" {
		return nil
	}

	members, err := racerctrl.ParseMembership(raw)
	if err != nil {
		z.t.Fatalf("parse draining %q: %v", raw, err)
	}

	return members
}

// balance is the widest gap in groups held between two nodes of one cohort.
func (z *scalingZone) balance(catalog racerctrl.Catalog) int {
	load := catalog.Load()

	var widest int

	for cohort := range racerctrl.Cohorts {
		low, high := -1, 0

		for _, id := range catalog.Column(cohort) {
			if low < 0 || load[id] < low {
				low = load[id]
			}

			if load[id] > high {
				high = load[id]
			}
		}

		if low >= 0 && high-low > widest {
			widest = high - low
		}
	}

	return widest
}

// A zone is grown and shrunk one group at a time, and every configuration it
// passes through on the way has to be one racer would accept and one that keeps
// every group a quorum. This is the whole point of the planner, so it is worth
// asserting against the operator rather than only against the planner.
func TestReconcileMembershipGrowsAndShrinksAZone(t *testing.T) {
	ctx := context.Background()

	zone := newScalingZone(t, ctx, 12)

	seeded := zone.settle()
	if len(seeded) != 12 {
		t.Fatalf("seeded catalog has %d groups, want 12", len(seeded))
	}

	if got := len(zone.members()); got != 3 {
		t.Fatalf("zone seeded with %d nodes, want 3", got)
	}

	for _, width := range []int{4, 7, 4, 3} {
		zone.widen(width)

		catalog := zone.settle()

		if got := len(catalog.Members()); got != width {
			t.Fatalf("zone holds %d nodes, want %d: %v", got, width, catalog)
		}

		if got := zone.balance(catalog); got > 1 {
			t.Fatalf("zone at width %d is out of balance by %d groups: %v", width, got, catalog)
		}

		if got := zone.draining(); len(got) != 0 {
			t.Fatalf("zone at width %d still has %v draining", width, got)
		}

		if err := catalog.Validate(); err != nil {
			t.Fatalf("zone at width %d published an illegal catalog: %v", width, err)
		}
	}
}
