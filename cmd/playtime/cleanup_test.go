// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestDeleteSharedResources(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Namespace = "jordan-testing"
	c := fakePlaytimeClient()

	// Provision every shared object up would create if missing.
	if err := ensureNamespace(ctx, c, cfg); err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}

	if _, err := ensureSharedRBAC(ctx, c, cfg); err != nil {
		t.Fatalf("ensureSharedRBAC: %v", err)
	}

	if err := ensureSite(ctx, c, cfg); err != nil {
		t.Fatalf("ensureSite: %v", err)
	}

	deleted, err := deleteSharedResources(ctx, c, cfg)
	if err != nil {
		t.Fatalf("deleteSharedResources: %v", err)
	}

	if len(deleted) != 6 {
		t.Fatalf("deleted = %v, want 6 shared objects", deleted)
	}

	assertGone := func(name string, obj client.Object, key types.NamespacedName) {
		t.Helper()

		if err := c.Get(ctx, key, obj); err == nil {
			t.Errorf("%s should have been deleted", name)
		} else if !apierrors.IsNotFound(err) {
			t.Errorf("get %s: %v", name, err)
		}
	}

	assertGone("namespace", &corev1.Namespace{}, types.NamespacedName{Name: cfg.Namespace})
	assertGone("service account", &corev1.ServiceAccount{}, types.NamespacedName{Namespace: cfg.Namespace, Name: ReaperServiceAccountName})
	assertGone("cluster role", &rbacv1.ClusterRole{}, types.NamespacedName{Name: cfg.reaperClusterName()})
	assertGone("cluster role binding", &rbacv1.ClusterRoleBinding{}, types.NamespacedName{Name: cfg.reaperClusterName()})
	assertGone("site", &netv1alpha1.Site{}, types.NamespacedName{Name: cfg.NodeSite})
	assertGone("site gateway pool assignment", &netv1alpha1.SiteGatewayPoolAssignment{}, types.NamespacedName{Name: cfg.NodeSite})
}

func TestDeleteSharedResourcesIdempotent(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Namespace = "jordan-testing"
	c := fakePlaytimeClient()

	// Nothing provisioned: deleting missing shared objects is not an error and
	// reports nothing deleted.
	deleted, err := deleteSharedResources(ctx, c, cfg)
	if err != nil {
		t.Fatalf("deleteSharedResources (empty): %v", err)
	}

	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
}

func TestDeleteSharedResourcesLeavesForeignScope(t *testing.T) {
	ctx := context.Background()

	mine := DefaultConfig()
	mine.Namespace = "jordan-testing"

	other := DefaultConfig()
	other.Namespace = "someone-else"
	other.NodeSite = "someone-else-site"

	c := fakePlaytimeClient()

	for _, cfg := range []Config{mine, other} {
		if err := ensureNamespace(ctx, c, cfg); err != nil {
			t.Fatalf("ensureNamespace %q: %v", cfg.Namespace, err)
		}

		if _, err := ensureSharedRBAC(ctx, c, cfg); err != nil {
			t.Fatalf("ensureSharedRBAC %q: %v", cfg.Namespace, err)
		}
	}

	if _, err := deleteSharedResources(ctx, c, mine); err != nil {
		t.Fatalf("deleteSharedResources: %v", err)
	}

	// The other scope's shared RBAC must survive.
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: other.Namespace, Name: ReaperServiceAccountName}, sa); err != nil {
		t.Errorf("foreign scope service account must survive: %v", err)
	}

	role := &rbacv1.ClusterRole{}
	if err := c.Get(ctx, types.NamespacedName{Name: other.reaperClusterName()}, role); err != nil {
		t.Errorf("foreign scope cluster role must survive: %v", err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: other.Namespace}, ns); err != nil {
		t.Errorf("foreign scope namespace must survive: %v", err)
	}
}
