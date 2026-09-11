// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/authn"
)

const controllerServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

type oidcVerifierFactory func(context.Context, string, string) (serviceAccountTokenVerifier, error)

func initializeNodeTokenVerifier(ctx context.Context, client kubernetes.Interface, issuer, audience, tokenPath string, newOIDC oidcVerifierFactory) (serviceAccountTokenVerifier, error) {
	issuer = strings.TrimSpace(issuer)
	if issuer != "" {
		verifier, err := newOIDC(ctx, issuer, audience)
		if err != nil {
			return nil, fmt.Errorf("initialize configured Kubernetes OIDC verifier: %w", err)
		}

		klog.Infof("Enabled local Kubernetes service account token validation for configured issuer %s", issuer)

		return verifier, nil
	}

	verifier, err := discoverNodeTokenVerifier(ctx, audience, tokenPath, newOIDC)
	if err == nil {
		return verifier, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	klog.Warningf("Automatic Kubernetes OIDC discovery unavailable; using TokenReview for node token validation: %v", err)

	return authn.NewKubernetesTokenReviewVerifier(client.AuthenticationV1()), nil
}

func discoverNodeTokenVerifier(ctx context.Context, audience, tokenPath string, newOIDC oidcVerifierFactory) (serviceAccountTokenVerifier, error) {
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read controller service account token: %w", err)
	}

	issuer, audience, err := authn.DiscoverKubernetesOIDCConfig(strings.TrimSpace(string(token)), audience)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC configuration from mounted token: %w", err)
	}

	verifier, err := newOIDC(ctx, issuer, audience)
	if err != nil {
		return nil, fmt.Errorf("initialize discovered Kubernetes OIDC verifier: %w", err)
	}

	klog.Infof("Enabled local Kubernetes service account token validation for discovered issuer %s", issuer)

	return verifier, nil
}
