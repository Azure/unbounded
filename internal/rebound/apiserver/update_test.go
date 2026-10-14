// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
)

func TestUpdateAcceptsNoOp(t *testing.T) {
	s, _, ctx := fixture(t)
	g := created(t, s, ctx, "first", "node-a")
	before := cached(t, s).ResourceVersion

	for name, change := range map[string]func(*rebound.NodeGrant){
		"unchanged":              func(*rebound.NodeGrant) {},
		"no resourceVersion":     func(u *rebound.NodeGrant) { u.ResourceVersion = "" },
		"status is ignored":      func(u *rebound.NodeGrant) { u.Status = rebound.NodeGrantStatus{Holder: "mallory"} },
		"server metadata":        func(u *rebound.NodeGrant) { u.UID, u.CreationTimestamp, u.Namespace = "", metav1.Time{}, "" },
		"managed fields ignored": func(u *rebound.NodeGrant) { u.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}} },
	} {
		t.Run(name, func(t *testing.T) {
			updated := g.DeepCopy()
			change(updated)

			var validated bool

			obj, createdNew, err := s.Update(ctx, g.Name, rest.DefaultUpdatedObjectInfo(updated), nil,
				func(_ context.Context, obj, old runtime.Object) error {
					validated = true

					if obj.(*rebound.NodeGrant).UID != g.UID || old.(*rebound.NodeGrant).UID != g.UID {
						t.Error("validation did not receive the stored grant")
					}

					return nil
				}, false, nil)
			if err != nil {
				t.Fatal(err)
			}

			if createdNew || !validated {
				t.Fatalf("created=%v validated=%v", createdNew, validated)
			}

			got, ok := obj.(*rebound.NodeGrant)
			if !ok || got.UID != g.UID || got.ResourceVersion != g.ResourceVersion || got.Status.Holder != "alice" {
				t.Fatalf("unexpected result %#v", obj)
			}
		})
	}

	if after := cached(t, s).ResourceVersion; after != before {
		t.Fatalf("no-op update wrote the ledger: %s -> %s", before, after)
	}
}

func TestUpdateRejectsChanges(t *testing.T) {
	s, _, ctx := fixture(t)
	g := created(t, s, ctx, "first", "node-a")
	before := cached(t, s).ResourceVersion

	for name, tc := range map[string]struct {
		change func(*rebound.NodeGrant)
		check  func(error) bool
		field  string
	}{
		"labels":          {func(u *rebound.NodeGrant) { u.Labels["team"] = "red" }, apierrors.IsInvalid, "metadata.labels"},
		"removed label":   {func(u *rebound.NodeGrant) { u.Labels = nil }, apierrors.IsInvalid, "metadata.labels"},
		"annotations":     {func(u *rebound.NodeGrant) { u.Annotations["new"] = "x" }, apierrors.IsInvalid, "metadata.annotations"},
		"ttl":             {func(u *rebound.NodeGrant) { u.Spec.TTL = &metav1.Duration{Duration: time.Minute} }, apierrors.IsInvalid, "spec"},
		"selector":        {func(u *rebound.NodeGrant) { u.Spec.NodeSelector.MatchLabels = map[string]string{"pool": "a"} }, apierrors.IsInvalid, "spec"},
		"name":            {func(u *rebound.NodeGrant) { u.Name = "second" }, apierrors.IsInvalid, "metadata.name"},
		"namespace":       {func(u *rebound.NodeGrant) { u.Namespace = "other" }, apierrors.IsInvalid, "metadata.namespace"},
		"ownerReferences": {func(u *rebound.NodeGrant) { u.OwnerReferences = []metav1.OwnerReference{{Name: "x"}} }, apierrors.IsInvalid, "metadata.ownerReferences"},
		"finalizers":      {func(u *rebound.NodeGrant) { u.Finalizers = []string{"x"} }, apierrors.IsInvalid, "metadata.finalizers"},
		"stale version":   {func(u *rebound.NodeGrant) { u.ResourceVersion = "1" }, apierrors.IsConflict, ""},
		"other UID":       {func(u *rebound.NodeGrant) { u.UID = "other" }, apierrors.IsConflict, ""},
	} {
		t.Run(name, func(t *testing.T) {
			updated := g.DeepCopy()
			tc.change(updated)

			_, _, err := s.Update(ctx, g.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, nil)
			if !tc.check(err) {
				t.Fatalf("unexpected error %v", err)
			}

			if tc.field == "" {
				return
			}

			var status apierrors.APIStatus
			if !errors.As(err, &status) || status.Status().Details == nil || len(status.Status().Details.Causes) != 1 ||
				status.Status().Details.Causes[0].Field != tc.field {
				t.Fatalf("unexpected causes in %v", err)
			}
		})
	}

	if after := cached(t, s).ResourceVersion; after != before {
		t.Fatalf("rejected update wrote the ledger: %s -> %s", before, after)
	}
}

func TestUpdateErrors(t *testing.T) {
	s, _, ctx := fixture(t)
	g := created(t, s, ctx, "first", "node-a")

	if _, _, err := s.Update(ctx, "missing", rest.DefaultUpdatedObjectInfo(g), nil, nil, true, nil); !apierrors.IsNotFound(err) {
		t.Fatalf("missing grant: %v", err)
	}

	broken := errors.New("broken transformer")

	_, _, err := s.Update(ctx, g.Name, rest.DefaultUpdatedObjectInfo(g, func(context.Context, runtime.Object, runtime.Object) (runtime.Object, error) {
		return nil, broken
	}), nil, nil, false, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("transformer error: %v", err)
	}

	denied := apierrors.NewForbidden(rebound.GroupResource(), g.Name, errors.New("denied"))

	_, _, err = s.Update(ctx, g.Name, rest.DefaultUpdatedObjectInfo(g), nil, func(context.Context, runtime.Object, runtime.Object) error {
		return denied
	}, false, nil)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("validation error: %v", err)
	}
}
