// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantrysnapshotter

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerctrl"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		appsv1.AddToScheme, corev1.AddToScheme, storagev1.AddToScheme, unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

// reconcilerEnv builds an Env whose Apply interceptor records applied objects as
// "Kind/name" keys.
func reconcilerEnv(t *testing.T, objects ...client.Object) (*component.Env, map[string]bool) {
	t.Helper()

	scheme := testScheme(t)
	applied := map[string]bool{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				o, ok := obj.(interface {
					GetName() string
					GetKind() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied[o.GetKind()+"/"+o.GetName()] = true

				return nil
			},
		}).
		Build()

	return &component.Env{
		Client:    cl,
		Scheme:    scheme,
		Namespace: component.DefaultNamespace,
		Config:    component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "test"},
	}, applied
}

func racerClass() *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: storageClassName},
		Provisioner: racerctrl.DriverName,
	}
}

func siteWith(name string, spec *unboundedv1alpha3.GantrySnapshotterComponentSpec) unboundedv1alpha3.Site {
	return unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundedv1alpha3.SiteSpec{
			Components: unboundedv1alpha3.SiteComponents{GantrySnapshotter: spec},
		},
	}
}

func enabledSpec() *unboundedv1alpha3.GantrySnapshotterComponentSpec {
	yes := true

	return &unboundedv1alpha3.GantrySnapshotterComponentSpec{
		SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &yes},
	}
}

func TestEnabledForDefaultsToDisabled(t *testing.T) {
	t.Parallel()

	no := false

	cases := []struct {
		name string
		spec *unboundedv1alpha3.GantrySnapshotterComponentSpec
		want bool
	}{
		{name: "no spec", spec: nil, want: false},
		{name: "spec without enabled", spec: &unboundedv1alpha3.GantrySnapshotterComponentSpec{}, want: false},
		{name: "explicit false", spec: &unboundedv1alpha3.GantrySnapshotterComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &no},
		}, want: false},
		{name: "explicit true", spec: enabledSpec(), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			site := siteWith("a", tc.spec)
			if got := EnabledFor(&site); got != tc.want {
				t.Fatalf("EnabledFor = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestLayoutForDefaults(t *testing.T) {
	t.Parallel()

	got, err := layoutFor(nil)
	if err != nil {
		t.Fatalf("layoutFor(nil): %v", err)
	}

	want := layout{segments: defaultSegments, segmentBytes: defaultSegmentBytes, catalogBytes: defaultCatalogBytes}
	if got != want {
		t.Fatalf("layoutFor(nil) = %+v, want %+v", got, want)
	}
}

func TestLayoutForOverrides(t *testing.T) {
	t.Parallel()

	segments := int32(2)
	segmentSize := resource.MustParse("16Gi")
	catalogSize := resource.MustParse("64Mi")

	spec := enabledSpec()
	spec.Segments = &segments
	spec.SegmentSize = &segmentSize
	spec.CatalogSize = &catalogSize

	site := siteWith("a", spec)

	got, err := layoutFor(&site)
	if err != nil {
		t.Fatalf("layoutFor: %v", err)
	}

	want := layout{segments: 2, segmentBytes: 16 << 30, catalogBytes: 64 << 20}
	if got != want {
		t.Fatalf("layoutFor = %+v, want %+v", got, want)
	}
}

func TestLayoutForRejectsUnalignedSizes(t *testing.T) {
	t.Parallel()

	// A size that is not a whole number of 4 MiB pages cannot become an
	// IMMUTABLE_4M extent, and the racer allocator would reject it silently
	// long after the Site was accepted.
	for _, tc := range []struct {
		name  string
		apply func(*unboundedv1alpha3.GantrySnapshotterComponentSpec)
		want  string
	}{
		{
			name: "segment size",
			apply: func(s *unboundedv1alpha3.GantrySnapshotterComponentSpec) {
				q := resource.MustParse("3Mi")
				s.SegmentSize = &q
			},
			want: "segmentSize",
		},
		{
			name: "catalog size",
			apply: func(s *unboundedv1alpha3.GantrySnapshotterComponentSpec) {
				q := resource.MustParse("1000")
				s.CatalogSize = &q
			},
			want: "catalogSize",
		},
		{
			name: "zero segment size",
			apply: func(s *unboundedv1alpha3.GantrySnapshotterComponentSpec) {
				q := resource.MustParse("0")
				s.SegmentSize = &q
			},
			want: "segmentSize",
		},
		{
			name: "zero segments",
			apply: func(s *unboundedv1alpha3.GantrySnapshotterComponentSpec) {
				n := int32(0)
				s.Segments = &n
			},
			want: "segments",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := enabledSpec()
			tc.apply(spec)
			site := siteWith("a", spec)

			_, err := layoutFor(&site)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("layoutFor = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}

// TestDesiredVolumesGeometryIsParseable is the contract between this component
// and the racer allocator: every volume it creates has to yield exactly one
// extent, because the node agent binds an image volume to a single device and
// the snapshotter addresses it as a single flat range.
func TestDesiredVolumesGeometryIsParseable(t *testing.T) {
	t.Parallel()

	l, err := layoutFor(nil)
	if err != nil {
		t.Fatalf("layoutFor: %v", err)
	}

	volumes := desiredVolumes(l)
	if len(volumes) != int(l.segments)+1 {
		t.Fatalf("desiredVolumes returned %d volumes, want %d", len(volumes), l.segments+1)
	}

	for _, want := range volumes {
		segments, err := racerctrl.ParseGeometry(uint64(want.bytes), want.attributes)
		if err != nil {
			t.Fatalf("ParseGeometry(%s): %v", want.name, err)
		}

		if len(segments) != 1 {
			t.Fatalf("%s has %d extents, want exactly one", want.name, len(segments))
		}

		if segments[0].Bytes() != uint64(want.bytes) {
			t.Fatalf("%s extent is %d bytes, want %d", want.name, segments[0].Bytes(), want.bytes)
		}

		switch want.role {
		case racerctrl.ImageRoleCatalog:
			if segments[0].Kind.String() != "OCC" {
				t.Fatalf("catalog extent kind = %s, want OCC", segments[0].Kind)
			}
		case racerctrl.ImageRoleSegment:
			if segments[0].Kind.String() != "IMMUTABLE_4M" {
				t.Fatalf("segment extent kind = %s, want IMMUTABLE_4M", segments[0].Kind)
			}
		default:
			t.Fatalf("%s has unknown role %q", want.name, want.role)
		}
	}
}

func TestEnsureImageVolumesWaitsForTheStorageClass(t *testing.T) {
	t.Parallel()

	env, _ := reconcilerEnv(t)

	l, _ := layoutFor(nil)

	pending, err := ensureImageVolumes(t.Context(), env, l)
	if err != nil {
		t.Fatalf("ensureImageVolumes: %v", err)
	}

	if !strings.Contains(pending, storageClassName) {
		t.Fatalf("pending = %q, want it to name the missing StorageClass", pending)
	}

	// Nothing may be created before the universe it would be allocated from
	// exists.
	var volumes corev1.PersistentVolumeList
	if err := env.Client.List(t.Context(), &volumes); err != nil {
		t.Fatalf("list volumes: %v", err)
	}

	if len(volumes.Items) != 0 {
		t.Fatalf("created %d volumes without a StorageClass", len(volumes.Items))
	}
}

func TestEnsureImageVolumesCreatesTheImageVolume(t *testing.T) {
	t.Parallel()

	env, _ := reconcilerEnv(t, racerClass())

	l, _ := layoutFor(nil)

	pending, err := ensureImageVolumes(t.Context(), env, l)
	if err != nil {
		t.Fatalf("ensureImageVolumes: %v", err)
	}

	if pending == "" {
		t.Fatal("freshly created volumes reported as ready")
	}

	var volumes corev1.PersistentVolumeList
	if err := env.Client.List(t.Context(), &volumes); err != nil {
		t.Fatalf("list volumes: %v", err)
	}

	if len(volumes.Items) != int(l.segments)+1 {
		t.Fatalf("created %d volumes, want %d", len(volumes.Items), l.segments+1)
	}

	roles := map[string]int{}

	for i := range volumes.Items {
		pv := &volumes.Items[i]

		roles[pv.Annotations[racerctrl.ImageRoleAnnotation]]++

		if pv.Spec.StorageClassName != storageClassName {
			t.Fatalf("%s storage class = %q", pv.Name, pv.Spec.StorageClassName)
		}

		if pv.Spec.VolumeMode == nil || *pv.Spec.VolumeMode != corev1.PersistentVolumeBlock {
			t.Fatalf("%s volume mode = %v, want Block", pv.Name, pv.Spec.VolumeMode)
		}

		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
			t.Fatalf("%s reclaim policy = %v, want Retain", pv.Name, pv.Spec.PersistentVolumeReclaimPolicy)
		}

		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != racerctrl.DriverName {
			t.Fatalf("%s is not a racer volume: %+v", pv.Name, pv.Spec.CSI)
		}

		// Without a claimRef the binder would be free to hand the cluster's
		// layer store to the next PVC that asks for this class.
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != pv.Name {
			t.Fatalf("%s is not reserved: %+v", pv.Name, pv.Spec.ClaimRef)
		}
	}

	if roles[racerctrl.ImageRoleCatalog] != 1 || roles[racerctrl.ImageRoleSegment] != int(l.segments) {
		t.Fatalf("roles = %#v", roles)
	}
}

func TestEnsureImageVolumesIsReadyOncePlaced(t *testing.T) {
	t.Parallel()

	l, _ := layoutFor(nil)

	objects := []client.Object{racerClass()}

	for _, want := range desiredVolumes(l) {
		pv := buildVolume(&component.Env{Namespace: component.DefaultNamespace}, want)
		pv.Annotations[racerctrl.CompositionAnnotation] = "0?baseLba=0&extent=1&kind=OCC&pages=1"
		objects = append(objects, pv)
	}

	env, _ := reconcilerEnv(t, objects...)

	pending, err := ensureImageVolumes(t.Context(), env, l)
	if err != nil {
		t.Fatalf("ensureImageVolumes: %v", err)
	}

	if pending != "" {
		t.Fatalf("pending = %q, want ready", pending)
	}
}

func TestEnsureImageVolumesReportsResizeRatherThanAttemptingIt(t *testing.T) {
	t.Parallel()

	l, _ := layoutFor(nil)

	// An existing volume half the requested size. Its extents are already
	// allocated, so the only honest outcome is to say so.
	small := desiredVolumes(l)[1]
	small.bytes = defaultSegmentBytes / 2
	pv := buildVolume(&component.Env{Namespace: component.DefaultNamespace}, small)
	pv.Annotations[racerctrl.CompositionAnnotation] = "0?baseLba=0&extent=2&kind=IMMUTABLE_4M&pages=1024"

	env, _ := reconcilerEnv(t, racerClass(), pv)

	pending, err := ensureImageVolumes(t.Context(), env, l)
	if err != nil {
		t.Fatalf("ensureImageVolumes: %v", err)
	}

	if !strings.Contains(pending, "cannot be resized") || !strings.Contains(pending, small.name) {
		t.Fatalf("pending = %q, want a resize report naming %s", pending, small.name)
	}

	var got corev1.PersistentVolume
	if err := env.Client.Get(t.Context(), client.ObjectKey{Name: small.name}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}

	if capacity := got.Spec.Capacity[corev1.ResourceStorage]; capacity.Value() != small.bytes {
		t.Fatalf("existing volume was resized to %s", capacity.String())
	}
}

func TestReconcileDisabledWithNothingInstalled(t *testing.T) {
	t.Parallel()

	env, applied := reconcilerEnv(t, racerClass())

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{siteWith("edge", nil)})
	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("Reconcile = %+v, want ready with Disabled", res)
	}

	if len(applied) != 0 {
		t.Fatalf("disabled reconcile applied %#v", applied)
	}

	// It must also not have created the image volumes, which would allocate
	// extents nothing is going to read.
	var volumes corev1.PersistentVolumeList
	if err := env.Client.List(t.Context(), &volumes); err != nil {
		t.Fatalf("list volumes: %v", err)
	}

	if len(volumes.Items) != 0 {
		t.Fatalf("disabled reconcile created %d volumes", len(volumes.Items))
	}
}

func TestReconcileRetainsAnExistingInstall(t *testing.T) {
	t.Parallel()

	// Uninstalling would leave nodes whose containerd points at a snapshotter
	// that is no longer scheduled, so an existing install is kept reconciled.
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName},
	}
	env, applied := reconcilerEnv(t, racerClass(), existing)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{siteWith("edge", nil)})
	if res.Err != nil {
		t.Fatalf("Reconcile = %+v", res)
	}

	if !applied["DaemonSet/"+daemonSetName] {
		t.Fatalf("retained install was not reconciled; applied=%#v", applied)
	}
}

func TestReconcileAppliesManifestsAndVolumes(t *testing.T) {
	t.Parallel()

	env, applied := reconcilerEnv(t, racerClass())

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{siteWith("edge", enabledSpec())})
	if res.Err != nil {
		t.Fatalf("Reconcile = %+v", res)
	}

	// Not ready yet: the volumes were only just created and the racer
	// allocator has not placed them.
	if res.Ready || res.Reason != "ImageVolumesPending" {
		t.Fatalf("Reconcile = %+v, want ImageVolumesPending", res)
	}

	for _, want := range []string{
		"ServiceAccount/gantry-snapshotter",
		"RuntimeClass/gantry-bootstrap",
		"DaemonSet/" + daemonSetName,
		"DaemonSet/" + nodeConfigDaemonSetName,
		"PriorityClass/gantry-snapshotter-node-critical",
	} {
		if !applied[want] {
			t.Fatalf("expected %s to be applied; applied=%#v", want, applied)
		}
	}

	var volumes corev1.PersistentVolumeList
	if err := env.Client.List(t.Context(), &volumes); err != nil {
		t.Fatalf("list volumes: %v", err)
	}

	if len(volumes.Items) != defaultSegments+1 {
		t.Fatalf("created %d volumes, want %d", len(volumes.Items), defaultSegments+1)
	}
}

func TestApplyMutatorStampsBothDaemonSets(t *testing.T) {
	t.Parallel()

	const image = "ghcr.io/azure/gantry-snapshotter:v1"

	for _, tc := range []struct{ ds, container string }{
		{ds: daemonSetName, container: containerName},
		{ds: nodeConfigDaemonSetName, container: nodeConfigCtrName},
	} {
		t.Run(tc.ds, func(t *testing.T) {
			t.Parallel()

			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "DaemonSet",
				"metadata":   map[string]any{"name": tc.ds},
				"spec": map[string]any{"template": map[string]any{
					"metadata": map[string]any{},
					"spec": map[string]any{"containers": []any{
						map[string]any{"name": tc.container, "image": "placeholder"},
					}},
				}},
			}}

			if err := applyMutator(image)(obj); err != nil {
				t.Fatalf("applyMutator: %v", err)
			}

			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")

			got, _ := containers[0].(map[string]any)
			if got["image"] != image {
				t.Fatalf("image = %v, want %s", got["image"], image)
			}
		})
	}
}

func TestResourcesExist(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		existing client.Object
		want     bool
	}{
		{name: "empty", want: false},
		{name: "agent", want: true, existing: &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName},
		}},
		{name: "node config", want: true, existing: &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: nodeConfigDaemonSetName},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var objects []client.Object
			if tc.existing != nil {
				objects = append(objects, tc.existing)
			}

			env, _ := reconcilerEnv(t, objects...)

			got, err := resourcesExist(t.Context(), env)
			if err != nil || got != tc.want {
				t.Fatalf("resourcesExist = %t, %v; want %t", got, err, tc.want)
			}
		})
	}
}
