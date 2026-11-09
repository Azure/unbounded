// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package kubelease implements the cachemesh election Store over a
// Kubernetes coordination/v1 Lease using typed client-go.
//
// # Mapping
//
// control.Record fields map onto the Lease as follows:
//
//	Holder          spec.holderIdentity
//	LeaseDuration   spec.leaseDurationSeconds (rounded up to whole seconds)
//	AcquireTime     spec.acquireTime
//	RenewTime       spec.renewTime
//	Transitions     spec.leaseTransitions
//	Incarnation     annotation cachemesh.unbounded-cloud.io/incarnation (decimal)
//	Term            annotation cachemesh.unbounded-cloud.io/term (decimal)
//	Addr            annotation cachemesh.unbounded-cloud.io/addr
//	Seeds           annotation cachemesh.unbounded-cloud.io/seeds (comma-separated)
//	ResourceVersion metadata.resourceVersion
//	UID             metadata.uid
//
// # Contract
//
// Get is a consistent read. GetCached passes resourceVersion "0" so the API
// server may answer from its watch cache. Create maps AlreadyExists (and
// Conflict) to control.ErrConflict. Update is a compare-and-swap on the
// record's ResourceVersion and maps Conflict to control.ErrConflict and
// NotFound to control.ErrNotFound. Errors are wrapped with %w.
//
// Update does not drop fields it does not own. The Store remembers the last
// Lease object it read or wrote; when the record's ResourceVersion matches
// it, Update starts from a copy of that object. Otherwise it reads the Lease
// first and, if the stored ResourceVersion differs from the record's, fails
// with control.ErrConflict without writing. Labels, other annotations, owner
// references, and spec fields cachemesh does not use are preserved.
//
// Malformed cachemesh annotations, written by hand or by a different
// version, are tolerated rather than failing reads: an unparsable
// incarnation or term reads as 0, and seeds that are empty or contain
// whitespace are dropped, keeping at most control.MaxSeeds. A term read as 0
// is safe because a new leader takes max(record term, view term)+1.
package kubelease

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/cachemesh/internal/control"
)

// Annotation keys.
const (
	AnnotationPrefix      = "cachemesh.unbounded-cloud.io/"
	AnnotationIncarnation = AnnotationPrefix + "incarnation"
	AnnotationTerm        = AnnotationPrefix + "term"
	AnnotationAddr        = AnnotationPrefix + "addr"
	AnnotationSeeds       = AnnotationPrefix + "seeds"
)

// LeaseClient is the subset of the typed Lease client the Store uses. It is
// satisfied by clientset.CoordinationV1().Leases(namespace).
type LeaseClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordinationv1.Lease, error)
	Create(ctx context.Context, lease *coordinationv1.Lease, opts metav1.CreateOptions) (*coordinationv1.Lease, error)
	Update(ctx context.Context, lease *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error)
}

// Store implements control.Store for one named Lease. It is safe for
// concurrent use.
type Store struct {
	client LeaseClient
	name   string

	mu   sync.Mutex
	last *coordinationv1.Lease
}

var _ control.Store = (*Store)(nil)

// New returns a Store for the Lease called name.
func New(client LeaseClient, name string) *Store {
	return &Store{client: client, name: name}
}

func (s *Store) remember(l *coordinationv1.Lease) {
	s.mu.Lock()
	s.last = l.DeepCopy()
	s.mu.Unlock()
}

func (s *Store) get(ctx context.Context, opts metav1.GetOptions) (control.Record, error) {
	l, err := s.client.Get(ctx, s.name, opts)
	if err != nil {
		return control.Record{}, mapErr("get", s.name, err)
	}

	s.remember(l)

	return Decode(l), nil
}

// Get reads the Lease with a consistent read.
func (s *Store) Get(ctx context.Context) (control.Record, error) {
	return s.get(ctx, metav1.GetOptions{})
}

// GetCached reads the Lease with resourceVersion "0".
func (s *Store) GetCached(ctx context.Context) (control.Record, error) {
	return s.get(ctx, metav1.GetOptions{ResourceVersion: "0"})
}

// Create creates the Lease.
func (s *Store) Create(ctx context.Context, r control.Record) (control.Record, error) {
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: s.name}}
	Encode(l, r)
	l.ResourceVersion = ""

	out, err := s.client.Create(ctx, l, metav1.CreateOptions{})
	if err != nil {
		return control.Record{}, mapErr("create", s.name, err)
	}

	s.remember(out)

	return Decode(out), nil
}

// Update replaces the Lease if r.ResourceVersion matches.
func (s *Store) Update(ctx context.Context, r control.Record) (control.Record, error) {
	if r.ResourceVersion == "" {
		return control.Record{}, fmt.Errorf("update lease %s: empty resourceVersion: %w", s.name, control.ErrConflict)
	}

	s.mu.Lock()

	var base *coordinationv1.Lease
	if s.last != nil && s.last.ResourceVersion == r.ResourceVersion {
		base = s.last.DeepCopy()
	}

	s.mu.Unlock()

	if base == nil {
		cur, err := s.client.Get(ctx, s.name, metav1.GetOptions{})
		if err != nil {
			return control.Record{}, mapErr("update", s.name, err)
		}

		s.remember(cur)

		if cur.ResourceVersion != r.ResourceVersion {
			return control.Record{}, fmt.Errorf("update lease %s: resourceVersion %s != %s: %w",
				s.name, cur.ResourceVersion, r.ResourceVersion, control.ErrConflict)
		}

		base = cur
	}

	Encode(base, r)

	out, err := s.client.Update(ctx, base, metav1.UpdateOptions{})
	if err != nil {
		return control.Record{}, mapErr("update", s.name, err)
	}

	s.remember(out)

	return Decode(out), nil
}

func mapErr(op, name string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s lease %s: %w: %w", op, name, control.ErrNotFound, err)
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%s lease %s: %w: %w", op, name, control.ErrConflict, err)
	default:
		return fmt.Errorf("%s lease %s: %w", op, name, err)
	}
}

// Decode converts a Lease into a Record. It never fails; see the package
// documentation for how malformed annotations are read.
func Decode(l *coordinationv1.Lease) control.Record {
	r := control.Record{ResourceVersion: l.ResourceVersion, UID: string(l.UID)}

	if l.Spec.HolderIdentity != nil {
		r.Holder = *l.Spec.HolderIdentity
	}

	if l.Spec.LeaseDurationSeconds != nil {
		r.LeaseDuration = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}

	if l.Spec.AcquireTime != nil {
		r.AcquireTime = l.Spec.AcquireTime.Time
	}

	if l.Spec.RenewTime != nil {
		r.RenewTime = l.Spec.RenewTime.Time
	}

	if l.Spec.LeaseTransitions != nil {
		r.Transitions = *l.Spec.LeaseTransitions
	}

	a := l.Annotations
	r.Incarnation, _ = strconv.ParseUint(a[AnnotationIncarnation], 10, 64) //nolint:errcheck // malformed reads as 0
	r.Term, _ = strconv.ParseUint(a[AnnotationTerm], 10, 64)               //nolint:errcheck // malformed reads as 0
	r.Addr = a[AnnotationAddr]
	r.Seeds = ParseSeeds(a[AnnotationSeeds])

	return r
}

// Encode writes r into l, leaving fields cachemesh does not own untouched.
func Encode(l *coordinationv1.Lease, r control.Record) {
	holder := r.Holder
	l.Spec.HolderIdentity = &holder

	secs := int32((r.LeaseDuration + time.Second - 1) / time.Second)
	l.Spec.LeaseDurationSeconds = &secs

	l.Spec.AcquireTime = microTime(r.AcquireTime)
	l.Spec.RenewTime = microTime(r.RenewTime)

	transitions := r.Transitions
	l.Spec.LeaseTransitions = &transitions

	if l.Annotations == nil {
		l.Annotations = map[string]string{}
	}

	l.Annotations[AnnotationIncarnation] = strconv.FormatUint(r.Incarnation, 10)
	l.Annotations[AnnotationTerm] = strconv.FormatUint(r.Term, 10)
	l.Annotations[AnnotationAddr] = r.Addr
	l.Annotations[AnnotationSeeds] = FormatSeeds(r.Seeds)
	l.ResourceVersion = r.ResourceVersion
}

func microTime(t time.Time) *metav1.MicroTime {
	if t.IsZero() {
		return nil
	}

	m := metav1.NewMicroTime(t)

	return &m
}

func validSeed(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

// ParseSeeds splits a seeds annotation, dropping invalid entries and keeping
// at most control.MaxSeeds.
func ParseSeeds(v string) []string {
	var out []string

	for s := range strings.SplitSeq(v, ",") {
		if len(out) == control.MaxSeeds {
			break
		}

		if validSeed(s) {
			out = append(out, s)
		}
	}

	return out
}

// FormatSeeds joins seeds for the annotation, dropping invalid entries and
// keeping at most control.MaxSeeds.
func FormatSeeds(seeds []string) string {
	kept := make([]string, 0, min(len(seeds), control.MaxSeeds))

	for _, s := range seeds {
		if len(kept) == control.MaxSeeds {
			break
		}

		if validSeed(s) {
			kept = append(kept, s)
		}
	}

	return strings.Join(kept, ",")
}
