// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"net/http"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

const EdgeTokenAudience = "metalman-edge"

// TokenReviewEdgeAuthenticator validates audience-bound Kubernetes ServiceAccount tokens.
type TokenReviewEdgeAuthenticator struct {
	Client             authenticationclient.AuthenticationV1Interface
	ServiceAccountName string
}

func (a *TokenReviewEdgeAuthenticator) Authenticate(ctx context.Context, request *http.Request) bool {
	if a == nil || a.Client == nil || strings.TrimSpace(a.ServiceAccountName) == "" {
		return false
	}
	token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return false
	}

	review, err := a.Client.TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{EdgeTokenAudience}},
	}, metav1.CreateOptions{})
	if err != nil || !review.Status.Authenticated || !slices.Contains(review.Status.Audiences, EdgeTokenAudience) {
		return false
	}

	prefix := "system:serviceaccount:"
	if !strings.HasPrefix(review.Status.User.Username, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(review.Status.User.Username, prefix), ":")

	return len(parts) == 2 && parts[1] == a.ServiceAccountName
}
