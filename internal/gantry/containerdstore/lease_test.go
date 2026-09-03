// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Tests for lease lifecycle in containerdstore.
//
// The fake LeaseManager records every Create/AddResource/Delete/List
// call so tests can assert on exact lease topology rather than indirect
// side-effects. CreatedAt is set explicitly by tests that need to
// exercise the expiration path (the real containerd backend would
// populate it on Create, but our fake leaves that to the test author
// so each scenario controls its clock).

package containerdstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"
)

// fakeLeases is a recording leases.Manager that supports the subset of
// operations Store exercises. It is intentionally tiny - no concurrency
// safety, no real expiration semantics - because the production
// implementation defers all of that to containerd itself.
type fakeLeases struct {
	created    []leases.Lease
	resources  map[string][]leases.Resource
	deleted    []string
	listResult []leases.Lease
	failCreate error
	failAdd    error
	failDelete error
	failList   error
}

func newFakeLeases() *fakeLeases {
	return &fakeLeases{
		resources: map[string][]leases.Resource{},
	}
}

func (f *fakeLeases) Create(ctx context.Context, opts ...leases.Opt) (leases.Lease, error) {
	if f.failCreate != nil {
		return leases.Lease{}, f.failCreate
	}

	var l leases.Lease

	l.Labels = map[string]string{}
	for _, opt := range opts {
		if err := opt(&l); err != nil {
			return leases.Lease{}, err
		}
	}

	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}

	f.created = append(f.created, l)

	return l, nil
}

func (f *fakeLeases) Delete(ctx context.Context, l leases.Lease, opts ...leases.DeleteOpt) error {
	if f.failDelete != nil {
		return f.failDelete
	}

	f.deleted = append(f.deleted, l.ID)

	return nil
}

func (f *fakeLeases) List(ctx context.Context, filters ...string) ([]leases.Lease, error) {
	if f.failList != nil {
		return nil, f.failList
	}

	return f.listResult, nil
}

func (f *fakeLeases) AddResource(ctx context.Context, l leases.Lease, r leases.Resource) error {
	if f.failAdd != nil {
		return f.failAdd
	}

	f.resources[l.ID] = append(f.resources[l.ID], r)

	return nil
}

// mustDigest constructs a sha256 gdigest for the test payload "x"
// repeated to look like a real digest. We use the same constant across
// tests so output comparisons remain stable.
func mustDigestFor(t *testing.T, hex string) gdigest.Digest {
	t.Helper()

	d, err := gdigest.Parse("sha256:" + hex)
	if err != nil {
		t.Fatalf("parse digest: %v", err)
	}

	return d
}

func TestAttachLease_CreatesLeaseWithLabelsAndResource(t *testing.T) {
	fl := newFakeLeases()
	fs := &fakeStore{}
	s := New(fs, WithLeaseManager(fl), WithLeaseTTL(30*time.Minute))

	d := mustDigestFor(t, "1111111111111111111111111111111111111111111111111111111111111111")
	if err := s.AttachLease(context.Background(), d, "registry.example.com", "library/nginx"); err != nil {
		t.Fatalf("AttachLease: %v", err)
	}

	if len(fl.created) != 1 {
		t.Fatalf("expected 1 lease created, got %d", len(fl.created))
	}

	got := fl.created[0]
	if got.Labels[LabelManaged] != "true" {
		t.Errorf("LabelManaged = %q, want true", got.Labels[LabelManaged])
	}

	if got.Labels[LabelSource] != "registry.example.com" {
		t.Errorf("LabelSource = %q", got.Labels[LabelSource])
	}

	if got.Labels[LabelRepository] != "library/nginx" {
		t.Errorf("LabelRepository = %q", got.Labels[LabelRepository])
	}

	if got.Labels[LabelDigest] != d.String() {
		t.Errorf("LabelDigest = %q", got.Labels[LabelDigest])
	}

	if got.Labels[LabelCreated] == "" {
		t.Error("LabelCreated empty")
	}

	if _, ok := got.Labels["containerd.io/gc.expire"]; !ok {
		t.Error("expected containerd.io/gc.expire label set by leases.WithExpiration")
	}

	res, ok := fl.resources[got.ID]
	if !ok || len(res) != 1 {
		t.Fatalf("expected 1 resource on lease, got %v", res)
	}

	if res[0].Type != "content" || res[0].ID != d.String() {
		t.Errorf("resource = %+v", res[0])
	}
}

func TestAttachLease_RollsBackOnAddResourceFailure(t *testing.T) {
	fl := newFakeLeases()
	fl.failAdd = errors.New("backend down")
	fs := &fakeStore{}
	s := New(fs, WithLeaseManager(fl))

	d := mustDigestFor(t, "2222222222222222222222222222222222222222222222222222222222222222")
	if err := s.AttachLease(context.Background(), d, "r", "p"); err == nil {
		t.Fatal("expected error")
	}
	// Created the lease, then deleted it on failure.
	if len(fl.created) != 1 {
		t.Fatalf("created leases = %d", len(fl.created))
	}

	if len(fl.deleted) != 1 || fl.deleted[0] != fl.created[0].ID {
		t.Errorf("expected rollback delete of %s, got %v", fl.created[0].ID, fl.deleted)
	}
}

func TestAttachLease_NoLeaseManagerReturnsSentinel(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs) // no WithLeaseManager

	d := mustDigestFor(t, "3333333333333333333333333333333333333333333333333333333333333333")
	if err := s.AttachLease(context.Background(), d, "r", "p"); !errors.Is(err, ErrNoLeaseManager) {
		t.Errorf("err = %v, want ErrNoLeaseManager", err)
	}
}

func TestCleanupExpiredLeases_DeletesExpiredKeepsFresh(t *testing.T) {
	fl := newFakeLeases()
	ttl := 30 * time.Minute
	now := time.Now()
	fl.listResult = []leases.Lease{
		{ID: "gantry-old", CreatedAt: now.Add(-2 * ttl), Labels: map[string]string{LabelManaged: "true"}},
		{ID: "gantry-fresh", CreatedAt: now.Add(-1 * time.Minute), Labels: map[string]string{LabelManaged: "true"}},
		{ID: "gantry-borderline", CreatedAt: now.Add(-ttl - time.Second), Labels: map[string]string{LabelManaged: "true"}},
	}
	fs := &fakeStore{}
	s := New(fs, WithLeaseManager(fl), WithLeaseTTL(ttl))

	deleted, err := s.CleanupExpiredLeases(context.Background())
	if err != nil {
		t.Fatalf("CleanupExpiredLeases: %v", err)
	}

	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	if len(fl.deleted) != 2 {
		t.Fatalf("backend delete calls = %v", fl.deleted)
	}
	// Verify the fresh one was NOT deleted.
	for _, id := range fl.deleted {
		if id == "gantry-fresh" {
			t.Error("fresh lease was deleted")
		}
	}
}

func TestCleanupExpiredLeases_NotFoundOnDeleteIsTolerated(t *testing.T) {
	fl := newFakeLeases()
	fl.failDelete = cerrdefs.ErrNotFound
	fl.listResult = []leases.Lease{
		{ID: "gantry-1", CreatedAt: time.Now().Add(-2 * time.Hour), Labels: map[string]string{LabelManaged: "true"}},
	}
	fs := &fakeStore{}
	s := New(fs, WithLeaseManager(fl), WithLeaseTTL(time.Minute))
	// A NotFound on delete means another process raced us - should be
	// silently absorbed (no error, but also not counted as a deletion
	// since we didn't actually delete it).
	deleted, err := s.CleanupExpiredLeases(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}

	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (NotFound skipped)", deleted)
	}
}

func TestCleanupExpiredLeases_FallsBackToLabelCreatedWhenCreatedAtZero(t *testing.T) {
	fl := newFakeLeases()
	old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	fl.listResult = []leases.Lease{
		{ID: "gantry-labelonly", Labels: map[string]string{
			LabelManaged: "true",
			LabelCreated: old,
		}},
	}
	fs := &fakeStore{}
	s := New(fs, WithLeaseManager(fl), WithLeaseTTL(30*time.Minute))

	deleted, err := s.CleanupExpiredLeases(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}

	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
}

func TestCleanupExpiredLeases_ListErrorBubbles(t *testing.T) {
	fl := newFakeLeases()
	fl.failList = errors.New("backend down")
	fs := &fakeStore{}

	s := New(fs, WithLeaseManager(fl))
	if _, err := s.CleanupExpiredLeases(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestLeaseTTL_DefaultsTo60m(t *testing.T) {
	fs := &fakeStore{}

	s := New(fs)
	if s.LeaseTTL() != DefaultLeaseTTL {
		t.Errorf("default TTL = %v, want %v", s.LeaseTTL(), DefaultLeaseTTL)
	}
}
