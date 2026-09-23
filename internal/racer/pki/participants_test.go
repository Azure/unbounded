// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestParticipantStorage100KAndBoundedLookup(t *testing.T) {
	f := newFixture(t)
	if err := f.m.mutate(t.Context(), func(s *state) error {
		expiry := f.now.Add(time.Hour)
		s.Authorities[0].LastIssuedExpiry = expiry
		bundleDigest := s.bundle().Digest()

		for i := range 100000 {
			id := node(fmt.Sprintf("pod-%08d", i), "boot")

			p, err := admit(s, id)
			if err != nil {
				return err
			}

			p.Leaves[digest([]byte("production"))] = leafRecord{Root: s.Active, Expiry: expiry}
			p.Leaves[digest([]byte("renewal"))] = leafRecord{Root: s.Active, Expiry: expiry}
			p.ProofDigest, p.ProofRoot, p.ProofFence = bundleDigest, s.Active, s.Fence
			p.ProofAt = f.now
			s.Retired[node(fmt.Sprintf("old-pod-%08d", i), "old-boot").Key().String()] = true
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	secret, metadata, err := f.m.readMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if metadata.Version != 2 || len(metadata.Members) != 0 || len(metadata.Shards) != 1024 || len(secret.Data[StateKey]) > maxStateBytes {
		t.Fatal("fleet not sharded into bounded objects")
	}
	// A cold lookup reads the commit point and one shard, independent of fleet size.
	loaded, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	gets := 0
	loaded.client = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		return c.Get(ctx, key, obj, opts...)
	}})

	id := node("pod-00099999", "boot")
	if got, err := loaded.Member(t.Context(), id.Key()); err != nil || got != id {
		t.Fatalf("lookup: %+v %v", got, err)
	}

	if gets != 2 {
		t.Fatalf("lookup made %d reads", gets)
	}

	if err := loaded.AcquireLeadership(t.Context(), "takeover"); err != nil {
		t.Fatal(err)
	}

	members, err := loaded.Members(t.Context())
	if err != nil || len(members) != 100000 {
		t.Fatalf("takeover lost fleet: %d %v", len(members), err)
	}

	if err := loaded.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := loaded.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	_, metadata, err = loaded.readMetadata(t.Context())
	if err != nil || metadata.Phase != "overlap" {
		t.Fatalf("unproven fleet did not block rotation: %v", err)
	}
}

func TestParticipantCommitFailureAndMissingShardFailClosed(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == SecretName {
			return errors.New("interrupted commit")
		}

		return c.Update(ctx, obj, opts...)
	}})
	if err := f.m.Admit(t.Context(), id); err == nil {
		t.Fatal("expected failed commit")
	}

	f.m.client = f.c
	if members, err := f.m.Members(t.Context()); err != nil || len(members) != 0 {
		t.Fatal("uncommitted shard became authoritative")
	}

	if err := f.m.Admit(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	_, s, err := f.m.readMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var cm corev1.ConfigMap

	ref := s.Shards[participantBucket(id.Key().String())]
	if err := f.c.Get(t.Context(), f.m.objectKey(ref.Name), &cm); err != nil {
		t.Fatal(err)
	}

	if err := f.c.Delete(t.Context(), &cm); err != nil {
		t.Fatal(err)
	}

	loaded, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err := loaded.Load(t.Context()); err == nil {
		t.Fatal("missing barrier shard accepted")
	}
}

func TestProofObservationsDoNotWriteAndDoNotSurviveTakeover(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	proof := f.proof(id, true)
	writes := 0

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		writes++
		return c.Update(ctx, obj, opts...)
	}, Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		writes++
		return c.Create(ctx, obj, opts...)
	}})
	for range 20 {
		if err := f.m.ObserveHeartbeat(t.Context(), id.Key(), proof.ack); err != nil {
			t.Fatal(err)
		}

		proof.at = proof.at.Add(time.Nanosecond)
		if err := f.m.RecordTLSProof(t.Context(), id.Key(), proof); err != nil {
			t.Fatal(err)
		}
	}

	if writes != 0 {
		t.Fatalf("heartbeat/proof made %d Kubernetes writes", writes)
	}

	next, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err := next.AcquireLeadership(t.Context(), "next"); err != nil {
		t.Fatal(err)
	}

	_, s, err := next.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if next.allProven(s, false) {
		t.Fatal("takeover reused observations")
	}

	if err := f.m.Reconcile(t.Context()); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("stale observer advanced rotation: %v", err)
	}
}

func TestParticipantCollectionPreservesCommittedAndRetiredState(t *testing.T) {
	f := newFixture(t)

	id := node("pod", "boot")
	if err := f.m.Admit(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	if err := f.m.Retire(t.Context(), id.Key()); err != nil {
		t.Fatal(err)
	}

	f.now = f.now.Add(time.Hour)
	if err := f.m.CollectParticipants(t.Context()); err != nil {
		t.Fatal(err)
	}

	var objects corev1.ConfigMapList
	if err := f.c.List(t.Context(), &objects, client.MatchingLabels{participantLabel: "true"}); err != nil {
		t.Fatal(err)
	}

	if len(objects.Items) != 1 {
		t.Fatalf("unreachable versions retained: %d", len(objects.Items))
	}

	if err := f.m.Admit(t.Context(), id); err == nil {
		t.Fatal("collection lost retirement")
	}
}

func TestParticipantMigrationRetainsLegacyAdmissionsAndRetirements(t *testing.T) {
	f := newFixture(t)

	secret, s, err := f.m.readMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	s.Version, s.Shards = 1, nil

	id := node("legacy", "boot")
	if _, err := admit(s, id); err != nil {
		t.Fatal(err)
	}

	retired := node("retired", "boot")
	s.Retired[retired.Key().String()] = true

	data, err := encodeState(s)
	if err != nil {
		t.Fatal(err)
	}

	secret.Data[StateKey] = data
	if err := f.c.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	next, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err := next.AcquireLeadership(t.Context(), "migration"); err != nil {
		t.Fatal(err)
	}

	if err := next.Admit(t.Context(), node("new", "boot")); err != nil {
		t.Fatal(err)
	}

	if got, err := next.Member(t.Context(), id.Key()); err != nil || got != id {
		t.Fatalf("legacy member lost: %+v %v", got, err)
	}

	if err := next.Admit(t.Context(), retired); err == nil {
		t.Fatal("legacy retirement lost")
	}
}

func TestWarmMemberAndProofAvoidKubernetesReads(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	proof := f.proof(id, false)
	// Warm the immutable shard once. Subsequent normal heartbeats must not
	// transfer the fleet's commit metadata from Kubernetes.
	if err := f.m.RecordTLSProof(t.Context(), id.Key(), proof); err != nil {
		t.Fatal(err)
	}

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("unexpected Kubernetes read on warm heartbeat")
	}})
	if err := f.m.ObserveHeartbeat(t.Context(), id.Key(), proof.ack); err != nil {
		t.Fatal(err)
	}

	proof.at = proof.at.Add(time.Nanosecond)
	if err := f.m.RecordTLSProof(t.Context(), id.Key(), proof); err != nil {
		t.Fatal(err)
	}

	if _, err := f.m.memberState(t.Context(), id.Key()); err != nil {
		t.Fatal(err)
	}
}

func TestParticipantCacheRequiresExactReference(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	issued := f.issue(id, false)

	certs, err := parseCertificates(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err != nil {
		t.Fatal(err)
	}

	_, s, err := f.m.readMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	bucket := participantBucket(id.Key().String())
	ref := s.Shards[bucket]
	ref.Digest = digest([]byte("different contents"))

	s.Shards[bucket] = ref
	if err := f.m.loadParticipants(t.Context(), s, id.Key().String()); err == nil {
		t.Fatal("cached object bypassed committed digest validation")
	}

	if err := f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err != nil {
		t.Fatalf("invalid reference poisoned committed lookup: %v", err)
	}
}

func TestParticipantCacheHistoricalReadsRemainBounded(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	issued := f.issue(id, false)

	certs, err := parseCertificates(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}

	var snapshots []*state

	for range 8 {
		f.issue(id, false)

		_, s, err := f.m.readMetadata(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		snapshots = append(snapshots, s)
	}

	if err := f.m.Retire(t.Context(), id.Key()); err != nil {
		t.Fatal(err)
	}

	for _, s := range snapshots {
		// Model an old read finishing after a newer commit/retirement. It may
		// replace the cached bucket, but must never become current membership.
		if err := f.m.loadParticipants(t.Context(), s, id.Key().String()); err != nil {
			t.Fatal(err)
		}

		if s.Members[id.Key().String()] == nil {
			t.Fatal("historical lookup lost its committed member")
		}

		if err := f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err == nil {
			t.Fatal("historical cache entry revived retired member")
		}

		if err := f.m.ObserveHeartbeat(t.Context(), id.Key(), Acknowledgment{Generation: issued.Bundle.Generation, Digest: issued.Bundle.Digest()}); err == nil {
			t.Fatal("historical cache entry granted retired member acknowledgment")
		}

		if len(f.m.shardCache) != 1 {
			t.Fatalf("single bucket retained %d versions", len(f.m.shardCache))
		}
	}
}
