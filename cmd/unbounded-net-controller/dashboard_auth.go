// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/authn"
	webhookpkg "github.com/Azure/unbounded/internal/net/webhook"
)

const (
	// sarCacheTTL is the time-to-live for SubjectAccessReview cache entries.
	sarCacheTTL = 5 * time.Minute
	// maxSARCacheSize is the maximum number of entries in the SAR cache.
	// When exceeded, expired entries are evicted first, then least-recently-used.
	maxSARCacheSize = 1000
)

// sarCacheEntry holds a cached SubjectAccessReview result.
type sarCacheEntry struct {
	allowed    bool
	expiry     time.Time
	lastAccess time.Time
}

// sarCache provides a thread-safe cache for SubjectAccessReview results.
type sarCache struct {
	mu      sync.Mutex
	entries map[string]sarCacheEntry
}

// dashboardAuthorizer checks whether a user is authorized to access dashboard
// endpoints by performing SubjectAccessReview requests against the Kubernetes
// API, with caching to minimize API server load.
type dashboardAuthorizer struct {
	clientset kubernetes.Interface
	cache     sarCache
}

// newDashboardAuthorizer creates a new dashboardAuthorizer that uses the given
// clientset to perform SubjectAccessReview requests.
func newDashboardAuthorizer(clientset kubernetes.Interface) *dashboardAuthorizer {
	return &dashboardAuthorizer{
		clientset: clientset,
		cache: sarCache{
			entries: make(map[string]sarCacheEntry),
		},
	}
}

// authorize checks whether the given username (with optional groups) is
// allowed to access the dashboard. Results are cached with a 5-minute TTL
// to avoid repeated SubjectAccessReview API calls.
func (da *dashboardAuthorizer) authorize(ctx context.Context, username string, groups []string) bool {
	da.cache.mu.Lock()
	if entry, ok := da.cache.entries[username]; ok && time.Now().Before(entry.expiry) {
		entry.lastAccess = time.Now()
		da.cache.entries[username] = entry
		da.cache.mu.Unlock()

		return entry.allowed
	}
	da.cache.mu.Unlock()

	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   username,
			Groups: groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     "get",
				Group:    "status.net.unbounded-kube.io",
				Resource: "status",
				Name:     "dashboard",
			},
		},
	}

	result, err := da.clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		klog.V(2).Infof("SubjectAccessReview failed for user %q: %v", username, err)
		return false
	}

	allowed := result.Status.Allowed && !result.Status.Denied

	now := time.Now()

	da.cache.mu.Lock()

	da.cache.entries[username] = sarCacheEntry{
		allowed:    allowed,
		expiry:     now.Add(sarCacheTTL),
		lastAccess: now,
	}
	if len(da.cache.entries) > maxSARCacheSize {
		da.evictCacheLocked(now)
	}
	da.cache.mu.Unlock()

	if !allowed {
		klog.V(2).Infof("Dashboard access denied for user %q", username)
	}

	return allowed
}

// evictCacheLocked removes expired entries first, then evicts the
// least-recently-used entries until the cache is within bounds.
// Caller must hold da.cache.mu.
func (da *dashboardAuthorizer) evictCacheLocked(now time.Time) {
	// First pass: remove expired entries.
	for k, v := range da.cache.entries {
		if now.After(v.expiry) {
			delete(da.cache.entries, k)
		}
	}
	// If still over limit, evict least-recently-used entries.
	for len(da.cache.entries) > maxSARCacheSize {
		var (
			oldestKey    string
			oldestAccess time.Time
		)

		first := true
		for k, v := range da.cache.entries {
			if first || v.lastAccess.Before(oldestAccess) {
				oldestKey = k
				oldestAccess = v.lastAccess
				first = false
			}
		}

		delete(da.cache.entries, oldestKey)
	}
}

// authorizeDashboardRequest checks whether the request is authorized to access
// dashboard endpoints. When requireDashboardAuth is false, all requests are
// allowed. Otherwise, the bearer token is validated as an HMAC viewer token
// and a SubjectAccessReview is performed to check for the
// status.net.unbounded-kube.io/status "get" permission on the "dashboard" resource.
func authorizeDashboardRequest(requireDashboardAuth bool, tokenIssuer *authn.TokenIssuer, authorizer *dashboardAuthorizer, r *http.Request) bool {
	if !requireDashboardAuth {
		return true
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	claims, err := tokenIssuer.Validate(token)
	if err != nil {
		klog.V(3).Infof("Dashboard HMAC token validation failed: %v", err)
		return false
	}

	if claims.Role != authn.RoleViewer {
		klog.V(3).Infof("Dashboard access rejected: token role %q is not viewer", claims.Role)
		return false
	}

	return authorizer.authorize(r.Context(), claims.Subject, claims.Groups)
}

// authorizeDashboardOrAggregated allows a request if it arrives via the
// aggregated API server (verified front-proxy client certificate) or if it
// passes dashboard authentication and authorization.
func authorizeDashboardOrAggregated(
	requireDashboardAuth bool,
	tokenIssuer *authn.TokenIssuer,
	authorizer *dashboardAuthorizer,
	webhookServer *webhookpkg.Server,
	r *http.Request,
) bool {
	if webhookServer.IsTrustedAggregatedRequest(r) {
		return true
	}

	return authorizeDashboardRequest(requireDashboardAuth, tokenIssuer, authorizer, r)
}
