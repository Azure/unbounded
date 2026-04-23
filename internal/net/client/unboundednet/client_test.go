// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unboundednet

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func mustUnstructured(t *testing.T, obj interface{}, kind string) *unstructured.Unstructured {
	t.Helper()

	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}

	m := map[string]interface{}{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal object to map: %v", err)
	}

	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion("net.unbounded-kube.io/v1alpha1")
	u.SetKind(kind)

	return u
}

func newFakeDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:        "SiteList",
			gatewayPoolGVR: "GatewayPoolList",
		},
		objects...,
	)
}

// TestSiteClientListGetWatchAndUpdateStatus tests SiteClientListGetWatchAndUpdateStatus.
func TestSiteClientListGetWatchAndUpdateStatus(t *testing.T) {
	siteObj := mustUnstructured(t, &unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec:       unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.0.0.0/16"}},
		Status:     unboundednetv1alpha1.SiteStatus{NodeCount: 1, SliceCount: 1},
	}, "Site")
	dyn := newFakeDynamic(siteObj)
	c := &siteClient{client: dyn.Resource(siteGVR)}
	ctx := context.Background()

	list, err := c.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(list.Items) != 1 || list.Items[0].Name != "site-a" {
		t.Fatalf("unexpected site list: %#v", list.Items)
	}

	site, err := c.Get(ctx, "site-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if site.Name != "site-a" {
		t.Fatalf("unexpected site get result: %#v", site)
	}

	if _, err := c.Get(ctx, "missing", metav1.GetOptions{}); err == nil {
		t.Fatalf("expected missing site get to fail")
	}

	w, err := c.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	w.Stop()

	site.Status.NodeCount = 3
	site.Status.SliceCount = 2

	updated, err := c.UpdateStatus(ctx, site, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	if updated.Status.NodeCount != 3 || updated.Status.SliceCount != 2 {
		t.Fatalf("unexpected updated status: %#v", updated.Status)
	}
}

// TestGatewayPoolClientListGetAndWatch tests GatewayPoolClientListGetAndWatch.
func TestGatewayPoolClientListGetAndWatch(t *testing.T) {
	poolObj := mustUnstructured(t, &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         "External",
			NodeSelector: map[string]string{"role": "gateway"},
		},
	}, "GatewayPool")
	dyn := newFakeDynamic(poolObj)
	c := &gatewayPoolClient{client: dyn.Resource(gatewayPoolGVR)}
	ctx := context.Background()

	list, err := c.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(list.Items) != 1 || list.Items[0].Name != "pool-a" {
		t.Fatalf("unexpected gateway pool list: %#v", list.Items)
	}

	pool, err := c.Get(ctx, "pool-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if pool.Name != "pool-a" {
		t.Fatalf("unexpected gateway pool get result: %#v", pool)
	}

	w, err := c.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	w.Stop()
}

// TestNewClientsInvalidConfig tests NewClientsInvalidConfig.
func TestNewClientsInvalidConfig(t *testing.T) {
	cfg := &rest.Config{Host: "://bad-url"}

	if _, err := NewSiteClient(cfg); err == nil {
		t.Fatalf("expected NewSiteClient to fail with empty config")
	}

	if _, err := NewGatewayPoolClient(cfg); err == nil {
		t.Fatalf("expected NewGatewayPoolClient to fail with empty config")
	}
}
