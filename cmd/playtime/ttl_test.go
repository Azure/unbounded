// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestExpiryString(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TTL = 90 * time.Minute
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	got := cfg.expiryString(now)

	want := "2026-07-14T13:30:00Z"
	if got != want {
		t.Fatalf("expiryString = %q, want %q", got, want)
	}

	parsed, err := parseExpiry(got)
	if err != nil {
		t.Fatalf("parseExpiry(%q): %v", got, err)
	}

	if !parsed.Equal(now.Add(cfg.TTL)) {
		t.Fatalf("round-trip expiry = %v, want %v", parsed, now.Add(cfg.TTL))
	}
}

func TestParseExpiryInvalid(t *testing.T) {
	if _, err := parseExpiry("not-a-timestamp"); err == nil {
		t.Fatal("expected error parsing invalid expiry")
	}
}

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		expiresAt string
		want      bool
	}{
		{"past is expired", now.Add(-time.Minute).UTC().Format(time.RFC3339), true},
		{"future is not expired", now.Add(time.Minute).UTC().Format(time.RFC3339), false},
		{"empty is not expired", "", false},
		{"whitespace is not expired", "   ", false},
		{"malformed is not expired", "garbage", false},
		{"exact now is not expired", now.UTC().Format(time.RFC3339), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExpired(tc.expiresAt, now); got != tc.want {
				t.Fatalf("isExpired(%q) = %v, want %v", tc.expiresAt, got, tc.want)
			}
		})
	}
}

func TestNodeOwnerRef(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "jordan-playtime-abc12",
			UID:  types.UID("uid-1234"),
		},
	}

	ref := nodeOwnerRef(node)

	if ref.APIVersion != "v1" || ref.Kind != "Node" {
		t.Fatalf("owner ref kind = %s/%s, want v1/Node", ref.APIVersion, ref.Kind)
	}

	if ref.Name != node.Name || ref.UID != node.UID {
		t.Fatalf("owner ref = %s/%s, want %s/%s", ref.Name, ref.UID, node.Name, node.UID)
	}
}

func TestWriteReadLastRun(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StateDir = filepath.Join(t.TempDir(), "state")

	got, err := readLastRun(cfg)
	if err != nil {
		t.Fatalf("readLastRun (missing): %v", err)
	}

	if got != "" {
		t.Fatalf("readLastRun (missing) = %q, want empty", got)
	}

	if err := writeLastRun(cfg, "jordan-playtime-abc12"); err != nil {
		t.Fatalf("writeLastRun: %v", err)
	}

	got, err = readLastRun(cfg)
	if err != nil {
		t.Fatalf("readLastRun: %v", err)
	}

	if want := "jordan-playtime-abc12"; got != want {
		t.Fatalf("readLastRun = %q, want %q", got, want)
	}
}

func fakePlaytimeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(playtimeScheme()).
		WithObjects(objs...).
		Build()
}

func TestEnsureSite(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	c := fakePlaytimeClient()

	// First call creates the site and its gateway pool assignment; a second
	// call is a no-op (idempotent, shared across runs).
	for i := 0; i < 2; i++ {
		if err := ensureSite(ctx, c, cfg); err != nil {
			t.Fatalf("ensureSite call %d: %v", i, err)
		}
	}

	site := &netv1alpha1.Site{}
	if err := c.Get(ctx, types.NamespacedName{Name: cfg.NodeSite}, site); err != nil {
		t.Fatalf("get site: %v", err)
	}

	if got := site.Spec.NodeCidrs; len(got) != 1 || got[0] != cfg.SiteNodeCIDR {
		t.Fatalf("site NodeCidrs = %v, want [%s]", got, cfg.SiteNodeCIDR)
	}

	if len(site.Spec.PodCidrAssignments) != 1 ||
		len(site.Spec.PodCidrAssignments[0].CidrBlocks) != 1 ||
		site.Spec.PodCidrAssignments[0].CidrBlocks[0] != cfg.SitePodCIDR {
		t.Fatalf("site PodCidrAssignments = %v, want pod cidr %s", site.Spec.PodCidrAssignments, cfg.SitePodCIDR)
	}

	assignment := &netv1alpha1.SiteGatewayPoolAssignment{}
	if err := c.Get(ctx, types.NamespacedName{Name: cfg.NodeSite}, assignment); err != nil {
		t.Fatalf("get site gateway pool assignment: %v", err)
	}

	if got := assignment.Spec.Sites; len(got) != 1 || got[0] != cfg.NodeSite {
		t.Fatalf("assignment Sites = %v, want [%s]", got, cfg.NodeSite)
	}

	if got := assignment.Spec.GatewayPools; len(got) != len(cfg.GatewayPools) || got[0] != cfg.GatewayPools[0] {
		t.Fatalf("assignment GatewayPools = %v, want %v", got, cfg.GatewayPools)
	}
}

func playtimeNode(name, namespace, expiresAt string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				TempNodeLabelKey: namespace,
			},
			Annotations: map[string]string{
				ExpiresAtAnnotation: expiresAt,
			},
		},
	}
}

func nodeExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()

	err := c.Get(context.Background(), types.NamespacedName{Name: name}, &corev1.Node{})
	if err == nil {
		return true
	}

	if apierrors.IsNotFound(err) {
		return false
	}

	t.Fatalf("get node %q: %v", name, err)

	return false
}

func TestListRunsScopedByNamespace(t *testing.T) {
	now := time.Now()
	mine := playtimeNode("mine", "jordan-testing", now.Format(time.RFC3339))
	other := playtimeNode("other", "someone-else", now.Format(time.RFC3339))
	c := fakePlaytimeClient(mine, other)

	runs, err := listRuns(context.Background(), c, "jordan-testing")
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].Name != "mine" {
		t.Fatalf("listRuns = %+v, want only 'mine'", runs)
	}
}

func TestReapExpired(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	future := now.Add(time.Hour).UTC().Format(time.RFC3339)

	expired := playtimeNode("expired", "jordan-testing", past)
	fresh := playtimeNode("fresh", "jordan-testing", future)
	noExpiry := playtimeNode("no-expiry", "jordan-testing", "")
	foreign := playtimeNode("foreign", "someone-else", past)
	c := fakePlaytimeClient(expired, fresh, noExpiry, foreign)

	reaped, err := reapExpired(context.Background(), c, "jordan-testing", now)
	if err != nil {
		t.Fatalf("reapExpired: %v", err)
	}

	if len(reaped) != 1 || reaped[0] != "expired" {
		t.Fatalf("reaped = %v, want [expired]", reaped)
	}

	if nodeExists(t, c, "expired") {
		t.Error("expired node should have been deleted")
	}

	if !nodeExists(t, c, "fresh") {
		t.Error("fresh node should survive")
	}

	if !nodeExists(t, c, "no-expiry") {
		t.Error("node without expiry should survive")
	}

	if !nodeExists(t, c, "foreign") {
		t.Error("foreign-namespace node must not be touched")
	}
}

func TestReapExpiredNoneExpired(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).UTC().Format(time.RFC3339)
	c := fakePlaytimeClient(playtimeNode("fresh", "jordan-testing", future))

	reaped, err := reapExpired(context.Background(), c, "jordan-testing", now)
	if err != nil {
		t.Fatalf("reapExpired: %v", err)
	}

	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none", reaped)
	}
}

func TestDeleteNode(t *testing.T) {
	now := time.Now()
	c := fakePlaytimeClient(playtimeNode("mine", "jordan-testing", now.Format(time.RFC3339)))

	if err := deleteNode(context.Background(), c, "mine"); err != nil {
		t.Fatalf("deleteNode: %v", err)
	}

	if nodeExists(t, c, "mine") {
		t.Error("node should have been deleted")
	}

	// Deleting a missing node is not an error.
	if err := deleteNode(context.Background(), c, "mine"); err != nil {
		t.Fatalf("deleteNode (missing): %v", err)
	}
}

func TestDeleteAllRuns(t *testing.T) {
	now := time.Now().Format(time.RFC3339)
	a := playtimeNode("a", "jordan-testing", now)
	b := playtimeNode("b", "jordan-testing", now)
	foreign := playtimeNode("foreign", "someone-else", now)
	c := fakePlaytimeClient(a, b, foreign)

	deleted, err := deleteAllRuns(context.Background(), c, "jordan-testing")
	if err != nil {
		t.Fatalf("deleteAllRuns: %v", err)
	}

	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 entries", deleted)
	}

	if nodeExists(t, c, "a") || nodeExists(t, c, "b") {
		t.Error("both namespace runs should have been deleted")
	}

	if !nodeExists(t, c, "foreign") {
		t.Error("foreign-namespace run must not be deleted")
	}
}

func TestEnsureSharedRBACIdempotent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Namespace = "jordan-testing"
	c := fakePlaytimeClient()
	ctx := context.Background()

	saName, err := ensureSharedRBAC(ctx, c, cfg)
	if err != nil {
		t.Fatalf("ensureSharedRBAC: %v", err)
	}

	if saName != ReaperServiceAccountName {
		t.Fatalf("saName = %q, want %q", saName, ReaperServiceAccountName)
	}

	// A second call (as a later run would make) must not error and must not
	// duplicate the shared objects.
	if _, err := ensureSharedRBAC(ctx, c, cfg); err != nil {
		t.Fatalf("ensureSharedRBAC (repeat): %v", err)
	}

	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: ReaperServiceAccountName}, sa); err != nil {
		t.Fatalf("get shared service account: %v", err)
	}

	if sa.OwnerReferences != nil {
		t.Errorf("shared service account must not be owned by a run, got %v", sa.OwnerReferences)
	}

	role := &rbacv1.ClusterRole{}
	if err := c.Get(ctx, client.ObjectKey{Name: cfg.reaperClusterName()}, role); err != nil {
		t.Fatalf("get shared cluster role: %v", err)
	}

	binding := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: cfg.reaperClusterName()}, binding); err != nil {
		t.Fatalf("get shared cluster role binding: %v", err)
	}

	if binding.OwnerReferences != nil {
		t.Errorf("shared cluster role binding must not be owned by a run, got %v", binding.OwnerReferences)
	}
}
