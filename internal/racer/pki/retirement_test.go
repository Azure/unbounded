// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func retirementPod(t *testing.T, f *fixture, id Identity) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: id.PodName, UID: types.UID(id.PodUID)}}
	if err := f.c.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	return pod
}

func TestRetirementCollectionPreservesLiveBootsMembersAndWatermarks(t *testing.T) {
	f := newFixture(t)
	live := node("live-pod", "retired-boot")
	live.PodName = "live"
	pod := retirementPod(t, f, live)
	deleted := node("deleted-pod", "boot")
	deleted.PodName = "deleted"

	f.issue(live, false)

	issued := f.issue(deleted, false)
	for _, id := range []Identity{live, deleted} {
		if err := f.m.Retire(t.Context(), id.Key()); err != nil {
			t.Fatal(err)
		}
	}

	pending := MemberKey{PodUID: live.PodUID, BootID: "pending"}
	if err := f.m.Retire(t.Context(), pending); err != nil {
		t.Fatal(err)
	}

	active := live
	active.BootID = "active-boot"
	f.issue(active, false)
	before := f.state()
	// Keep a label-free terminating Pod: only actual UID absence permits GC.
	pod.Finalizers = []string{"test/retain"}
	if err := f.c.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	if err := f.c.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	after := f.state()
	if !after.RequireLivePod || len(after.Retired) != 2 || !after.Retired[live.Key().String()] || !after.Retired[pending.String()] {
		t.Fatal("collection removed live-Pod tombstones or retained deleted UID")
	}

	if !reflect.DeepEqual(before.Authorities, after.Authorities) || !reflect.DeepEqual(before.Members, after.Members) {
		t.Fatal("collection changed CA watermarks or durable barriers")
	}

	certs, err := parseCertificates(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.m.VerifyMember(t.Context(), deleted.Key(), certs[0].Raw); err == nil {
		t.Fatal("collected identity's unexpired leaf accepted")
	}

	if err := f.m.Admit(t.Context(), live); err == nil {
		t.Fatal("old boot of live Pod re-admitted")
	}

	if err := f.m.Admit(t.Context(), active); err != nil {
		t.Fatal(err)
	}
	// Expiry does not authorize pruning a live Pod's old boot.
	f.now = issued.NotAfter.Add(f.m.options.ClockSkew).Add(time.Second)
	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	if !f.state().Retired[live.Key().String()] {
		t.Fatal("expiry erased stale-boot protection")
	}
}

func TestCollectedPodCannotReenrollAfterTakeoverOrNameReuse(t *testing.T) {
	for _, kind := range []string{Node, ControlPlane} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)

			id := node("deleted", "boot")
			if kind == ControlPlane {
				id = Identity{Kind: ControlPlane, PodUID: "deleted", BootID: "boot"}
			}

			id.PodName = "reused"
			f.issue(id, false)

			if err := f.m.Retire(t.Context(), id.Key()); err != nil {
				t.Fatal(err)
			}

			if err := f.m.CollectRetirements(t.Context()); err != nil {
				t.Fatal(err)
			}

			if len(f.state().Retired) != 0 {
				t.Fatal("deleted UID retained")
			}

			next, err := New(f.c, "racer", f.m.options)
			if err != nil {
				t.Fatal(err)
			}

			if err := next.AcquireLeadership(t.Context(), "takeover"); err != nil {
				t.Fatal(err)
			}

			csr, _ := csrKey(t)
			assertRejected := func(id Identity) {
				t.Helper()

				if err := next.Admit(t.Context(), id); err == nil {
					t.Fatal("collected process admitted")
				}

				if _, err := next.Issue(t.Context(), csr, id); err == nil {
					t.Fatal("collected process issued production leaf")
				}

				if _, err := next.IssueProbe(t.Context(), csr, id); err == nil {
					t.Fatal("collected process issued proof leaf")
				}
			}
			assertRejected(id)
			replacement := id
			replacement.PodUID = "replacement"
			retirementPod(t, f, replacement)
			assertRejected(id)
			missingName := id
			missingName.PodName = ""
			assertRejected(missingName)

			if _, err := next.Issue(t.Context(), csr, replacement); err != nil {
				t.Fatal("replacement blocked", err)
			}

			if err := f.m.Admit(t.Context(), replacement); !errors.Is(err, ErrNotLeader) {
				t.Fatalf("stale leader admitted: %v", err)
			}
		})
	}
}

func TestRetirementCollectionErrorsDoNotPartiallyCommit(t *testing.T) {
	for _, operation := range []string{"list", "shard", "commit"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			for _, uid := range []string{"deleted", "live"} {
				if err := f.m.Retire(t.Context(), MemberKey{PodUID: uid, BootID: "boot"}); err != nil {
					t.Fatal(err)
				}
			}

			id := node("live", "boot")
			id.PodName = "live"
			retirementPod(t, f, id)
			// Keep the deleted and live keys in one bucket so collection must
			// write an immutable replacement rather than only dropping a reference.
			for i := 0; ; i++ {
				key := MemberKey{PodUID: "live", BootID: fmt.Sprintf("boot-%d", i)}
				if participantBucket(key.String()) == participantBucket("deleted/boot") {
					if err := f.m.Retire(t.Context(), key); err != nil {
						t.Fatal(err)
					}

					break
				}
			}

			before := f.state()

			f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok && operation == "list" {
						return errors.New("API unavailable")
					}

					return c.List(ctx, list, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if operation == "shard" {
						return errors.New("shard unavailable")
					}

					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if operation == "commit" && obj.GetName() == SecretName {
						return errors.New("commit unavailable")
					}

					return c.Update(ctx, obj, opts...)
				},
			})
			if err := f.m.CollectRetirements(t.Context()); err == nil {
				t.Fatal("expected collection failure")
			}

			f.m.client = f.c
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("failed collection changed authoritative state")
			}
		})
	}
}

func TestRetirementCollectionFencesDelayedIssuance(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	id.PodName = "pod"
	pod := retirementPod(t, f, id)
	f.issue(id, false)

	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	before := f.state()
	interrupted := false
	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == SecretName && !interrupted {
			interrupted = true
			// The old request already passed its live-Pod check and signed a
			// leaf. Takeover, retirement, and GC must prevent returning that leaf.
			if err := f.c.Delete(ctx, pod); err != nil {
				return err
			}

			next, err := New(f.c, "racer", f.m.options)
			if err != nil {
				return err
			}

			if err := next.AcquireLeadership(ctx, "next"); err != nil {
				return err
			}

			if err := next.Retire(ctx, id.Key()); err != nil {
				return err
			}

			if err := next.CollectRetirements(ctx); err != nil {
				return err
			}
		}

		return c.Update(ctx, obj, opts...)
	}})
	csr, _ := csrKey(t)

	issued, err := f.m.Issue(t.Context(), csr, id)
	if !errors.Is(err, ErrNotLeader) || len(issued.CertificatePEM) != 0 {
		t.Fatalf("delayed issuance survived takeover/GC: %v", err)
	}

	after := f.state()
	if len(after.Members) != 0 || len(after.Retired) != 0 || !after.RequireLivePod {
		t.Fatal("delayed commit revived collected identity")
	}

	if !reflect.DeepEqual(before.Authorities, after.Authorities) {
		t.Fatal("GC changed outstanding certificate watermark")
	}
}

func TestRetirementCollection100KDeletedPodsAndLegacyState(t *testing.T) {
	f := newFixture(t)
	if err := f.m.mutate(t.Context(), func(s *state) error {
		for i := range 100000 {
			s.Retired[fmt.Sprintf("deleted-%d/boot", i)] = true
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	s := f.state()
	if len(s.Shards) != 0 || len(s.Retired) != 0 || !s.RequireLivePod {
		t.Fatal("deleted fleet history remains in committed storage")
	}
	// Existing v1 state can take the same atomic upgrade path.
	secret, legacy, err := f.m.readMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	legacy.Version, legacy.RequireLivePod = 1, false
	legacy.Retired["legacy/boot"] = true

	data, err := encodeState(legacy)
	if err != nil {
		t.Fatal(err)
	}

	secret.Data[StateKey] = data
	if err := f.c.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	s = f.state()
	if s.Version != 2 || !s.RequireLivePod || len(s.Retired) != 0 {
		t.Fatal("legacy upgrade did not atomically enforce live admission")
	}
}

func TestCollectedRetirementStillDelaysRootRemovalUntilLeafExpiry(t *testing.T) {
	f := newFixture(t)
	id := node("deleted", "boot")
	id.PodName = "deleted"

	issued := f.issue(id, false)
	if err := f.m.Retire(t.Context(), id.Key()); err != nil {
		t.Fatal(err)
	}

	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := f.m.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if s := f.state(); s.Phase != "switched" || len(s.Authorities) != 2 {
		t.Fatal("GC bypassed old-leaf expiry barrier")
	}

	f.now = issued.NotAfter.Add(f.m.options.ClockSkew)
	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if s := f.state(); s.Phase != "stable" || len(s.Authorities) != 1 {
		t.Fatal("expired collected identity blocked root removal")
	}
}

func TestLivePodAdmissionReadFailureDoesNotIssue(t *testing.T) {
	f := newFixture(t)
	id := node("live", "boot")
	id.PodName = "live"
	retirementPod(t, f, id)
	f.issue(id, false)

	if err := f.m.CollectRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}

	before := f.state()
	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			return errors.New("Pod API unavailable")
		}

		return c.Get(ctx, key, obj, opts...)
	}})

	csr, _ := csrKey(t)
	if issued, err := f.m.Issue(t.Context(), csr, id); err == nil || len(issued.CertificatePEM) != 0 {
		t.Fatal("live-Pod read failure issued a certificate")
	}

	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("failed live check changed PKI state")
	}
}
