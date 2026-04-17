// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/net/authn"
	webhookpkg "github.com/Azure/unbounded-kube/internal/net/webhook"
)

const (
	aggregatedTokenNodePath   = "/apis/status.net.unbounded-kube.io/v1alpha1/token/node"
	aggregatedTokenViewerPath = "/apis/status.net.unbounded-kube.io/v1alpha1/token/viewer"
)

// tokenEndpointConfig holds configurable parameters for token endpoints.
type tokenEndpointConfig struct {
	nodeTokenLifetime   time.Duration // default 4 hours
	viewerTokenLifetime time.Duration // default 30 minutes
}

// tokenNodeRequest is the JSON body for the token/node endpoint.
type tokenNodeRequest struct {
	ServiceAccountToken string `json:"serviceAccountToken"`
}

// tokenNodeResponse is the JSON response from the token/node endpoint.
type tokenNodeResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
	NodeName  string `json:"nodeName"`
}

// tokenViewerResponse is the JSON response from the token/viewer endpoint.
type tokenViewerResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

// registerTokenEndpoints registers the /token/node and /token/viewer handlers
// on the aggregated API paths. These endpoints issue HMAC tokens after
// verifying the caller's identity via front-proxy cert auth and SAR.
func registerTokenEndpoints(mux *http.ServeMux, health *healthState, webhookServer *webhookpkg.Server, tokenIssuer *authn.TokenIssuer, cfg tokenEndpointConfig) {
	if cfg.nodeTokenLifetime <= 0 {
		cfg.nodeTokenLifetime = 4 * time.Hour
	}

	if cfg.viewerTokenLifetime <= 0 {
		cfg.viewerTokenLifetime = 30 * time.Minute
	}

	mux.HandleFunc(aggregatedTokenNodePath, func(w http.ResponseWriter, r *http.Request) {
		handleTokenNode(w, r, health, webhookServer, tokenIssuer, cfg)
	})

	mux.HandleFunc(aggregatedTokenViewerPath, func(w http.ResponseWriter, r *http.Request) {
		handleTokenViewer(w, r, health, webhookServer, tokenIssuer, cfg)
	})
}

// handleTokenNode handles the token/node endpoint. It verifies the
// front-proxy cert, performs a SAR, validates the submitted SA token via
// TokenReview, extracts the node name, and issues an HMAC node token.
func handleTokenNode(w http.ResponseWriter, r *http.Request, health *healthState, webhookServer *webhookpkg.Server, tokenIssuer *authn.TokenIssuer, cfg tokenEndpointConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Verify front-proxy cert.
	if !webhookServer.IsTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 2. Extract X-Remote-User and X-Remote-Group headers.
	remoteUser := strings.TrimSpace(r.Header.Get("X-Remote-User"))
	if remoteUser == "" {
		http.Error(w, "missing X-Remote-User header", http.StatusBadRequest)
		return
	}

	remoteGroups := r.Header.Values("X-Remote-Group")

	// 3. Perform SAR: can this user create token/node?
	if !performSAR(r.Context(), health.clientset, remoteUser, remoteGroups, "create", "token", "node") {
		klog.V(2).Infof("Token/node SAR denied for user %q", remoteUser)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	// 4. Parse request body.
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var req tokenNodeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.ServiceAccountToken == "" {
		http.Error(w, "serviceAccountToken is required", http.StatusBadRequest)
		return
	}

	// 5. Validate the SA token via TokenReview.
	tokenReviewCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	username, ok := health.tokenAuth.authenticateUserWithTokenReview(tokenReviewCtx, req.ServiceAccountToken)
	if !ok {
		http.Error(w, "invalid service account token", http.StatusUnauthorized)
		return
	}

	// 6. Extract node name from the SA token JWT payload.
	nodeName, err := authn.ExtractNodeNameFromSAToken(req.ServiceAccountToken)
	if err != nil {
		klog.V(2).Infof("Failed to extract node name from SA token: %v", err)
		http.Error(w, "failed to extract node name from token", http.StatusBadRequest)

		return
	}

	if nodeName == "" {
		klog.V(2).Infof("SA token for user %q has no node name claim", username)
		http.Error(w, "service account token is not bound to a node", http.StatusBadRequest)

		return
	}

	// 7. Issue HMAC node token.
	token, expiresAt, err := tokenIssuer.IssueNodeToken(username, nodeName, cfg.nodeTokenLifetime)
	if err != nil {
		klog.Errorf("Failed to issue node token for %q (node %s): %v", username, nodeName, err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	klog.V(3).Infof("Issued node token for %q (node %s, expires %s)", username, nodeName, expiresAt.Format(time.RFC3339))

	// 8. Return JSON response.
	w.Header().Set("Content-Type", "application/json")

	resp := tokenNodeResponse{
		Token:     token,
		ExpiresAt: expiresAt.Format(time.RFC3339),
		NodeName:  nodeName,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		klog.V(4).Infof("Token/node response write failed: %v", err)
	}
}

// handleTokenViewer handles the token/viewer endpoint. It verifies the
// front-proxy cert, performs a SAR, and issues an HMAC viewer token.
func handleTokenViewer(w http.ResponseWriter, r *http.Request, health *healthState, webhookServer *webhookpkg.Server, tokenIssuer *authn.TokenIssuer, cfg tokenEndpointConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Verify front-proxy cert.
	if !webhookServer.IsTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 2. Extract X-Remote-User and X-Remote-Group headers.
	remoteUser := strings.TrimSpace(r.Header.Get("X-Remote-User"))
	if remoteUser == "" {
		http.Error(w, "missing X-Remote-User header", http.StatusBadRequest)
		return
	}

	remoteGroups := r.Header.Values("X-Remote-Group")

	// 3. Perform SAR: can this user create token/viewer?
	if !performSAR(r.Context(), health.clientset, remoteUser, remoteGroups, "create", "token", "viewer") {
		klog.V(2).Infof("Token/viewer SAR denied for user %q", remoteUser)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	// 4. Issue HMAC viewer token (include groups for downstream SAR checks).
	token, expiresAt, err := tokenIssuer.IssueViewerToken(remoteUser, remoteGroups, cfg.viewerTokenLifetime)
	if err != nil {
		klog.Errorf("Failed to issue viewer token for %q: %v", remoteUser, err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	klog.V(3).Infof("Issued viewer token for %q (expires %s)", remoteUser, expiresAt.Format(time.RFC3339))

	// 5. Return JSON response.
	w.Header().Set("Content-Type", "application/json")

	resp := tokenViewerResponse{
		Token:     token,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		klog.V(4).Infof("Token/viewer response write failed: %v", err)
	}
}

// performSAR performs a SubjectAccessReview to check whether the given user
// is authorized to perform the specified action on a token resource in the
// status.net.unbounded-kube.io API group.
func performSAR(ctx context.Context, clientset kubernetes.Interface, username string, groups []string, verb, resource, resourceName string) bool {
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   username,
			Groups: groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     verb,
				Group:    "status.net.unbounded-kube.io",
				Resource: resource,
				Name:     resourceName,
			},
		},
	}

	sarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(sarCtx, review, metav1.CreateOptions{})
	if err != nil {
		klog.V(2).Infof("SAR failed for user %q (verb=%s resource=%s name=%s): %v", username, verb, resource, resourceName, err)
		return false
	}

	allowed := result.Status.Allowed && !result.Status.Denied
	if !allowed {
		reason := result.Status.Reason
		if reason == "" {
			reason = fmt.Sprintf("denied=%v", result.Status.Denied)
		}

		klog.V(3).Infof("SAR denied for user %q (verb=%s resource=%s name=%s): %s", username, verb, resource, resourceName, reason)
	}

	return allowed
}
