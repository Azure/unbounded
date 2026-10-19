// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func toUnstructuredSiteWatch(t *testing.T, v interface{}) *unstructured.Unstructured {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	return &unstructured.Unstructured{Object: obj}
}

func newTestInformer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
}

// TestFindMySiteFromCRDsAndGatewayDetection tests FindMySiteFromCRDsAndGatewayDetection.
func TestFindMySiteFromCRDsAndGatewayDetection(t *testing.T) {
	sliceInformer := newTestInformer()
	gatewayPoolInformer := newTestInformer()

	slice := &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		SiteName:   "site-a",
		Nodes: []unboundednetv1alpha1.NodeInfo{
			{Name: "node-a", WireGuardPublicKey: "pub-a"},
		},
	}
	if err := sliceInformer.GetStore().Add(toUnstructuredSiteWatch(t, slice)); err != nil {
		t.Fatalf("add slice failed: %v", err)
	}

	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{
			{Name: "gw-a", WireGuardPublicKey: "pub-gw", SiteName: "site-gw"},
		}},
	}
	if err := gatewayPoolInformer.GetStore().Add(toUnstructuredSiteWatch(t, pool)); err != nil {
		t.Fatalf("add pool failed: %v", err)
	}

	if got, resolved := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, "pub-a"); got != "site-a" || !resolved {
		t.Fatalf("unexpected site from slices: site=%q resolved=%v", got, resolved)
	}

	if got, resolved := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, "pub-gw"); got != "site-gw" || !resolved {
		t.Fatalf("unexpected site from gateway pool: site=%q resolved=%v", got, resolved)
	}

	if !isGatewayNodeFromCRDs(gatewayPoolInformer, "pub-gw") {
		t.Fatalf("expected pub-gw to be detected as gateway node")
	}

	if isGatewayNodeFromCRDs(gatewayPoolInformer, "unknown") {
		t.Fatalf("did not expect unknown key to be detected as gateway node")
	}
}

// TestWaitForSiteMembershipImmediate tests WaitForSiteMembershipImmediate.
func TestWaitForSiteMembershipImmediate(t *testing.T) {
	sliceInformer := newTestInformer()
	gatewayPoolInformer := newTestInformer()

	slice := &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		SiteName:   "site-a",
		Nodes: []unboundednetv1alpha1.NodeInfo{
			{Name: "node-a", WireGuardPublicKey: "pub-a"},
		},
	}
	if err := sliceInformer.GetStore().Add(toUnstructuredSiteWatch(t, slice)); err != nil {
		t.Fatalf("add slice failed: %v", err)
	}

	got, err := waitForSiteMembership(context.Background(), sliceInformer, gatewayPoolInformer, "pub-a")
	if err != nil {
		t.Fatalf("waitForSiteMembership returned error: %v", err)
	}

	if got != "site-a" {
		t.Fatalf("unexpected site membership result: %q", got)
	}
}

func TestWaitForSiteMembershipGatewayWithoutSite(t *testing.T) {
	sliceInformer := newTestInformer()
	gatewayPoolInformer := newTestInformer()

	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{
			{Name: "gw-a", WireGuardPublicKey: "pub-gw"},
		}},
	}
	if err := gatewayPoolInformer.GetStore().Add(toUnstructuredSiteWatch(t, pool)); err != nil {
		t.Fatalf("add pool failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := waitForSiteMembership(ctx, sliceInformer, gatewayPoolInformer, "pub-gw")
	if err != nil {
		t.Fatalf("published gateway without a site did not resolve: %v", err)
	}

	if got != "" {
		t.Fatalf("gateway without a site resolved to %q", got)
	}
}

func membershipPoolObject(t *testing.T, nodes ...unboundednetv1alpha1.GatewayNodeInfo) *unstructured.Unstructured {
	t.Helper()

	return toUnstructuredSiteWatch(t, &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
		Status:     unboundednetv1alpha1.GatewayPoolStatus{Nodes: nodes},
	})
}

func membershipSliceObject(t *testing.T, key, site string) *unstructured.Unstructured {
	t.Helper()

	return toUnstructuredSiteWatch(t, &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		SiteName:   site,
		Nodes:      []unboundednetv1alpha1.NodeInfo{{Name: "node-a", WireGuardPublicKey: key}},
	})
}

func TestFindMySiteFromCRDsMembership(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		pool         bool
		gateways     []unboundednetv1alpha1.GatewayNodeInfo
		slice        *unstructured.Unstructured
		wantSite     string
		wantResolved bool
		wantGateway  bool
	}{
		{
			name: "gateway without site", key: "pub-a", pool: true,
			gateways:     []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a", WireGuardPublicKey: "pub-a"}},
			wantResolved: true, wantGateway: true,
		},
		{
			name: "gateway with named site", key: "pub-a", pool: true,
			gateways: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a", WireGuardPublicKey: "pub-a", SiteName: "site-gw"}},
			wantSite: "site-gw", wantResolved: true, wantGateway: true,
		},
		{
			name: "named site member", key: "pub-a",
			slice:    membershipSliceObject(t, "pub-a", "site-a"),
			wantSite: "site-a", wantResolved: true,
		},
		{
			name: "named site keeps precedence", key: "pub-a", pool: true,
			gateways: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a", WireGuardPublicKey: "pub-a", SiteName: "site-gw"}},
			slice:    membershipSliceObject(t, "pub-a", "site-a"),
			wantSite: "site-a", wantResolved: true, wantGateway: true,
		},
		{name: "absent membership", key: "pub-a"},
		{name: "pool selector without published node", key: "pub-a", pool: true},
		{
			name: "unpublished gateway key", key: "pub-a", pool: true,
			gateways: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a"}},
		},
		{
			name: "mismatched published key", key: "pub-a", pool: true,
			gateways: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a", WireGuardPublicKey: "pub-other"}},
		},
		{
			name: "empty lookup key", pool: true,
			gateways: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a", WireGuardPublicKey: "pub-a"}},
		},
		{
			name: "empty keys do not establish identity", pool: true,
			gateways: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "gw-a"}},
		},
		{
			name: "unnamed regular site is unresolved", key: "pub-a",
			slice: membershipSliceObject(t, "pub-a", ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slices, pools := newTestInformer(), newTestInformer()
			if tt.pool {
				if err := pools.GetStore().Add(membershipPoolObject(t, tt.gateways...)); err != nil {
					t.Fatalf("add pool: %v", err)
				}
			}

			if tt.slice != nil {
				if err := slices.GetStore().Add(tt.slice); err != nil {
					t.Fatalf("add slice: %v", err)
				}
			}

			site, resolved := findMySiteFromCRDs(slices, pools, tt.key)
			if site != tt.wantSite || resolved != tt.wantResolved {
				t.Fatalf("site=%q resolved=%v, want site=%q resolved=%v", site, resolved, tt.wantSite, tt.wantResolved)
			}

			if gateway := isGatewayNodeFromCRDs(pools, tt.key); gateway != tt.wantGateway {
				t.Fatalf("gateway=%v, want %v", gateway, tt.wantGateway)
			}

			if !tt.wantResolved {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				if _, err := waitForSiteMembership(ctx, slices, pools, tt.key); !errors.Is(err, context.Canceled) {
					t.Fatalf("unresolved membership bypassed cancellation: %v", err)
				}
			}
		})
	}
}

type siteMembershipInformer struct {
	cache.SharedIndexInformer
	added   chan cache.ResourceEventHandler
	removed chan struct{}
	addErr  error
}

func newSiteMembershipInformer() *siteMembershipInformer {
	return &siteMembershipInformer{
		SharedIndexInformer: newTestInformer(),
		added:               make(chan cache.ResourceEventHandler, 1),
		removed:             make(chan struct{}, 1),
	}
}

func (i *siteMembershipInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	if i.addErr != nil {
		return nil, i.addErr
	}

	registration, err := i.SharedIndexInformer.AddEventHandler(handler)
	if err == nil {
		i.added <- handler
	}

	return registration, err
}

func (i *siteMembershipInformer) RemoveEventHandler(registration cache.ResourceEventHandlerRegistration) error {
	if err := i.SharedIndexInformer.RemoveEventHandler(registration); err != nil {
		return err
	}

	i.removed <- struct{}{}

	return nil
}

type siteMembershipResult struct {
	site string
	err  error
}

func startSiteMembershipWait(ctx context.Context, slices, pools cache.SharedIndexInformer) <-chan siteMembershipResult {
	result := make(chan siteMembershipResult, 1)

	go func() {
		site, err := waitForSiteMembership(ctx, slices, pools, "pub-a")
		result <- siteMembershipResult{site: site, err: err}
	}()

	return result
}

func awaitMembershipHandler(t *testing.T, ctx context.Context, informer *siteMembershipInformer) cache.ResourceEventHandler {
	t.Helper()

	select {
	case handler := <-informer.added:
		return handler
	case <-ctx.Done():
		t.Fatalf("membership handler was not registered: %v", ctx.Err())
		return nil
	}
}

func assertMembershipHandlerRemoved(t *testing.T, informer *siteMembershipInformer) {
	t.Helper()

	select {
	case <-informer.removed:
	default:
		t.Fatal("membership handler was not removed")
	}
}

func TestWaitForSiteMembershipPublishedEvents(t *testing.T) {
	tests := []struct {
		name    string
		gateway bool
		update  bool
		site    string
	}{
		{name: "siteless gateway add", gateway: true},
		{name: "siteless gateway update", gateway: true, update: true},
		{name: "named gateway add", gateway: true, site: "site-gw"},
		{name: "named gateway update", gateway: true, update: true, site: "site-gw"},
		{name: "site member add", site: "site-a"},
		{name: "site member update", update: true, site: "site-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			slices, pools := newSiteMembershipInformer(), newSiteMembershipInformer()
			target := slices
			object := membershipSliceObject(t, "pub-a", tt.site)

			old := membershipSliceObject(t, "pub-other", tt.site)
			if tt.gateway {
				target = pools
				object = membershipPoolObject(t, unboundednetv1alpha1.GatewayNodeInfo{Name: "gw-a", WireGuardPublicKey: "pub-a", SiteName: tt.site})
				old = membershipPoolObject(t, unboundednetv1alpha1.GatewayNodeInfo{Name: "gw-a", WireGuardPublicKey: "pub-other", SiteName: tt.site})
			}

			if tt.update {
				if err := target.GetStore().Add(old); err != nil {
					t.Fatalf("add mismatched membership: %v", err)
				}
			}

			result := startSiteMembershipWait(ctx, slices, pools)
			sliceHandler := awaitMembershipHandler(t, ctx, slices)
			poolHandler := awaitMembershipHandler(t, ctx, pools)

			select {
			case got := <-result:
				t.Fatalf("unpublished or mismatched membership resolved: %#v", got)
			default:
			}

			handler := sliceHandler
			if tt.gateway {
				handler = poolHandler
			}

			if tt.update {
				if err := target.GetStore().Update(object); err != nil {
					t.Fatalf("update membership: %v", err)
				}

				handler.OnUpdate(old, object)
			} else {
				if err := target.GetStore().Add(object); err != nil {
					t.Fatalf("add membership: %v", err)
				}

				handler.OnAdd(object, false)
			}

			select {
			case got := <-result:
				if got.err != nil || got.site != tt.site {
					t.Fatalf("site=%q err=%v, want site=%q", got.site, got.err, tt.site)
				}
			case <-ctx.Done():
				t.Fatalf("published membership did not resolve: %v", ctx.Err())
			}

			assertMembershipHandlerRemoved(t, slices)
			assertMembershipHandlerRemoved(t, pools)
		})
	}
}

func TestWaitForSiteMembershipCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slices, pools := newSiteMembershipInformer(), newSiteMembershipInformer()
	result := startSiteMembershipWait(ctx, slices, pools)

	registrationCtx, registrationCancel := context.WithTimeout(context.Background(), time.Second)
	defer registrationCancel()

	awaitMembershipHandler(t, registrationCtx, slices)
	awaitMembershipHandler(t, registrationCtx, pools)
	cancel()

	select {
	case got := <-result:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation was not preserved: %v", got.err)
		}
	case <-registrationCtx.Done():
		t.Fatal("membership wait did not stop after cancellation")
	}

	assertMembershipHandlerRemoved(t, slices)
	assertMembershipHandlerRemoved(t, pools)
}

func TestWaitForSiteMembershipHandlerErrors(t *testing.T) {
	for _, poolError := range []bool{false, true} {
		name := "slice registration"
		if poolError {
			name = "pool registration"
		}

		t.Run(name, func(t *testing.T) {
			slices, pools := newSiteMembershipInformer(), newSiteMembershipInformer()

			want := errors.New("handler registration failed")
			if poolError {
				pools.addErr = want
			} else {
				slices.addErr = want
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			if _, err := waitForSiteMembership(ctx, slices, pools, "pub-a"); !errors.Is(err, want) {
				t.Fatalf("registration error was not preserved: %v", err)
			}

			if poolError {
				assertMembershipHandlerRemoved(t, slices)
			}
		})
	}
}

// TestGetManageCniPluginFromCRDs tests GetManageCniPluginFromCRDs.
func TestGetManageCniPluginFromCRDs(t *testing.T) {
	siteInformer := newTestInformer()

	trueVal := true
	falseVal := false

	sites := []*unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-default"}, Spec: unboundedv1alpha3.SiteSpec{ManageCniPlugin: nil}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-true"}, Spec: unboundedv1alpha3.SiteSpec{ManageCniPlugin: &trueVal}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-false"}, Spec: unboundedv1alpha3.SiteSpec{ManageCniPlugin: &falseVal}},
	}
	for _, s := range sites {
		if err := siteInformer.GetStore().Add(toUnstructuredSiteWatch(t, s)); err != nil {
			t.Fatalf("add site failed: %v", err)
		}
	}

	if !getManageCniPluginFromCRDs(siteInformer, "") {
		t.Fatalf("expected empty site default to true")
	}

	if !getManageCniPluginFromCRDs(siteInformer, "site-default") {
		t.Fatalf("expected nil ManageCniPlugin default to true")
	}

	if !getManageCniPluginFromCRDs(siteInformer, "site-true") {
		t.Fatalf("expected explicit true to be honored")
	}

	if getManageCniPluginFromCRDs(siteInformer, "site-false") {
		t.Fatalf("expected explicit false to be honored")
	}

	if !getManageCniPluginFromCRDs(siteInformer, "site-missing") {
		t.Fatalf("expected missing site default to true")
	}
}
