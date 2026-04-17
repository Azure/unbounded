// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unboundednet

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
)

// SiteInterface has methods to work with Site resources.
type SiteInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*unboundednetv1alpha1.SiteList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*unboundednetv1alpha1.Site, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	UpdateStatus(ctx context.Context, site *unboundednetv1alpha1.Site, opts metav1.UpdateOptions) (*unboundednetv1alpha1.Site, error)
}

// GatewayPoolInterface has methods to work with GatewayPool resources.
type GatewayPoolInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*unboundednetv1alpha1.GatewayPoolList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*unboundednetv1alpha1.GatewayPool, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// siteClient implements SiteInterface
type siteClient struct {
	client dynamic.NamespaceableResourceInterface
}

var siteGVR = schema.GroupVersionResource{
	Group:    unboundednetv1alpha1.GroupName,
	Version:  "v1alpha1",
	Resource: "sites",
}

var gatewayPoolGVR = schema.GroupVersionResource{
	Group:    unboundednetv1alpha1.GroupName,
	Version:  "v1alpha1",
	Resource: "gatewaypools",
}

// NewSiteClient creates a new client for Site resources
func NewSiteClient(config *rest.Config) (SiteInterface, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return &siteClient{
		client: dynamicClient.Resource(siteGVR),
	}, nil
}

// gatewayPoolClient implements GatewayPoolInterface
type gatewayPoolClient struct {
	client dynamic.NamespaceableResourceInterface
}

// NewGatewayPoolClient creates a new client for GatewayPool resources.
func NewGatewayPoolClient(config *rest.Config) (GatewayPoolInterface, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return &gatewayPoolClient{
		client: dynamicClient.Resource(gatewayPoolGVR),
	}, nil
}

// List returns Site resources matching list options.
func (c *siteClient) List(ctx context.Context, opts metav1.ListOptions) (*unboundednetv1alpha1.SiteList, error) {
	unstructuredList, err := c.client.List(ctx, opts)
	if err != nil {
		return nil, err
	}

	siteList := &unboundednetv1alpha1.SiteList{}

	data, err := unstructuredList.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured list: %w", err)
	}

	if err := json.Unmarshal(data, siteList); err != nil {
		return nil, fmt.Errorf("failed to unmarshal site list: %w", err)
	}

	return siteList, nil
}

// Get returns a Site resource by name.
func (c *siteClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*unboundednetv1alpha1.Site, error) {
	unstructured, err := c.client.Get(ctx, name, opts)
	if err != nil {
		return nil, err
	}

	site := &unboundednetv1alpha1.Site{}

	data, err := unstructured.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured: %w", err)
	}

	if err := json.Unmarshal(data, site); err != nil {
		return nil, fmt.Errorf("failed to unmarshal site: %w", err)
	}

	return site, nil
}

// Watch starts a watch for Site resources.
func (c *siteClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return c.client.Watch(ctx, opts)
}

// UpdateStatus updates only the status subresource for a Site.
func (c *siteClient) UpdateStatus(ctx context.Context, site *unboundednetv1alpha1.Site, opts metav1.UpdateOptions) (*unboundednetv1alpha1.Site, error) {
	// Create a patch for just the status
	statusPatch := map[string]interface{}{
		"status": map[string]interface{}{
			"nodeCount":  site.Status.NodeCount,
			"sliceCount": site.Status.SliceCount,
		},
	}

	patchData, err := json.Marshal(statusPatch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal status patch: %w", err)
	}

	unstructured, err := c.client.Patch(ctx, site.Name, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err != nil {
		return nil, err
	}

	updatedSite := &unboundednetv1alpha1.Site{}

	data, err := unstructured.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured: %w", err)
	}

	if err := json.Unmarshal(data, updatedSite); err != nil {
		return nil, fmt.Errorf("failed to unmarshal site: %w", err)
	}

	return updatedSite, nil
}

// List returns GatewayPool resources matching list options.
func (c *gatewayPoolClient) List(ctx context.Context, opts metav1.ListOptions) (*unboundednetv1alpha1.GatewayPoolList, error) {
	unstructuredList, err := c.client.List(ctx, opts)
	if err != nil {
		return nil, err
	}

	poolList := &unboundednetv1alpha1.GatewayPoolList{}

	data, err := unstructuredList.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured list: %w", err)
	}

	if err := json.Unmarshal(data, poolList); err != nil {
		return nil, fmt.Errorf("failed to unmarshal gateway pool list: %w", err)
	}

	return poolList, nil
}

// Get returns a GatewayPool resource by name.
func (c *gatewayPoolClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*unboundednetv1alpha1.GatewayPool, error) {
	unstructured, err := c.client.Get(ctx, name, opts)
	if err != nil {
		return nil, err
	}

	pool := &unboundednetv1alpha1.GatewayPool{}

	data, err := unstructured.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal unstructured: %w", err)
	}

	if err := json.Unmarshal(data, pool); err != nil {
		return nil, fmt.Errorf("failed to unmarshal gateway pool: %w", err)
	}

	return pool, nil
}

// Watch starts a watch for GatewayPool resources.
func (c *gatewayPoolClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return c.client.Watch(ctx, opts)
}
