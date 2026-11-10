// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/config"
)

// DefaultKubeProxyRepository is where the managed kube-proxy image comes from
// when neither the configuration nor the cluster's own kube-proxy names one.
const DefaultKubeProxyRepository = "registry.k8s.io/kube-proxy"

const (
	providerKubeProxyImageRetryInitialInterval = 10 * time.Second
	providerKubeProxyImageRetryMaxInterval     = 5 * time.Minute
	providerKubeProxyImageRetryBudget          = 12 * time.Hour
)

// ManagedKubeProxyImage works out the image the managed kube-proxy DaemonSets
// run from the configured --managed-kube-proxy-image value:
//
//   - A reference with a tag or digest is used as-is, without reading the
//     cluster.
//   - A reference without one takes the tag of the image kube-system/kube-proxy
//     runs, so a mirror of the provider's image keeps the provider's version.
//     Without a provider tag, the registry is searched for the API server's
//     major.minor.patch version, preferring the highest numeric maintenance
//     suffix over the exact version tag.
//   - Empty reuses the kube-system/kube-proxy image, and otherwise falls back to
//     registry.k8s.io/kube-proxy. An unversioned provider image supplies its
//     repository, but still needs the API server version and registry search.
//
// Provider reads fall back after retries. Fallback selection requires a valid
// API server version and a matching registry tag; failures return errors rather
// than choosing latest. Explicit and provider-supplied versions are used as-is.
// The image is worked out once, when the controller starts leading.
func ManagedKubeProxyImage(ctx context.Context, clientset kubernetes.Interface, configured string) (string, error) {
	return managedKubeProxyImage(ctx, clientset, configured, registryKubeProxyTag)
}

func managedKubeProxyImage(
	ctx context.Context,
	clientset kubernetes.Interface,
	configured string,
	resolveTag func(context.Context, string, string) (string, error),
) (string, error) {
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

	provider, err := providerKubeProxyImage(ctx, clientset)
	if err != nil {
		return "", fmt.Errorf("managed kube-proxy image discovery: %w", err)
	}

	if configured == "" && provider != "" {
		ref, parseErr := config.ParseImageReference(provider)
		// Preserve provider references verbatim. Only a valid, unversioned
		// reference needs its repository carried into fallback tag selection.
		if parseErr != nil || ref.HasVersion() {
			return provider, nil
		}

		repository = ref.Name
	}

	tag := imageTag(provider)
	if tag != "" {
		return repository + ":" + tag, nil
	}

	serverTag, err := serverVersionTag(ctx, clientset)
	if err != nil {
		return "", fmt.Errorf("managed kube-proxy image: %w", err)
	}

	tag, err = resolveTag(ctx, repository, serverTag)
	if err != nil {
		return "", fmt.Errorf("managed kube-proxy image: %w", err)
	}

	return repository + ":" + tag, nil
}

// providerKubeProxyImage returns the image of the kube-proxy container in
// kube-system/kube-proxy, or an empty string when there is none to read.
// NotFound falls back immediately; other read errors use exponential backoff
// from ten seconds to five minutes for at most twelve hours before falling back.
// The budget covers both requests and waits. Parent cancellation stops discovery
// without falling back.
func providerKubeProxyImage(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	retryCtx, cancel := context.WithTimeout(ctx, providerKubeProxyImageRetryBudget)
	defer cancel()

	backoff := wait.Backoff{
		Duration: providerKubeProxyImageRetryInitialInterval,
		Factor:   2,
		Cap:      providerKubeProxyImageRetryMaxInterval,
		Steps:    math.MaxInt32,
	}

	var (
		image    string
		attempts int
	)

	// DelayFunc keeps returning the capped interval instead of ending retries
	// when the cap is reached. Only the context deadline limits the retry window.
	err := backoff.DelayFunc().Until(retryCtx, true, true, func(ctx context.Context) (bool, error) {
		// Until invokes its immediate condition even for an already-canceled
		// context, so check before issuing any API request.
		if err := ctx.Err(); err != nil {
			return false, err
		}

		attempts++

		ds, err := clientset.AppsV1().DaemonSets(metav1.NamespaceSystem).Get(ctx, "kube-proxy", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		if err != nil {
			klog.Warningf("Failed to read kube-system/kube-proxy for managed kube-proxy image (attempt %d, retry budget %s): %v", attempts, providerKubeProxyImageRetryBudget, err)

			return false, nil
		}

		for _, container := range ds.Spec.Template.Spec.Containers {
			if container.Name == "kube-proxy" && container.Image != "" {
				image = container.Image

				break
			}
		}

		return true, nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}

		klog.Warningf("Falling back to defaults after the %s retry budget for reading kube-system/kube-proxy expired (%d attempts)", providerKubeProxyImageRetryBudget, attempts)
	}

	return image, nil
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

// serverVersionTag returns the API server's major.minor.patch version, with a
// leading v, for matching registry tags. Vendor prerelease and build metadata
// are not part of the Kubernetes release version being matched.
func serverVersionTag(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("discover Kubernetes server version: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	if serverVersion == nil || serverVersion.GitVersion == "" {
		return "", fmt.Errorf("empty Kubernetes server version")
	}

	parsed, err := version.ParseSemantic(serverVersion.GitVersion)
	if err != nil {
		return "", fmt.Errorf("parse Kubernetes server version %q: %w", serverVersion.GitVersion, err)
	}

	return fmt.Sprintf("v%d.%d.%d", parsed.Major(), parsed.Minor(), parsed.Patch()), nil
}
