// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

type publicIPDiscoveryConfig struct {
	Enabled              bool
	Server               string
	RecheckInterval      time.Duration
	InitialDelayLimit    time.Duration
	CleanupRetryInterval time.Duration
}

type publicIPDiscoverer interface {
	DiscoverPublicIP(ctx context.Context, server string) (netip.Addr, error)
}

func runPublicIPDiscovery(
	ctx context.Context,
	clientset kubernetes.Interface,
	nodeName string,
	cfg publicIPDiscoveryConfig,
	discoverer publicIPDiscoverer,
) {
	initialDelay := time.Duration(0)
	if cfg.Enabled {
		initialDelay = publicIPDiscoveryInitialDelay(nodeName, cfg.InitialDelayLimit)
	}

	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		err := discoverAndAnnotateNodePublicIP(
			ctx,
			clientset,
			nodeName,
			cfg,
			discoverer,
			time.Second,
			2*time.Second,
		)
		if err != nil {
			nodePublicIPDiscoveryFailures.Inc()
			klog.Warningf("Public IP discovery failed: %v", err)
		}

		if !cfg.Enabled && err == nil {
			return
		}

		recheckInterval := cfg.RecheckInterval
		if !cfg.Enabled {
			recheckInterval = cfg.CleanupRetryInterval
			if recheckInterval <= 0 {
				recheckInterval = publicIPDiscoveryCleanupRetryInterval
			}
		}

		timer.Reset(recheckInterval)
	}
}

func publicIPDiscoveryInitialDelay(nodeName string, limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}

	hash := fnv.New64a()
	_, _ = hash.Write([]byte(nodeName))

	return time.Duration(hash.Sum64() % uint64(limit))
}

func discoverAndAnnotateNodePublicIP(
	ctx context.Context,
	clientset kubernetes.Interface,
	nodeName string,
	cfg publicIPDiscoveryConfig,
	discoverer publicIPDiscoverer,
	retryBackoffs ...time.Duration,
) error {
	apiCtx, cancel := context.WithTimeout(ctx, publicIPDiscoveryTimeout)
	node, err := clientset.CoreV1().Nodes().Get(apiCtx, nodeName, metav1.GetOptions{})

	cancel()

	if err != nil {
		return fmt.Errorf("get node %s before public IP discovery: %w", nodeName, err)
	}

	_, hasDeclaredIP := node.Annotations[unboundednetv1alpha1.NodeDeclaredPublicIPAnnotation]
	if !cfg.Enabled || hasDeclaredIP || nodeHasExternalIP(node) {
		apiCtx, cancel = context.WithTimeout(ctx, publicIPDiscoveryTimeout)
		defer cancel()

		return removeDiscoveredNodePublicIP(apiCtx, clientset, node)
	}

	publicIP, err := discoverPublicIPWithRetry(ctx, discoverer, cfg.Server, retryBackoffs...)
	if err != nil {
		return fmt.Errorf("query STUN server %s: %w", cfg.Server, err)
	}

	publicIPString := publicIP.String()

	// Keep the last confirmed address through two missed rechecks.
	expiresAt := time.Now().UTC().Add(3 * cfg.RecheckInterval).Format(time.RFC3339Nano)

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation:          publicIPString,
				unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation: expiresAt,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal public IP annotation patch: %w", err)
	}

	apiCtx, cancel = context.WithTimeout(ctx, publicIPDiscoveryTimeout)
	defer cancel()

	if _, err := clientset.CoreV1().Nodes().Patch(apiCtx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate node %s with discovered public IP: %w", nodeName, err)
	}

	klog.Infof("Annotated node %s with STUN-discovered public IP %s", nodeName, publicIPString)

	return nil
}

func discoverPublicIPWithRetry(
	ctx context.Context,
	discoverer publicIPDiscoverer,
	server string,
	retryBackoffs ...time.Duration,
) (netip.Addr, error) {
	var err error

	for attempt := 0; attempt <= len(retryBackoffs); attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, publicIPDiscoveryTimeout)
		publicIP, discoverErr := discoverer.DiscoverPublicIP(attemptCtx, server)

		cancel()

		if discoverErr == nil {
			return publicIP, nil
		}

		err = discoverErr

		if ctx.Err() != nil {
			return netip.Addr{}, ctx.Err()
		}

		if attempt == len(retryBackoffs) {
			break
		}

		timer := time.NewTimer(retryBackoffs[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()

			return netip.Addr{}, ctx.Err()
		case <-timer.C:
		}
	}

	return netip.Addr{}, fmt.Errorf("STUN discovery failed after %d attempts: %w", len(retryBackoffs)+1, err)
}

func nodeHasExternalIP(node *corev1.Node) bool {
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeExternalIP {
			return true
		}
	}

	return false
}

func removeDiscoveredNodePublicIP(ctx context.Context, clientset kubernetes.Interface, node *corev1.Node) error {
	_, hasIP := node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation]

	_, hasExpiry := node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation]
	if !hasIP && !hasExpiry {
		return nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation:          nil,
				unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation: nil,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal public IP annotation cleanup patch: %w", err)
	}

	if _, err := clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("remove discovered public IP from node %s: %w", node.Name, err)
	}

	klog.Infof("Removed STUN-discovered public IP from node %s", node.Name)

	return nil
}
