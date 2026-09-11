// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// KubernetesTokenReviewVerifier authenticates service account tokens through
// Kubernetes before trusting their embedded node identity.
type KubernetesTokenReviewVerifier struct {
	client authenticationclient.AuthenticationV1Interface
}

func NewKubernetesTokenReviewVerifier(client authenticationclient.AuthenticationV1Interface) *KubernetesTokenReviewVerifier {
	return &KubernetesTokenReviewVerifier{client: client}
}

func (v *KubernetesTokenReviewVerifier) Verify(ctx context.Context, token string) (*KubernetesServiceAccountIdentity, error) {
	if token == "" {
		return nil, fmt.Errorf("service account token is required")
	}

	reviewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// An empty audience list asks Kubernetes to validate its API-server audience,
	// which is the audience of the node agent's automounted service account token.
	review, err := v.client.TokenReviews().Create(reviewCtx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("review service account token: %w", err)
	}

	if review.Status.Error != "" {
		return nil, fmt.Errorf("service account token review failed: %s", review.Status.Error)
	}

	if !review.Status.Authenticated {
		return nil, fmt.Errorf("service account token is not authenticated")
	}

	// Only decode claims after Kubernetes has authenticated this exact token.
	identity, err := DecodeKubernetesServiceAccountIdentity(token)
	if err != nil {
		return nil, err
	}

	if identity.Namespace == "" || identity.ServiceAccountName == "" || identity.NodeName == "" {
		return nil, fmt.Errorf("service account token is missing service account or node identity claims")
	}

	expectedSubject := fmt.Sprintf("system:serviceaccount:%s:%s", identity.Namespace, identity.ServiceAccountName)
	if identity.Subject != expectedSubject || review.Status.User.Username != expectedSubject {
		return nil, fmt.Errorf("service account token claims do not match the reviewed identity")
	}

	return identity, nil
}
