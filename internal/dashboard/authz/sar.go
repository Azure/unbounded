// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package authz

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

	"github.com/Azure/unbounded/internal/dashboard/contract"
)

const (
	// sarCacheTTL bounds how long a SubjectAccessReview result is cached.
	sarCacheTTL = 5 * time.Minute
	// maxSARCacheSize caps the SAR cache before LRU/expiry eviction kicks in.
	maxSARCacheSize = 4096
)

// SubjectAccessReview is an Authorizer backed by the Kubernetes
// SubjectAccessReview API, with a small TTL cache to limit API server load.
//
// For the prototype the caller identity is taken from trusted reverse-proxy
// headers (X-Remote-User / X-Remote-Group), matching how the kube-aggregator
// front-proxy presents identity. Token issuance and validation (the HMAC
// viewer token path from the net dashboard) is deliberately left for a later
// milestone; the interface does not change when it is added.
type SubjectAccessReview struct {
	clientset kubernetes.Interface

	mu    sync.Mutex
	cache map[string]sarEntry
}

type sarEntry struct {
	allowed    bool
	expiry     time.Time
	lastAccess time.Time
}

// NewSubjectAccessReview returns a SAR-backed authorizer using the given
// clientset.
func NewSubjectAccessReview(clientset kubernetes.Interface) *SubjectAccessReview {
	return &SubjectAccessReview{
		clientset: clientset,
		cache:     make(map[string]sarEntry),
	}
}

// Subject reads the caller identity from trusted front-proxy headers.
func (s *SubjectAccessReview) Subject(r *http.Request) (Subject, bool) {
	user := strings.TrimSpace(r.Header.Get("X-Remote-User"))
	if user == "" {
		return Subject{}, false
	}

	return Subject{User: user, Groups: r.Header.Values("X-Remote-Group")}, true
}

// Allowed performs (or serves from cache) a SubjectAccessReview for the
// permission. A nil permission is allowed for any authenticated subject.
func (s *SubjectAccessReview) Allowed(ctx context.Context, sub Subject, perm *contract.Permission) bool {
	if perm == nil {
		return sub.User != ""
	}

	key := cacheKey(sub, perm)

	s.mu.Lock()

	if entry, ok := s.cache[key]; ok && time.Now().Before(entry.expiry) {
		entry.lastAccess = time.Now()
		s.cache[key] = entry
		s.mu.Unlock()

		return entry.allowed
	}

	s.mu.Unlock()

	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   sub.User,
			Groups: sub.Groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     perm.Verb,
				Group:    perm.APIGroup,
				Resource: perm.Resource,
				Name:     perm.Name,
			},
		},
	}

	result, err := s.clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		klog.V(2).Infof("dashboard SubjectAccessReview failed for user %q: %v", sub.User, err)
		return false
	}

	allowed := result.Status.Allowed && !result.Status.Denied

	s.store(key, allowed)

	return allowed
}

func (s *SubjectAccessReview) store(key string, allowed bool) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cache[key] = sarEntry{allowed: allowed, expiry: now.Add(sarCacheTTL), lastAccess: now}

	if len(s.cache) > maxSARCacheSize {
		s.evictLocked(now)
	}
}

// evictLocked removes expired entries first, then evicts least-recently-used
// entries until the cache is within bounds. Caller must hold s.mu.
func (s *SubjectAccessReview) evictLocked(now time.Time) {
	for k, v := range s.cache {
		if now.After(v.expiry) {
			delete(s.cache, k)
		}
	}

	for len(s.cache) > maxSARCacheSize {
		var (
			oldestKey    string
			oldestAccess time.Time
			first        = true
		)

		for k, v := range s.cache {
			if first || v.lastAccess.Before(oldestAccess) {
				oldestKey = k
				oldestAccess = v.lastAccess
				first = false
			}
		}

		delete(s.cache, oldestKey)
	}
}

func cacheKey(sub Subject, perm *contract.Permission) string {
	return strings.Join([]string{
		sub.User,
		strings.Join(sub.Groups, ","),
		perm.Verb,
		perm.APIGroup,
		perm.Resource,
		perm.Name,
	}, "|")
}
