// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	controlAudience    = "racer-control"
	credentialTTL      = 5 * time.Second
	credentialCapacity = 4096
	credentialFlights  = 64
	credentialWaiters  = 64
	credentialTimeout  = time.Second
)

var (
	errInvalidCredential     = errors.New("invalid Pod credential")
	errCredentialUnavailable = errors.New("pod credential review unavailable")
)

func tokenReviewConfig(kube *rest.Config, qps float64, burst int) (*rest.Config, error) {
	if math.IsNaN(qps) || math.IsInf(qps, 0) || qps <= 0 || float32(qps) == 0 || qps > 100000 || burst < 1 || burst > 100000 {
		return nil, errors.New("token-review-qps and token-review-burst must be positive and at most 100000")
	}

	cfg := rest.CopyConfig(kube)
	cfg.QPS, cfg.Burst, cfg.RateLimiter = float32(qps), burst, nil
	cfg.Timeout = credentialTimeout

	return cfg, nil
}

func newTokenReviewClient(cfg *rest.Config, scheme *runtime.Scheme) (client.Client, error) {
	// The only endpoint is fixed; avoid discovery work on the heartbeat deadline.
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{authenticationv1.SchemeGroupVersion})
	mapper.Add(authenticationv1.SchemeGroupVersion.WithKind("TokenReview"), meta.RESTScopeRoot)

	return client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper})
}

// Only digests and a bounded Pod UID survive a review; never retain bearer tokens.
type credentialKey struct {
	token, audience [32]byte
}
type credentialEntry struct {
	key    credentialKey
	podUID string
	until  time.Time
	expiry time.Time
}
type credentialFlight struct {
	done    chan struct{}
	waiters int
	podUID  string
	err     error
	expiry  time.Time
	until   time.Time
}
type credentialCache struct {
	mu      sync.Mutex
	entries map[credentialKey]*list.Element
	lru     list.List
	flights map[credentialKey]*credentialFlight
	now     func() time.Time // Fixed before serving; nil uses the real clock.
}

func (c *credentialCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}

	return time.Now()
}

// TokenReview supplies no expiry. Unverified claims NEVER authenticate a token:
// a usable exp can only shorten a successful review's five-second lifetime.
// Unknown/malformed expiry means no completed-result caching at all.
func credentialExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}

	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return time.Time{}
	}

	seconds, err := claims.Exp.Int64()
	// Keep time arithmetic within a conservative supported range (year 9999).
	if err != nil || seconds > 253402300799 {
		return time.Time{}
	}

	if seconds <= 0 {
		return time.Unix(0, 0)
	}

	return time.Unix(seconds, 0)
}

func (c *credentialCache) authenticate(ctx context.Context, kube client.Client, token, audience string) (string, error) {
	if token == "" || len(token) > 16384 || audience == "" || len(audience) > 256 {
		return "", errInvalidCredential
	}

	ctx, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()

	key := credentialKey{sha256.Sum256([]byte(token)), sha256.Sum256([]byte(audience))}

	c.mu.Lock()

	now := c.clock()
	if e := c.entries[key]; e != nil {
		entry, ok := e.Value.(credentialEntry)
		if !ok {
			c.mu.Unlock()
			return "", errCredentialUnavailable
		}

		if now.Before(entry.until) && now.Before(entry.expiry) {
			c.lru.MoveToFront(e)
			c.mu.Unlock()

			return entry.podUID, nil
		}

		delete(c.entries, key)
		c.lru.Remove(e)
	}

	if f := c.flights[key]; f != nil {
		if f.waiters >= credentialWaiters {
			c.mu.Unlock()
			return "", errCredentialUnavailable
		}

		f.waiters++
		c.mu.Unlock()

		defer func() { c.mu.Lock(); f.waiters--; c.mu.Unlock() }()

		select {
		case <-ctx.Done():
			return "", errCredentialUnavailable
		case <-f.done:
			if ctx.Err() != nil {
				return "", errCredentialUnavailable
			}

			if f.err == nil && !c.clock().Before(f.until) {
				return "", errCredentialUnavailable
			}

			if f.err == nil && !f.expiry.IsZero() && !c.clock().Before(f.expiry) {
				return "", errInvalidCredential
			}

			return f.podUID, f.err
		}
	}

	if len(c.flights) >= credentialFlights {
		c.mu.Unlock()
		return "", errCredentialUnavailable
	}

	if c.flights == nil {
		c.flights = make(map[credentialKey]*credentialFlight)
		c.entries = make(map[credentialKey]*list.Element)
	}

	f := &credentialFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	expiry := credentialExpiry(token)

	podUID, err := reviewCredential(ctx, kube, token, audience)
	if ctx.Err() != nil {
		podUID, err = "", errCredentialUnavailable
	}

	if err == nil && !expiry.IsZero() && !c.clock().Before(expiry) {
		podUID, err = "", errInvalidCredential
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Keep the monotonic TTL separate from JWT wall-clock expiry: a clock step
	// backwards must not extend the revocation window of a near-expiry token.
	until := now.Add(credentialTTL) // API latency cannot extend the revocation window.
	if err == nil && c.clock().Before(until) && c.clock().Before(expiry) {
		if c.lru.Len() == credentialCapacity {
			old := c.lru.Back()

			entry, ok := old.Value.(credentialEntry)
			if !ok {
				panic("invalid credential cache entry")
			}

			delete(c.entries, entry.key)
			c.lru.Remove(old)
		}

		c.entries[key] = c.lru.PushFront(credentialEntry{key, podUID, until, expiry})
	}

	f.podUID, f.err, f.expiry, f.until = podUID, err, expiry, until

	delete(c.flights, key)
	close(f.done)

	return podUID, err
}

func reviewCredential(ctx context.Context, kube client.Client, token, audience string) (string, error) {
	review := &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}}
	if err := kube.Create(ctx, review); err != nil || review.Status.Error != "" {
		return "", errCredentialUnavailable
	}

	acceptedAudience := false
	for _, a := range review.Status.Audiences {
		acceptedAudience = acceptedAudience || a == audience
	}

	uid := review.Status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if !review.Status.Authenticated || !acceptedAudience || len(uid) != 1 || uid[0] == "" || len(uid[0]) > 256 {
		return "", errInvalidCredential
	}

	return uid[0], nil
}
