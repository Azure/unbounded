// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// Racer cache hits never consult origin, and Racer's cache key is the digest
// alone. Before Racer is read, the handler therefore asks the upstream
// registry whether the caller's exact credential may read this repository and
// digest. Without that check a request with a bogus credential, an anonymous
// request on a registry with a public /v2/ root, or a request under another
// repository path could be served private cached content.
const (
	defaultAccessAllowTTL   = 60 * time.Second
	defaultAccessDenyTTL    = 10 * time.Second
	defaultAccessMaxEntries = 16384
	accessCheckTimeout      = 5 * time.Second
)

// AccessHeader issues an upstream HEAD for ref using only the request-scoped
// delegated Authorization in ctx. origin.Client built with
// WithDelegatedCredentialsOnly satisfies it.
type AccessHeader interface {
	Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error)
}

// accessDecision is the cached outcome of one upstream access check. It never
// contains caller credentials; challenge is the registry's public challenge.
type accessDecision struct {
	status    int // http.StatusOK, 401, 403, 404, or 503
	challenge string
}

type accessEntry struct {
	decision accessDecision
	expires  time.Time
}

type accessChecker struct {
	head       AccessHeader
	allowTTL   time.Duration
	denyTTL    time.Duration
	maxEntries int
	now        func() time.Time
	group      singleflight.Group

	mu      sync.Mutex
	entries map[string]accessEntry
}

func newAccessChecker(head AccessHeader) *accessChecker {
	return &accessChecker{
		head: head, allowTTL: defaultAccessAllowTTL, denyTTL: defaultAccessDenyTTL,
		maxEntries: defaultAccessMaxEntries, now: time.Now, entries: make(map[string]accessEntry),
	}
}

// accessKey hashes the normalized credential together with the resource so the
// raw credential is never retained. Fields are length-delimited by NUL bytes,
// which cannot appear in any of them after validation.
func accessKey(authorization string, ref ifaces.OriginRef) string {
	h := sha256.New()
	for _, part := range []string{authorization, ref.Registry, ref.Repository, strconv.Itoa(int(ref.Kind)), ref.Digest.String()} {
		_, _ = h.Write([]byte(part)) //nolint:errcheck // hash writes cannot fail
		_, _ = h.Write([]byte{0})    //nolint:errcheck // hash writes cannot fail
	}

	return hex.EncodeToString(h.Sum(nil))
}

func (c *accessChecker) lookup(key string) (accessDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return accessDecision{}, false
	}

	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		return accessDecision{}, false
	}

	return e.decision, true
}

func (c *accessChecker) store(key string, d accessDecision) {
	var ttl time.Duration

	switch d.status {
	case http.StatusOK:
		ttl = c.allowTTL
	case http.StatusUnauthorized, http.StatusForbidden:
		ttl = c.denyTTL
	default:
		return
	}

	if ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if _, ok := c.entries[key]; !ok && len(c.entries) >= c.maxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: evict arbitrary entries. Losing one only costs a HEAD.
		for k := range c.entries {
			if len(c.entries) < c.maxEntries {
				break
			}

			delete(c.entries, k)
		}
	}

	c.entries[key] = accessEntry{decision: d, expires: now.Add(ttl)}
}

// check returns the caller's access decision for ref. Concurrent identical
// checks share one upstream HEAD; the shared HEAD is detached from any single
// caller's cancellation but keeps that caller's (identical) credential.
func (c *accessChecker) check(ctx context.Context, ref ifaces.OriginRef) accessDecision {
	authorization := registryauth.Authorization(ctx)
	key := accessKey(authorization, ref)

	if d, ok := c.lookup(key); ok {
		return d
	}

	ch := c.group.DoChan(key, func() (any, error) {
		if d, ok := c.lookup(key); ok {
			return d, nil
		}

		headCtx, cancel := context.WithTimeout(registryauth.WithAuthorization(context.WithoutCancel(ctx), authorization), accessCheckTimeout)
		defer cancel()

		d := classifyAccess(c.head.Head(headCtx, ref))
		c.store(key, d)

		return d, nil
	})

	select {
	case <-ctx.Done():
		return accessDecision{status: http.StatusServiceUnavailable}
	case res := <-ch:
		return res.Val.(accessDecision) //nolint:forcetypeassert // the flight only returns accessDecision
	}
}

func classifyAccess(_ int64, _ string, err error) accessDecision {
	if err == nil {
		return accessDecision{status: http.StatusOK}
	}

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		return accessDecision{status: http.StatusServiceUnavailable}
	}

	switch {
	case oe.StatusCode == http.StatusForbidden:
		return accessDecision{status: http.StatusForbidden}
	case oe.Class == ifaces.FailureAuth:
		return accessDecision{status: http.StatusUnauthorized, challenge: oe.Challenge}
	case oe.Class == ifaces.FailureNotFound:
		return accessDecision{status: http.StatusNotFound}
	default:
		return accessDecision{status: http.StatusServiceUnavailable}
	}
}
