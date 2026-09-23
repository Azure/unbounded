// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStateGCMetadataPagination(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "interrupted"}[fail], func(t *testing.T) {
			f, r, _ := forwardFixture(t)

			ds, err := r.forwardHistory("default")
			if err != nil {
				t.Fatal(err)
			}

			keep := forwardChunkName(stateName("default"), ds[0].Ref, 0)
			api := &gcPagesAPI{Client: f.s.controlStore.client, keep: keep, fail: fail}
			store := stateStore{client: api, namespace: "state"}

			g, pointer, err := store.load(t.Context(), "default")
			if err != nil {
				t.Fatal(err)
			}

			g.Revision++
			if err := store.commit(t.Context(), g, pointer); err != nil {
				t.Fatal("GC failure undid commit", err)
			}

			want := []string{"orphan-1", "orphan-2"}
			if fail {
				want = want[:1]
			}

			if api.invalid || api.pages != 2 || !reflect.DeepEqual(api.deleted, want) {
				t.Fatalf("invalid GC: pages=%d invalid=%t deleted=%v", api.pages, api.invalid, api.deleted)
			}

			committed, _, err := store.load(t.Context(), "default")
			if err != nil || committed.Revision != g.Revision {
				t.Fatal("commit not durable", err)
			}

			if _, err := store.readForwardSnapshot(t.Context(), "default", ds[0]); err != nil {
				t.Fatal("GC removed referenced bytes", err)
			}
		})
	}
}

type gcPagesAPI struct {
	client.Client
	keep    string
	fail    bool
	invalid bool
	pages   int
	deleted []string
}

func (c *gcPagesAPI) List(_ context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	c.pages++
	options := (&client.ListOptions{}).ApplyOptions(opts)

	list, ok := obj.(*metav1.PartialObjectMetadataList)
	if !ok || options.Limit != 100 || options.Namespace != "state" || options.LabelSelector == nil || options.LabelSelector.String() != stateOwnerLabel+"="+stateName("default") || list.GroupVersionKind() != corev1.SchemeGroupVersion.WithKind("ConfigMapList") {
		c.invalid = true
		return errors.New("invalid metadata page request")
	}

	if c.pages == 1 && options.Continue == "" {
		list.Items = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: c.keep}}, {ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "orphan-1"}}}
		list.Continue = "page-2"

		return nil
	}

	if c.pages == 2 && options.Continue == "page-2" {
		if c.fail {
			return errors.New("page unavailable")
		}

		list.Items = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "orphan-2"}}}

		return nil
	}

	c.invalid = true

	return errors.New("unexpected continuation")
}

func (c *gcPagesAPI) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.deleted = append(c.deleted, obj.GetName())
	if _, ok := obj.(*corev1.ConfigMap); !ok || obj.GetNamespace() != "state" {
		c.invalid = true
	}

	return c.Client.Delete(ctx, obj, opts...)
}
