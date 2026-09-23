// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kubetesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func withTypedConfigMapLists(builder *fake.ClientBuilder, scheme *runtime.Scheme) *fake.ClientBuilder {
	// As in coordinationKube, all reads and writes share one authoritative tracker.
	// The fake's versioned wrapper still handles writes, status updates, and CAS.
	tracker := kubetesting.NewObjectTracker(scheme, serializer.NewCodecFactory(scheme).UniversalDecoder())

	return builder.WithScheme(scheme).WithObjectTracker(tracker).
		WithInterceptorFuncs(typedConfigMapLists(tracker))
}

func typedConfigMapLists(tracker kubetesting.ObjectTracker) interceptor.Funcs {
	return interceptor.Funcs{List: func(ctx context.Context, underlying client.WithWatch, obj client.ObjectList, opts ...client.ListOption) error {
		if list, ok := obj.(*metav1.PartialObjectMetadataList); ok && list.GroupVersionKind() == corev1.SchemeGroupVersion.WithKind("ConfigMapList") {
			options := (&client.ListOptions{}).ApplyOptions(opts)
			if options.FieldSelector == nil && options.Raw == nil && options.UnsafeDisableDeepCopy == nil && !options.DisableReadYourWritesConsistency {
				return listConfigMapMetadata(tracker, list, options)
			}
		}

		list, ok := obj.(*corev1.ConfigMapList)
		if !ok || !list.GroupVersionKind().Empty() {
			return underlying.List(ctx, obj, opts...)
		}

		options := (&client.ListOptions{}).ApplyOptions(opts)
		if options.FieldSelector != nil || options.Raw != nil || options.Limit != 0 || options.Continue != "" || options.UnsafeDisableDeepCopy != nil || options.DisableReadYourWritesConsistency {
			// Field selectors require the fake's registered indexes. Delegate options
			// outside the namespace/label fast path instead of approximating them.
			return underlying.List(ctx, obj, opts...)
		}

		gvk := corev1.SchemeGroupVersion.WithKind("ConfigMap")

		stored, err := tracker.List(corev1.SchemeGroupVersion.WithResource("configmaps"), gvk, options.Namespace)
		if err != nil {
			return err
		}

		// Tracker.List already deep-copies the typed list. Avoid the pinned fake's
		// additional JSON/base64 round-trip over every chunk before label filtering.
		// Keep ListMeta as well as complete payloads; this is not a metadata cache.
		result := stored.(*corev1.ConfigMapList)

		items := make([]corev1.ConfigMap, 0, len(result.Items))
		for _, item := range result.Items {
			if options.LabelSelector != nil && !options.LabelSelector.Matches(labels.Set(item.Labels)) {
				continue
			}

			// Match the default fake's typed-object and managed-field projection.
			item.TypeMeta = metav1.TypeMeta{}
			item.ManagedFields = nil
			items = append(items, item)
		}

		*list = *result
		list.TypeMeta = metav1.TypeMeta{}
		list.Items = items

		return nil
	}}
}

func listConfigMapMetadata(tracker kubetesting.ObjectTracker, list *metav1.PartialObjectMetadataList, options *client.ListOptions) error {
	gvk := corev1.SchemeGroupVersion.WithKind("ConfigMap")

	stored, err := tracker.List(corev1.SchemeGroupVersion.WithResource("configmaps"), gvk, options.Namespace)
	if err != nil {
		return err
	}

	// Match the pinned fake: Limit and Continue are ignored, ListMeta is reset,
	// and item GVK is explicit. Real GC pagination is tested by gcPagesAPI.
	// Tracker.List owns these copies; project metadata without JSON/base64 of
	// chunk payloads, and without keeping a second store that can miss writes.
	result := stored.(*corev1.ConfigMapList)

	items := make([]metav1.PartialObjectMetadata, 0, len(result.Items))
	for _, item := range result.Items {
		if options.LabelSelector != nil && !options.LabelSelector.Matches(labels.Set(item.Labels)) {
			continue
		}

		item.ManagedFields = nil
		items = append(items, metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: item.ObjectMeta})
	}

	*list = metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}, Items: items}

	return nil
}

func TestTypedConfigMapListEquivalence(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	immutable := true
	objects := []client.Object{
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{
			Namespace: "state", Name: "chunk", UID: "chunk-uid", ResourceVersion: "12", Generation: 3,
			Labels: map[string]string{stateOwnerLabel: "owner", "tier": "hot"}, Annotations: map[string]string{"note": "retained"},
			Finalizers: []string{"hold"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "pointer", UID: "pointer-uid"}},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "fixture", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{"f:manifest":{}}}`)}}},
		}, Immutable: &immutable, Data: map[string]string{"manifest": "payload"}, BinaryData: map[string][]byte{"state": {0, 1, 255}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "other-owner", Labels: map[string]string{stateOwnerLabel: "other", "tier": "cold"}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "chunk", Labels: map[string]string{stateOwnerLabel: "owner"}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "unlabeled"}},
	}
	stock := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	fast := withTypedConfigMapLists(fake.NewClientBuilder(), scheme).WithObjects(objects...).Build()

	selector, err := labels.Parse("tier in (hot,cold)," + stateOwnerLabel + "!=other")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		opts []client.ListOption
	}{
		{name: "all-namespaces"},
		{name: "namespace", opts: []client.ListOption{client.InNamespace("state")}},
		{name: "gc-owner", opts: []client.ListOption{client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}}},
		{name: "gc-page", opts: []client.ListOption{client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}, client.Limit(100), client.Continue("")}},
		{name: "set-selector", opts: []client.ListOption{client.MatchingLabelsSelector{Selector: selector}}},
		{name: "no-label-match", opts: []client.ListOption{client.MatchingLabels{"missing": "value"}}},
		{name: "no-namespace-match", opts: []client.ListOption{client.InNamespace("missing")}},
		{name: "field-index-error", opts: []client.ListOption{client.MatchingFields{"metadata.name": "chunk"}}},
		{name: "unsupported-field-selector", opts: []client.ListOption{client.MatchingFieldsSelector{Selector: fields.OneTermNotEqualSelector("metadata.name", "chunk")}}},
		{name: "raw-options", opts: []client.ListOption{&client.ListOptions{Raw: &metav1.ListOptions{LabelSelector: "tier=hot"}}}},
		{name: "pagination", opts: []client.ListOption{client.Limit(1), client.Continue("next")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := &corev1.ConfigMapList{}
			got := &corev1.ConfigMapList{ListMeta: metav1.ListMeta{Continue: "stale"}, Items: []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "stale"}}}}
			wantErr := stock.List(t.Context(), want, tc.opts...)

			gotErr := fast.List(t.Context(), got, tc.opts...)
			if (wantErr == nil) != (gotErr == nil) || wantErr != nil && wantErr.Error() != gotErr.Error() {
				t.Fatalf("errors differ: stock=%v typed=%v", wantErr, gotErr)
			}

			if wantErr != nil {
				return
			}

			if !reflect.DeepEqual(want.Items, got.Items) || want.TypeMeta != got.TypeMeta {
				t.Fatalf("lists differ: stock=%+v typed=%+v", want, got)
			}

			if got.Continue != "" {
				t.Fatal("retained stale list metadata")
			}
		})
		t.Run("metadata/"+tc.name, func(t *testing.T) {
			want := configMapMetadataList()
			got := configMapMetadataList()
			remaining := int64(12)
			got.ListMeta = metav1.ListMeta{ResourceVersion: "stale", Continue: "stale", RemainingItemCount: &remaining}
			got.Items = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{Name: "stale"}}}
			wantErr := stock.List(t.Context(), want, tc.opts...)

			gotErr := fast.List(t.Context(), got, tc.opts...)
			if (wantErr == nil) != (gotErr == nil) || wantErr != nil && wantErr.Error() != gotErr.Error() {
				t.Fatalf("errors differ: stock=%v metadata=%v", wantErr, gotErr)
			}

			if wantErr == nil && !reflect.DeepEqual(want, got) {
				t.Fatalf("metadata lists differ: stock=%+v fast=%+v", want, got)
			}
		})
	}

	var listed corev1.ConfigMapList
	if err := fast.List(t.Context(), &listed, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}); err != nil {
		t.Fatal(err)
	}

	listed.Items[0].BinaryData["state"][0] = 99
	listed.Items[0].Data["manifest"] = "mutated"
	listed.Items[0].Labels[stateOwnerLabel] = "mutated"
	*listed.Items[0].Immutable = false
	listed.Items[0].OwnerReferences[0].Name = "mutated"

	var again corev1.ConfigMapList
	if err := fast.List(t.Context(), &again, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}); err != nil {
		t.Fatal(err)
	}

	var want corev1.ConfigMapList
	if err := stock.List(t.Context(), &want, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(again.Items, want.Items) {
		t.Fatal("list mutation changed authoritative objects")
	}

	updated := again.Items[0].DeepCopy()

	updated.Data["manifest"] = "committed"
	if err := fast.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}

	if err := fast.Update(t.Context(), &again.Items[0]); !apierrors.IsConflict(err) {
		t.Fatalf("stale list item bypassed CAS: %v", err)
	}

	if err := fast.List(t.Context(), &again, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}); err != nil {
		t.Fatal(err)
	}

	if len(again.Items) != 1 || again.Items[0].Data["manifest"] != "committed" {
		t.Fatal("list did not observe update")
	}

	updated.Finalizers = nil
	if err := fast.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}

	if err := fast.Delete(t.Context(), updated); err != nil {
		t.Fatal(err)
	}

	if err := fast.List(t.Context(), &again, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}); err != nil || len(again.Items) != 0 {
		t.Fatalf("list did not observe deletion: %v", err)
	}
}

func configMapMetadataList() *metav1.PartialObjectMetadataList {
	return &metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}}
}

func TestConfigMapMetadataListMutationAndWrites(t *testing.T) {
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "chunk", Labels: map[string]string{"owner": "first"}, Annotations: map[string]string{"note": "original"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "pointer", UID: "pointer-uid"}}}, BinaryData: map[string][]byte{"state": {1, 2, 3}}}
	kube := fakeKube(object)
	list := configMapMetadataList()

	options := []client.ListOption{client.InNamespace("state"), client.MatchingLabels{"owner": "first"}, client.Limit(100), client.Continue("")}
	if err := kube.List(t.Context(), list, options...); err != nil || len(list.Items) != 1 {
		t.Fatalf("initial list: %+v %v", list, err)
	}

	original := list.DeepCopy()
	list.Items[0].Labels["owner"] = "mutated"
	list.Items[0].Annotations["note"] = "mutated"

	list.Items[0].OwnerReferences[0].Name = "mutated"
	if err := kube.List(t.Context(), list, options...); err != nil || !reflect.DeepEqual(original, list) {
		t.Fatalf("list mutation changed tracker: %+v %v", list, err)
	}

	updated := &corev1.ConfigMap{}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(object), updated); err != nil {
		t.Fatal(err)
	}

	updated.Labels["owner"] = "second"
	if err := kube.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}

	if err := kube.List(t.Context(), list, options...); err != nil || len(list.Items) != 0 {
		t.Fatalf("list retained old labels after update: %+v %v", list, err)
	}

	if err := kube.List(t.Context(), list, client.InNamespace("state")); err != nil || len(list.Items) != 1 || list.Items[0].ResourceVersion != updated.ResourceVersion {
		t.Fatalf("list did not observe update: %+v %v", list, err)
	}

	stale := &corev1.ConfigMap{ObjectMeta: original.Items[0].ObjectMeta}
	if err := kube.Update(t.Context(), stale); !apierrors.IsConflict(err) {
		t.Fatalf("metadata bypassed CAS: %v", err)
	}

	if err := kube.Delete(t.Context(), &corev1.ConfigMap{ObjectMeta: list.Items[0].ObjectMeta}); err != nil {
		t.Fatal(err)
	}

	if err := kube.List(t.Context(), list, client.InNamespace("state")); err != nil || len(list.Items) != 0 {
		t.Fatalf("metadata-based delete not visible: %+v %v", list, err)
	}
}

type configMapListTracker struct {
	kubetesting.ObjectTracker
	list *corev1.ConfigMapList
	err  error
}

func (t configMapListTracker) List(schema.GroupVersionResource, schema.GroupVersionKind, string, ...metav1.ListOptions) (runtime.Object, error) {
	return t.list.DeepCopy(), t.err
}

func TestTypedConfigMapListMetadataAndFailure(t *testing.T) {
	remaining := int64(2)
	tracker := configMapListTracker{list: &corev1.ConfigMapList{ListMeta: metav1.ListMeta{ResourceVersion: "17", Continue: "next", RemainingItemCount: &remaining}}}

	var got corev1.ConfigMapList
	if err := typedConfigMapLists(tracker).List(t.Context(), nil, &got); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got.ListMeta, tracker.list.ListMeta) {
		t.Fatalf("lost tracker list metadata: %+v", got.ListMeta)
	}

	*got.RemainingItemCount = 99
	if *tracker.list.RemainingItemCount != 2 {
		t.Fatal("list metadata aliases tracker")
	}

	metadata := configMapMetadataList()
	if err := typedConfigMapLists(tracker).List(t.Context(), nil, metadata, client.Limit(100), client.Continue("next")); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(metadata.ListMeta, metav1.ListMeta{}) {
		t.Fatalf("metadata projection retained tracker pagination unlike pinned fake: %+v", metadata.ListMeta)
	}

	tracker.err = errors.New("tracker unavailable")
	if err := typedConfigMapLists(tracker).List(t.Context(), nil, &got); !errors.Is(err, tracker.err) {
		t.Fatalf("lost tracker error: %v", err)
	}

	if err := typedConfigMapLists(tracker).List(t.Context(), nil, metadata); !errors.Is(err, tracker.err) {
		t.Fatalf("lost metadata tracker error: %v", err)
	}
}

func TestTypedConfigMapListFallback(t *testing.T) {
	sentinel := errors.New("underlying list")
	underlying := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return sentinel
	}})

	disable := true
	for _, tc := range []struct {
		name string
		list client.ObjectList
		opts []client.ListOption
	}{
		{name: "other-kind", list: &corev1.PodList{}},
		{name: "explicit-gvk", list: &corev1.ConfigMapList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"}}},
		{name: "field-selector", opts: []client.ListOption{client.MatchingFields{"metadata.name": "chunk"}}},
		{name: "raw", opts: []client.ListOption{&client.ListOptions{Raw: &metav1.ListOptions{}}}},
		{name: "limit", opts: []client.ListOption{client.Limit(1)}},
		{name: "continue", opts: []client.ListOption{client.Continue("next")}},
		{name: "unsafe-deep-copy", opts: []client.ListOption{&client.ListOptions{UnsafeDisableDeepCopy: &disable}}},
		{name: "read-consistency", opts: []client.ListOption{&client.ListOptions{DisableReadYourWritesConsistency: true}}},
		{name: "metadata-no-gvk", list: &metav1.PartialObjectMetadataList{}},
		{name: "metadata-other-gvk", list: &metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}}},
		{name: "metadata-other-version", list: &metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "other/v1", Kind: "ConfigMapList"}}},
		{name: "metadata-field-selector", list: configMapMetadataList(), opts: []client.ListOption{client.MatchingFields{"metadata.name": "chunk"}}},
		{name: "metadata-raw", list: configMapMetadataList(), opts: []client.ListOption{&client.ListOptions{Raw: &metav1.ListOptions{}}}},
		{name: "metadata-unsafe-deep-copy", list: configMapMetadataList(), opts: []client.ListOption{&client.ListOptions{UnsafeDisableDeepCopy: &disable}}},
		{name: "metadata-read-consistency", list: configMapMetadataList(), opts: []client.ListOption{&client.ListOptions{DisableReadYourWritesConsistency: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.list == nil {
				tc.list = &corev1.ConfigMapList{}
			}

			if err := typedConfigMapLists(nil).List(t.Context(), underlying, tc.list, tc.opts...); !errors.Is(err, sentinel) {
				t.Fatalf("did not delegate: %v", err)
			}
		})
	}
}

func TestTypedConfigMapListStateGC(t *testing.T) {
	ctx := t.Context()
	owner := stateName("default")
	orphan := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "orphan", Labels: map[string]string{stateOwnerLabel: owner}}, BinaryData: map[string][]byte{"state": []byte("interrupted write")}}
	otherOwner := orphan.DeepCopy()
	otherOwner.Name = "other-owner"
	otherOwner.Labels[stateOwnerLabel] = stateName("other")
	otherNamespace := orphan.DeepCopy()
	otherNamespace.Namespace = "elsewhere"
	kube := fakeKube(orphan, otherOwner, otherNamespace)
	store := stateStore{client: kube, namespace: "state"}
	g := &generation{Format: generationFormat, Universe: "default"}

	var pointer *corev1.ConfigMap

	for revision := uint64(1); revision <= 3; revision++ {
		g.Revision = revision
		if err := store.commit(ctx, g, pointer); err != nil {
			t.Fatal(err)
		}

		loaded, next, err := store.load(ctx, "default")
		if err != nil || loaded.Revision != revision {
			t.Fatalf("committed state did not reload: %+v %v", loaded, err)
		}

		pointer = next
	}

	var chunks corev1.ConfigMapList
	if err := kube.List(ctx, &chunks, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: owner}); err != nil {
		t.Fatal(err)
	}

	if len(chunks.Items) != 2 {
		t.Fatalf("GC must retain current and previous chunks, got %d", len(chunks.Items))
	}

	if err := kube.Get(ctx, client.ObjectKeyFromObject(orphan), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan was not collected: %v", err)
	}

	for _, retained := range []*corev1.ConfigMap{otherOwner, otherNamespace} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(retained), &corev1.ConfigMap{}); err != nil {
			t.Fatalf("GC crossed owner or namespace boundary: %v", err)
		}
	}
}

func BenchmarkConfigMapChunkList(b *testing.B) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}

	objects := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "owned", Labels: map[string]string{stateOwnerLabel: "owner"}}, BinaryData: map[string][]byte{"state": bytes.Repeat([]byte{1}, 6*1024*1024)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "unrelated"}, BinaryData: map[string][]byte{"state": bytes.Repeat([]byte{2}, 6*1024*1024)}},
	}

	for _, name := range []string{"stock", "typed", "stock-metadata", "typed-metadata"} {
		b.Run(name, func(b *testing.B) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if name == "typed" || name == "typed-metadata" {
				builder = withTypedConfigMapLists(builder, scheme)
			}

			kube := builder.WithObjects(objects...).Build()

			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				if name == "stock-metadata" || name == "typed-metadata" {
					list := configMapMetadataList()
					if err := kube.List(b.Context(), list, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}, client.Limit(100), client.Continue("")); err != nil {
						b.Fatal(err)
					}

					if len(list.Items) != 1 || list.Items[0].Name != "owned" || list.Continue != "" {
						b.Fatal("incorrect metadata projection")
					}

					continue
				}

				var list corev1.ConfigMapList
				if err := kube.List(b.Context(), &list, client.InNamespace("state"), client.MatchingLabels{stateOwnerLabel: "owner"}); err != nil {
					b.Fatal(err)
				}

				if len(list.Items) != 1 || len(list.Items[0].BinaryData["state"]) != 6*1024*1024 {
					b.Fatal("lost selected payload")
				}
			}
		})
	}
}
