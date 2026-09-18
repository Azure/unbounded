// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type tokenAuthResult struct {
	authenticated bool
	username      string
	expiresAt     time.Time
	lastAccess    time.Time
}

// maxTokenCacheSize is the maximum number of entries in the token cache.
// When exceeded, expired entries are evicted first, then least-recently-used.
const maxTokenCacheSize = 500

// tokenAuthenticator verifies bearer tokens with caching.
type tokenAuthenticator struct {
	mu             sync.RWMutex
	cache          map[string]*tokenAuthResult
	cacheTTL       time.Duration
	allowedSANames map[string]bool
	verifier       serviceAccountTokenVerifier
	configured     bool
}

// newTokenAuthenticator creates a new token authenticator that accepts tokens from
// the specified service accounts. If allowedSAs is empty, any authenticated SA is accepted.
func newTokenAuthenticator(verifier serviceAccountTokenVerifier, allowedSAs []string) *tokenAuthenticator {
	allowed := make(map[string]bool, len(allowedSAs))
	for _, sa := range allowedSAs {
		allowed[sa] = true
	}

	return &tokenAuthenticator{
		cache:          make(map[string]*tokenAuthResult),
		cacheTTL:       5 * time.Minute,
		allowedSANames: allowed,
		verifier:       verifier,
		configured:     verifier != nil,
	}
}

// authenticate checks if the request has a valid bearer token from an allowed service account.
// Results are cached to avoid repeated authentication work.
func (a *tokenAuthenticator) authenticate(r *http.Request) bool {
	username, ok := a.authenticateUser(r)
	if !ok {
		return false
	}

	if len(a.allowedSANames) == 0 {
		return true
	}

	saID, ok := serviceAccountIDFromUsername(username)
	if !ok {
		klog.V(2).Infof("Token authenticated but username %q is not a service account", username)
		return false
	}

	if !a.allowedSANames[saID] {
		klog.V(2).Infof("Token authenticated but SA %s not in allowed list", saID)
		return false
	}

	return true
}

// authenticateUser validates the bearer token and returns the
// authenticated username. Unlike authenticate(), it does not check the SA
// allowlist. Returns ("", false) if the token is missing or invalid.
func (a *tokenAuthenticator) authenticateUser(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return "", false
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	// Check cache first.
	a.mu.RLock()

	if result, ok := a.cache[token]; ok && time.Now().Before(result.expiresAt) {
		result.lastAccess = time.Now()

		a.mu.RUnlock()

		if result.authenticated {
			return result.username, true
		}

		return "", false
	}

	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		username      string
		authenticated bool
	)

	if a.verifier != nil {
		identity, err := a.verifier.Verify(ctx, token)
		if err != nil {
			klog.V(2).Infof("Token authentication failed: %v", err)
		} else {
			username = identity.Subject
			authenticated = true
		}
	}

	// Cache the result with LRU eviction.
	now := time.Now()

	a.mu.Lock()

	a.cache[token] = &tokenAuthResult{
		authenticated: authenticated,
		username:      username,
		expiresAt:     now.Add(a.cacheTTL),
		lastAccess:    now,
	}
	if len(a.cache) > maxTokenCacheSize {
		a.evictCacheLocked(now)
	}
	a.mu.Unlock()

	if authenticated {
		return username, true
	}

	return "", false
}

// evictCacheLocked removes expired entries first, then evicts the
// least-recently-used entries until the cache is within bounds.
// Caller must hold a.mu write lock.
func (a *tokenAuthenticator) evictCacheLocked(now time.Time) {
	// First pass: remove expired entries.
	for k, v := range a.cache {
		if now.After(v.expiresAt) {
			delete(a.cache, k)
		}
	}
	// If still over limit, evict least-recently-used entries.
	for len(a.cache) > maxTokenCacheSize {
		var (
			oldestKey    string
			oldestAccess time.Time
		)

		first := true
		for k, v := range a.cache {
			if first || v.lastAccess.Before(oldestAccess) {
				oldestKey = k
				oldestAccess = v.lastAccess
				first = false
			}
		}

		delete(a.cache, oldestKey)
	}
}

func serviceAccountIDFromUsername(username string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(username), ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", false
	}

	if parts[2] == "" || parts[3] == "" {
		return "", false
	}

	return parts[2] + ":" + parts[3], true
}
