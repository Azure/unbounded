// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/config"
)

// DefaultKubeProxyRepository is where the managed kube-proxy image comes from
// when neither the configuration nor the cluster's own kube-proxy names one.
const DefaultKubeProxyRepository = "registry.k8s.io/kube-proxy"

// ManagedKubeProxyImage works out the image the managed kube-proxy DaemonSets
// run from the configured --managed-kube-proxy-image value:
//
//   - A reference with a tag or digest is used as-is, without reading the
//     cluster.
//   - A reference without one takes the tag of the image kube-system/kube-proxy
//     runs, so a mirror of the provider's image keeps the provider's version.
//     Without that DaemonSet, or when its image names no tag, the tag is the
//     API server's version.
//   - Empty reuses the kube-system/kube-proxy image, and otherwise falls back to
//     registry.k8s.io/kube-proxy at the API server's version.
//
// The only error is a configured value that is not an image reference. A
// cluster that cannot be read falls back instead, as it always has: the image
// is worked out once, when the controller starts leading, and failing there
// would leave managed kube-proxy unreconciled until the next restart.
func ManagedKubeProxyImage(ctx context.Context, clientset kubernetes.Interface, configured string) (string, error) {
	repository := DefaultKubeProxyRepository

	if configured != "" {
		ref, err := config.ParseImageReference(configured)
		if err != nil {
			return "", fmt.Errorf("managed kube-proxy image: %w", err)
		}

		if ref.HasVersion() {
			return configured, nil
		}

		repository = configured
	}

	provider := providerKubeProxyImage(ctx, clientset)
	if configured == "" && provider != "" {
		return provider, nil
	}

	tag := imageTag(provider)
	if tag == "" {
		tag = serverVersionTag(clientset)
	}

	return repository + ":" + tag, nil
}

// providerKubeProxyImage returns the image of the kube-proxy container in
// kube-system/kube-proxy, or an empty string when there is none to read.
func providerKubeProxyImage(ctx context.Context, clientset kubernetes.Interface) string {
	ds, err := clientset.AppsV1().DaemonSets(metav1.NamespaceSystem).Get(ctx, "kube-proxy", metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Warningf("Failed to read kube-system/kube-proxy for managed kube-proxy image: %v", err)
		}

		return ""
	}

	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == "kube-proxy" && container.Image != "" {
			return container.Image
		}
	}

	return ""
}

// imageTag returns the tag of the provider's kube-proxy image. Its digest is
// deliberately not carried over: a mirror that re-pushed the image can hold it
// under a different digest, while the tag stays the same.
func imageTag(image string) string {
	if image == "" {
		return ""
	}

	ref, err := config.ParseImageReference(image)
	if err != nil {
		klog.Warningf("Cannot take a tag from kube-system/kube-proxy image %q: %v", image, err)

		return ""
	}

	return ref.Tag
}

// serverVersionTag returns the API server's version for use as an image tag,
// or latest when it cannot be read.
func serverVersionTag(clientset kubernetes.Interface) string {
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		klog.Warningf("Failed to discover Kubernetes server version for managed kube-proxy image: %v", err)

		return "latest"
	}

	if serverVersion.GitVersion == "" {
		return "latest"
	}

	return serverVersion.GitVersion
}
