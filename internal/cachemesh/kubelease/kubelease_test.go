// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kubelease

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/control"
)

const (
	ns   = "cachemesh"
	name = "leader"
)

var leasesGR = schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}

func sample() control.Record {
	now := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)

	return control.Record{
		Holder:        "node-a",
		Incarnation:   42,
		Term:          7,
		Addr:          "10.0.0.1:7000",
		Seeds:         []string{"10.0.0.2:7000", "10.0.0.3:7000"},
		LeaseDuration: 15 * time.Second,
		AcquireTime:   now,
		RenewTime:     now.Add(time.Second),
		Transitions:   3,
	}
}

func newStore(objs ...runtime.Object) (*fake.Clientset, *Store) {
	c := fake.NewClientset(objs...)
	rv := 100

	// The fake tracker neither assigns nor checks resourceVersions; emulate
	// the API server so compare-and-swap behaves like the real thing.
	c.PrependReactor("*", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() != "create" && a.GetVerb() != "update" {
			return false, nil, nil
		}

		obj, _ := a.(k8stesting.CreateAction).GetObject().(*coordinationv1.Lease) //nolint:errcheck,forcetypeassert // create and update actions both implement CreateAction
		if obj != nil && a.GetVerb() == "update" {
			cur, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), obj.Name)
			if err != nil {
				return true, nil, err
			}

			if curLease, ok := cur.(*coordinationv1.Lease); ok && curLease.ResourceVersion != obj.ResourceVersion {
				return true, nil, apierrors.NewConflict(leasesGR, obj.Name, errors.New("resourceVersion mismatch"))
			}
		}

		if obj != nil {
			rv++
			obj.ResourceVersion = strconv.Itoa(rv)
		}

		return false, nil, nil
	})

	return c, New(c.CoordinationV1().Leases(ns), name)
}

func eqRecord(t *testing.T, got, want control.Record) {
	t.Helper()

	if got.Holder != want.Holder || got.Incarnation != want.Incarnation || got.Term != want.Term ||
		got.Addr != want.Addr || !slices.Equal(got.Seeds, want.Seeds) || got.LeaseDuration != want.LeaseDuration ||
		!got.AcquireTime.Equal(want.AcquireTime) || !got.RenewTime.Equal(want.RenewTime) || got.Transitions != want.Transitions {
		t.Fatalf("record mismatch\n got %+v\nwant %+v", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, s := newStore()

	if _, err := s.Get(ctx); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}

	want := sample()

	created, err := s.Create(ctx, want)
	if err != nil {
		t.Fatal(err)
	}

	eqRecord(t, created, want)

	if created.ResourceVersion == "" {
		t.Fatal("no resourceVersion")
	}

	got, err := s.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	eqRecord(t, got, want)

	got.Term = 8
	got.Holder = ""
	got.Seeds = nil

	upd, err := s.Update(ctx, got)
	if err != nil {
		t.Fatal(err)
	}

	eqRecord(t, upd, got)

	if _, err := s.Create(ctx, want); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("create existing: %v", err)
	}
}

func TestLeaseDurationRoundsUp(t *testing.T) {
	t.Parallel()

	r := sample()
	r.LeaseDuration = 1500 * time.Millisecond

	var l coordinationv1.Lease
	Encode(&l, r)

	if *l.Spec.LeaseDurationSeconds != 2 {
		t.Fatalf("seconds %d", *l.Spec.LeaseDurationSeconds)
	}

	r.AcquireTime = time.Time{}
	Encode(&l, r)

	if l.Spec.AcquireTime != nil {
		t.Fatal("zero time encoded")
	}
}

func TestUpdateConflictAndNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, s := newStore()

	r := sample()
	r.ResourceVersion = "5"

	if _, err := s.Update(ctx, r); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	created, err := s.Create(ctx, sample())
	if err != nil {
		t.Fatal(err)
	}

	// Stale version: detected from the read before writing.
	stale := created
	stale.ResourceVersion = "stale"

	if _, err := s.Update(ctx, stale); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}

	empty := created
	empty.ResourceVersion = ""

	if _, err := s.Update(ctx, empty); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("empty rv update: %v", err)
	}
}

func TestServerErrorsMapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c, s := newStore()

	created, err := s.Create(ctx, sample())
	if err != nil {
		t.Fatal(err)
	}

	var sentRV string

	c.PrependReactor("update", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, ok := a.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if !ok {
			t.Fatal("unexpected object")
		}

		sentRV = obj.ResourceVersion

		switch *obj.Spec.LeaseTransitions {
		case 100:
			return true, nil, apierrors.NewConflict(leasesGR, name, errors.New("rv"))
		case 101:
			return true, nil, apierrors.NewNotFound(leasesGR, name)
		case 102:
			return true, nil, errors.New("boom")
		}

		return false, nil, nil
	})
	c.PrependReactor("create", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(leasesGR, name, errors.New("x"))
	})

	cases := []struct {
		transitions int32
		want        error
	}{{100, control.ErrConflict}, {101, control.ErrNotFound}, {102, nil}}

	for _, tc := range cases {
		r := created
		r.Transitions = tc.transitions

		_, err := s.Update(ctx, r)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) ||
			(tc.want == nil && (errors.Is(err, control.ErrConflict) || errors.Is(err, control.ErrNotFound))) {
			t.Fatalf("transitions %d: %v", tc.transitions, err)
		}

		if sentRV != created.ResourceVersion {
			t.Fatalf("sent rv %q want %q", sentRV, created.ResourceVersion)
		}
	}

	if _, err := s.Create(ctx, sample()); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("create conflict: %v", err)
	}
}

func TestGetCachedUsesRV0(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c, s := newStore()

	if _, err := s.Create(ctx, sample()); err != nil {
		t.Fatal(err)
	}

	var rvs []string

	c.PrependReactor("get", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ga, ok := a.(k8stesting.GetActionImpl)
		if !ok {
			t.Fatalf("action %T", a)
		}

		rvs = append(rvs, ga.GetOptions.ResourceVersion)

		return false, nil, nil
	})

	if _, err := s.GetCached(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rvs, []string{"0", ""}) {
		t.Fatalf("resource versions %q", rvs)
	}
}

func TestUpdatePreservesForeignFields(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	strategy := "Custom"
	l := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, ResourceVersion: "9",
			Labels:      map[string]string{"app": "gantry"},
			Annotations: map[string]string{"other/x": "y", AnnotationTerm: "3"},
		},
		Spec: coordinationv1.LeaseSpec{Strategy: (*coordinationv1.CoordinatedLeaseStrategy)(&strategy)},
	}

	for _, viaRead := range []bool{true, false} {
		c, s := newStore(l.DeepCopy())

		r := sample()
		r.ResourceVersion = "9"

		if viaRead {
			if _, err := s.Get(ctx); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := s.Update(ctx, r); err != nil {
			t.Fatal(err)
		}

		got, err := c.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}

		if got.Labels["app"] != "gantry" || got.Annotations["other/x"] != "y" || got.Spec.Strategy == nil ||
			got.Annotations[AnnotationTerm] != "7" {
			t.Fatalf("viaRead=%v: lost fields: %+v", viaRead, got)
		}
	}
}

func TestMalformedAnnotationsTolerated(t *testing.T) {
	t.Parallel()

	many := make([]string, 0, 40)
	for i := range 40 {
		many = append(many, "s"+strconv.Itoa(i))
	}

	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationIncarnation: "-1",
		AnnotationTerm:        "abc",
		AnnotationSeeds:       ",a, b ,,c d," + strings.Join(many, ","),
	}}}

	r := Decode(l)
	if r.Incarnation != 0 || r.Term != 0 || r.Holder != "" {
		t.Fatalf("decoded %+v", r)
	}

	if len(r.Seeds) != control.MaxSeeds || r.Seeds[0] != "a" || r.Seeds[1] != "s0" {
		t.Fatalf("seeds %q", r.Seeds)
	}

	if got := FormatSeeds(append([]string{"", "x y", "a,b", "ok"}, many...)); !strings.HasPrefix(got, "ok,s0,") ||
		len(ParseSeeds(got)) != control.MaxSeeds {
		t.Fatalf("format %q", got)
	}

	if ParseSeeds("") != nil {
		t.Fatal("empty seeds")
	}
}

// TestElectorOverKubelease runs one Elector against the fake API server.
func TestElectorOverKubelease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, s := newStore()
	e := control.NewElector(control.ElectionConfig{}, control.Self{ID: "a", Incarnation: 1, Key: 1, Addr: "a:1"}, newRand())

	in := control.Input{Wall: time.Unix(1e9, 0), HaveView: true, ClusterSize: 3, IsCandidate: true, Seeds: []string{"b:1"}}
	for range 100 {
		in.Now += int64(100 * time.Millisecond)
		in.Wall = in.Wall.Add(100 * time.Millisecond)

		for _, op := range e.Tick(in) {
			e.OnResult(in, control.Execute(ctx, s, op))
		}
	}

	r, err := s.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if e.State() != control.StateLeader || r.Holder != "a" || r.Term != e.Term() || !slices.Equal(r.Seeds, []string{"b:1"}) {
		t.Fatalf("state %v record %+v", e.State(), r)
	}
}

func newRand() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func TestUpdateCachedBaseServerConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c, s := newStore()

	created, err := s.Create(ctx, sample())
	if err != nil {
		t.Fatal(err)
	}

	// Another writer changes the Lease behind the Store's back.
	l, err := c.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.CoordinationV1().Leases(ns).Update(ctx, l, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Update(ctx, created); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("update with cached base: %v", err)
	}
}
