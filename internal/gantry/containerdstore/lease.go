// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Lease management for containerd-mode storage.
//
// Background : when Gantry
// ingests bytes into containerd's content store, the bytes need a lease
// before/during commit because no kubelet Image reference exists yet.
// Kubelet-pulled content is protected because kubelet creates an Image
// resource for it; Gantry-pulled content has no Image, so absent a lease
// containerd may GC the blob the next time its periodic GC runs.
//
// This file adds two pieces:
//
// 1. CreateLease(ctx, d, source, repo): creates a containerd lease
// before ingest, bound to d's content resource with a configurable TTL
// plus the
// plan-mandated labels:
// gantry.io/managed=true
// gantry.io/source=<registry>
// gantry.io/repository=<repo>
// gantry.io/digest=<digest>
// containerd.io/gc.expire=<RFC3339 timestamp>
// The `containerd.io/gc.expire` label is what containerd's
// lease manager recognizes for TTL expiration - leases.
// WithExpiration sets it under the hood. The returned guard is
// released on failed ingest; AttachLease is retained as a compatibility
// wrapper for tests and older call sites.
//
// 2. CleanupExpiredLeases(ctx): lists every gantry-managed lease,
// deletes those past their expiry, and returns the count of
// leases removed. Containerd's own GC will then collect any
// content the deleted leases were the last reference to.
//
// The Manager interface is the containerd-shipped leases.Manager -
// the same one production wires via client.LeasesService. Tests
// supply a fake manager that records calls in memory.

package containerdstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"
)

// Plan-mandated label keys for Gantry-owned leases.
const (
	// LabelManaged identifies leases created by Gantry. Used as the
	// list filter for cleanup so we never touch leases owned by
	// kubelet or other system components.
	LabelManaged = "gantry.io/managed"
	// LabelSource records the upstream registry hostname the digest
	// was pulled from (e.g. "registry.example.com").
	LabelSource = "gantry.io/source"
	// LabelRepository records the OCI repository the digest was
	// pulled from (e.g. "library/nginx").
	LabelRepository = "gantry.io/repository"
	// LabelDigest records the digest the lease protects. Redundant
	// with the resource binding but useful for log/metric extraction
	// without an extra ListResources call.
	LabelDigest = "gantry.io/digest"
	// LabelCreated records the RFC3339 creation timestamp. Used by
	// CleanupExpiredLeases when the containerd gc.expire label is
	// missing (e.g. legacy leases from earlier agent versions).
	LabelCreated = "gantry.io/created"
)

// DefaultLeaseTTL is the lease lifetime used when WithLeaseTTL is
// not specified. 60 minutes is the midpoint of the plan-specified
// 30–120 minute range and large enough to absorb pull-then-deploy
// latency on slow nodes without keeping unused content alive
// indefinitely.
const DefaultLeaseTTL = 60 * time.Minute

// LeasePrefix is prepended to every lease ID so list-by-prefix
// filtering can identify gantry-owned leases without parsing labels.
const LeasePrefix = "gantry-"

// LeaseManager is the subset of containerd's leases.Manager interface
// the Store consumes. Defined here (instead of importing the concrete
// containerd type at the call site) so tests can substitute a fake.
type LeaseManager interface {
	Create(ctx context.Context, opts ...leases.Opt) (leases.Lease, error)
	Delete(ctx context.Context, l leases.Lease, opts ...leases.DeleteOpt) error
	List(ctx context.Context, filters ...string) ([]leases.Lease, error)
	AddResource(ctx context.Context, l leases.Lease, r leases.Resource) error
}

// WithLeaseManager wires the containerd lease manager into the Store
// so AttachLease and CleanupExpiredLeases can run. Without this option
// both methods return ErrNoLeaseManager - tests or dev-only wiring
// may omit it; production containerd-only wiring always sets it.
func WithLeaseManager(m LeaseManager) Option {
	return func(s *Store) { s.leases = m }
}

// WithLeaseTTL overrides the default lease TTL. Zero or negative
// leaves the default unchanged.
func WithLeaseTTL(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.leaseTTL = d
		}
	}
}

// ErrNoLeaseManager is returned by AttachLease and CleanupExpiredLeases
// when the Store was not constructed with WithLeaseManager.
var ErrNoLeaseManager = fmt.Errorf("containerdstore: lease manager not configured")

// LeaseGuard is returned by CreateLease. Release deletes the lease; callers
// use it on failed ingest so an aborted background pull does not leave an
// empty Gantry-owned lease behind until TTL cleanup.
type LeaseGuard struct {
	store *Store
	lease leases.Lease
}

// Release deletes the lease synchronously. Safe to call on a nil guard
// (no-op), which lets callers defer Release without a guard check.
func (g *LeaseGuard) Release(ctx context.Context) error {
	if g == nil || g.store == nil {
		return nil
	}

	return g.store.leases.Delete(g.store.withNS(ctx), g.lease, leases.SynchronousDelete)
}

// CreateLease creates a containerd lease that keeps d alive for the
// configured TTL and returns a guard the caller can release on failed
// ingest. It is safe to call before content commit: the content resource
// binding names the digest that will be committed, so containerd GC sees
// the intended protection as soon as the content appears.
//
// source is the upstream registry hostname; repository is the OCI
// repository path. Both are stamped as labels so the lease catalog
// is self-describing without cross-referencing inflight state.
func (s *Store) CreateLease(ctx context.Context, d gdigest.Digest, source, repository string) (*LeaseGuard, error) {
	if s.leases == nil {
		return nil, ErrNoLeaseManager
	}

	ctx = s.withNS(ctx)
	leaseID := LeasePrefix + d.String() + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	labels := map[string]string{
		LabelManaged:    "true",
		LabelSource:     source,
		LabelRepository: repository,
		LabelDigest:     d.String(),
		LabelCreated:    time.Now().UTC().Format(time.RFC3339),
	}

	lease, err := s.leases.Create(ctx,
		leases.WithID(leaseID),
		leases.WithLabels(labels),
		leases.WithExpiration(s.leaseTTL),
	)
	if err != nil {
		return nil, fmt.Errorf("containerdstore: leases.Create: %w", err)
	}

	res := leases.Resource{
		ID:   d.String(),
		Type: "content",
	}
	if err := s.leases.AddResource(ctx, lease, res); err != nil {
		// Roll back the lease - without a resource binding it
		// protects nothing and would still consume a slot.
		_ = s.leases.Delete(ctx, lease) //nolint:errcheck // best-effort
		return nil, fmt.Errorf("containerdstore: leases.AddResource: %w", err)
	}

	return &LeaseGuard{store: s, lease: lease}, nil
}

// AttachLease creates a containerd lease that keeps d alive for the
// configured TTL. The returned LeaseGuard is intentionally discarded
// because the caller does not need to release this lease early - it
// will expire via TTL. Idempotent at the digest level: a digest may be
// referenced by multiple leases without breaking anything, but
// gantry uses one lease per Commit to keep cleanup arithmetic simple.
func (s *Store) AttachLease(ctx context.Context, d gdigest.Digest, source, repository string) error {
	_, err := s.CreateLease(ctx, d, source, repository)
	return err
}

// CleanupExpiredLeases removes every Gantry-managed lease whose
// creation timestamp + configured TTL is in the past, returning the
// number of leases deleted. Used by a periodic background task in
// cmd/gantry to keep the lease catalog from growing unbounded.
//
// We rely on our own LabelCreated rather than parsing containerd's
// containerd.io/gc.expire because the latter is internal-format and
// not exposed in the leases.Lease.Labels map after creation in some
// containerd versions. Lease.CreatedAt + Store.leaseTTL is the
// authoritative computation.
func (s *Store) CleanupExpiredLeases(ctx context.Context) (int, error) {
	if s.leases == nil {
		return 0, ErrNoLeaseManager
	}

	ctx = s.withNS(ctx)

	ls, err := s.leases.List(ctx, managedLeaseFilter())
	if err != nil {
		return 0, fmt.Errorf("containerdstore: leases.List: %w", err)
	}

	now := time.Now()
	deleted := 0

	var lastErr error

	for _, l := range ls {
		if !s.isExpired(l, now) {
			continue
		}

		if err := s.leases.Delete(ctx, l, leases.SynchronousDelete); err != nil {
			if !errors.Is(err, cerrdefs.ErrNotFound) {
				lastErr = err
			}
			// One lease failing shouldn't block cleanup of others.
			continue
		}

		deleted++
	}

	return deleted, lastErr
}

// isExpired returns true when the lease was created more than
// s.leaseTTL ago. Falls back to LabelCreated when CreatedAt is the
// zero value (some older containerd lease backends don't populate it).
func (s *Store) isExpired(l leases.Lease, now time.Time) bool {
	created := l.CreatedAt
	if created.IsZero() {
		if ts, ok := l.Labels[LabelCreated]; ok {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				created = parsed
			}
		}
	}

	if created.IsZero() {
		// No timestamp available - treat as fresh to avoid deleting
		// leases we can't safely reason about. The lease's own
		// containerd.io/gc.expire label will eventually expire it.
		return false
	}

	return now.After(created.Add(s.leaseTTL))
}

// LeaseTTL returns the configured lease TTL. Used by cmd/gantry to
// schedule the cleanup interval as a function of the TTL.
func (s *Store) LeaseTTL() time.Duration {
	return s.leaseTTL
}

// ListManagedLeases returns the set of Gantry-managed leases currently
// live in containerd, filtered by the LabelManaged=true label. Used by
// the the gantry_containerd_lease_active gauge sampler. Returns
// ErrNoLeaseManager when this Store was built without WithLeaseManager.
//
// This is a best-effort snapshot - the catalog may have changed by
// the time the caller observes the slice. Callers that need exact
// counts should rely on the counters (created/released) instead.
func (s *Store) ListManagedLeases(ctx context.Context) ([]leases.Lease, error) {
	if s.leases == nil {
		return nil, ErrNoLeaseManager
	}

	ctx = s.withNS(ctx)

	ls, err := s.leases.List(ctx, managedLeaseFilter())
	if err != nil {
		return nil, fmt.Errorf("containerdstore: leases.List: %w", err)
	}

	return ls, nil
}

// managedLeaseFilter returns the containerd lease filter expression
// that selects every Gantry-managed lease. The label key contains a
// slash and a dot (`gantry.io/managed`), both of which are filter
// metacharacters in containerd's filter parser; the key must be
// double-quoted so the parser treats it as a literal map key rather
// than as a nested-field access.
func managedLeaseFilter() string {
	return `labels."` + LabelManaged + `"=="true"`
}
