// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurepodcidr

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var Resource = schema.GroupVersionResource{Group: "acn.azure.com", Version: "v1alpha", Resource: "nodenetworkconfigs"}

func InformerIfAvailable(client discovery.DiscoveryInterface, factory dynamicinformer.DynamicSharedInformerFactory) (cache.SharedIndexInformer, error) {
	resources, err := client.ServerResourcesForGroupVersion(Resource.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("discover Azure NodeNetworkConfig API: %w", err)
	}

	for _, resource := range resources.APIResources {
		if resource.Name == Resource.Resource {
			return factory.ForResource(Resource).Informer(), nil
		}
	}

	return nil, nil
}

func UsesManagedCilium(node *corev1.Node) bool {
	return node.Labels["kubernetes.azure.com/ebpf-dataplane"] == "cilium"
}

func FromCache(node *corev1.Node, informer cache.SharedIndexInformer) ([]string, error) {
	if len(node.Spec.PodCIDRs) > 0 {
		return slices.Clone(node.Spec.PodCIDRs), nil
	}

	if !UsesManagedCilium(node) || informer == nil {
		return nil, nil
	}

	object, exists, err := informer.GetStore().GetByKey("kube-system/" + node.Name)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, fmt.Errorf("NodeNetworkConfig for node %s is not available", node.Name)
	}

	configuration, ok := object.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("invalid cached NodeNetworkConfig type %T", object)
	}

	return FromNodeNetworkConfig(node, configuration)
}

// Get falls back to Azure's per-node overlay allocation, never the cluster subnet.
func Get(ctx context.Context, client dynamic.Interface, node *corev1.Node) ([]string, error) {
	if len(node.Spec.PodCIDRs) > 0 {
		return slices.Clone(node.Spec.PodCIDRs), nil
	}

	if !UsesManagedCilium(node) {
		return nil, nil
	}

	object, err := client.Resource(Resource).Namespace("kube-system").Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get NodeNetworkConfig for node %s: %w", node.Name, err)
	}

	return FromNodeNetworkConfig(node, object)
}

func FromNodeNetworkConfig(node *corev1.Node, object *unstructured.Unstructured) ([]string, error) {
	if node.UID == "" || object.GetName() != node.Name || object.GetNamespace() != "kube-system" {
		return nil, fmt.Errorf("NodeNetworkConfig identity does not match node %s", node.Name)
	}

	owned := false

	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == "v1" && owner.Kind == "Node" && owner.Name == node.Name && owner.UID == node.UID {
			owned = true
		}
	}

	if !owned {
		return nil, fmt.Errorf("NodeNetworkConfig is not owned by current node %s UID %s", node.Name, node.UID)
	}

	containers, _, err := unstructured.NestedSlice(object.Object, "status", "networkContainers")
	if err != nil {
		return nil, err
	}

	var cidrs []string

	for _, value := range containers {
		container, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid NodeNetworkConfig network container")
		}

		if container["type"] != "overlay" || container["assignmentMode"] != "static" {
			continue
		}

		nodeIP, ok := container["nodeIP"].(string)
		if !ok || !slices.ContainsFunc(node.Status.Addresses, func(address corev1.NodeAddress) bool {
			return address.Type == corev1.NodeInternalIP && address.Address == nodeIP
		}) {
			return nil, fmt.Errorf("NodeNetworkConfig network container does not match node %s internal IP", node.Name)
		}

		for _, field := range []string{"primaryIP", "primaryIPV6"} {
			value, exists := container[field]
			if !exists || value == "" {
				continue
			}

			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid NodeNetworkConfig %s", field)
			}

			prefix, err := netip.ParsePrefix(text)
			if err != nil || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
				return nil, fmt.Errorf("invalid NodeNetworkConfig overlay CIDR %q", text)
			}

			if !slices.Contains(cidrs, prefix.String()) {
				cidrs = append(cidrs, prefix.String())
			}
		}
	}

	if len(cidrs) == 0 {
		return nil, fmt.Errorf("no static overlay assignment in NodeNetworkConfig for node %s", node.Name)
	}

	return cidrs, nil
}
