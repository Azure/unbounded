// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"time"

	corelisters "k8s.io/client-go/listers/core/v1"
)

// PodBoundTokenVerifierOptions supplies process-lifetime informer caches.
// Ready must reject unsynced or stopped caches. Informer reads are eventually
// consistent, not a strongly consistent substitute for TokenReview.
type PodBoundTokenVerifierOptions struct {
	Namespace       string
	ServiceAccount  string
	Pods            corelisters.PodLister
	ServiceAccounts corelisters.ServiceAccountLister
	Ready           func() bool
}

// PodBoundTokenVerifier checks bound objects after cryptographic verification.
// It deliberately supports only Pod-bound node agents, not every Kubernetes
// service account token type, and never caches an authorization decision.
type PodBoundTokenVerifier struct {
	verifier interface {
		Verify(context.Context, string) (*KubernetesServiceAccountIdentity, error)
	}
	options PodBoundTokenVerifierOptions
	now     func() time.Time
}

// NewPodBoundTokenVerifier decorates an OIDC verifier, not a TokenReview verifier.
func NewPodBoundTokenVerifier(verifier interface {
	Verify(context.Context, string) (*KubernetesServiceAccountIdentity, error)
}, options PodBoundTokenVerifierOptions,
) *PodBoundTokenVerifier {
	return &PodBoundTokenVerifier{verifier: verifier, options: options, now: time.Now}
}

// Verify authenticates the token and rechecks its Pod and service account in the
// caches on every request, including when the signing keys are already cached.
func (v *PodBoundTokenVerifier) Verify(ctx context.Context, token string) (*KubernetesServiceAccountIdentity, error) {
	if err := v.checkReady(ctx); err != nil {
		return nil, err
	}

	identity, err := v.verifier.Verify(ctx, token)
	if err != nil {
		return nil, err
	}

	if identity == nil || identity.Namespace != v.options.Namespace ||
		identity.ServiceAccountName != v.options.ServiceAccount ||
		identity.Namespace == "" || identity.ServiceAccountName == "" ||
		identity.Subject != "system:serviceaccount:"+identity.Namespace+":"+identity.ServiceAccountName {
		return nil, fmt.Errorf("unexpected node service account identity")
	}

	if identity.PodName == "" || identity.PodUID == "" || identity.ServiceAccountUID == "" || identity.NodeName == "" {
		return nil, fmt.Errorf("node token requires complete Pod, service account UID, and node claims")
	}

	sa, err := v.options.ServiceAccounts.ServiceAccounts(identity.Namespace).Get(identity.ServiceAccountName)
	if err != nil {
		return nil, fmt.Errorf("get bound service account from cache: %w", err)
	}

	// Kubernetes accepts deletion timestamps exactly at the 60-second boundary.
	cutoff := v.now().Add(-60 * time.Second)
	if string(sa.UID) != identity.ServiceAccountUID ||
		(sa.DeletionTimestamp != nil && sa.DeletionTimestamp.Time.Before(cutoff)) {
		return nil, fmt.Errorf("bound service account has been deleted or replaced")
	}

	pod, err := v.options.Pods.Pods(identity.Namespace).Get(identity.PodName)
	if err != nil {
		return nil, fmt.Errorf("get bound Pod from cache: %w", err)
	}

	if string(pod.UID) != identity.PodUID ||
		(pod.DeletionTimestamp != nil && pod.DeletionTimestamp.Time.Before(cutoff)) {
		return nil, fmt.Errorf("bound Pod has been deleted or replaced")
	}

	// For our node-only scope, also tie the informational node claim and service
	// account identity to the Pod spec. Node readiness is not an auth gate.
	if pod.Spec.ServiceAccountName != identity.ServiceAccountName || pod.Spec.NodeName != identity.NodeName {
		return nil, fmt.Errorf("bound Pod service account or node does not match token")
	}

	if err := v.checkReady(ctx); err != nil {
		return nil, err
	}

	return identity, nil
}

func (v *PodBoundTokenVerifier) checkReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if v.options.Ready == nil || !v.options.Ready() {
		return fmt.Errorf("node authentication object caches are not ready")
	}

	return nil
}
