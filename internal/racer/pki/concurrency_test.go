// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestIssuanceDoesNotBlockCommittedMemberObservations(t *testing.T) {
	for _, outcome := range []string{"commit", "failure", "takeover"} {
		t.Run(outcome, func(t *testing.T) {
			f := newFixture(t)
			id := node("existing", "boot")
			issued := f.issue(id, false)

			certs, err := parseCertificates(issued.CertificatePEM)
			if err != nil {
				t.Fatal(err)
			}

			proof := f.proof(id, true)
			pending := node("pending", "boot")
			csr, _ := csrKey(t)
			entered, release := make(chan struct{}), make(chan struct{})

			var once sync.Once

			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			failure := errors.New("interrupted commit")
			f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetName() == SecretName {
					close(entered)

					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}

					if outcome == "failure" {
						return failure
					}
				}

				return c.Update(ctx, obj, opts...)
			}})

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			done := make(chan error, 1)

			go func() {
				_, err := f.m.Issue(ctx, csr, pending)
				done <- err
			}()

			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("issuance did not reach commit")
			}

			observed := make(chan error, 1)

			go func() {
				if err := f.m.VerifyMember(ctx, id.Key(), certs[0].Raw); err != nil {
					observed <- err
					return
				}

				if err := f.m.ObserveHeartbeat(ctx, id.Key(), proof.ack); err != nil {
					observed <- err
					return
				}

				if err := f.m.RecordTLSProof(ctx, id.Key(), proof); err != nil {
					observed <- err
					return
				}

				if err := f.m.ObserveHeartbeat(ctx, pending.Key(), proof.ack); err == nil {
					observed <- errors.New("uncommitted member accepted")
					return
				}

				observed <- nil
			}()

			select {
			case err := <-observed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("issuance API write blocked committed-member verification/heartbeat/proof")
			}

			if outcome == "takeover" {
				next, err := New(f.c, "racer", f.m.options)
				if err != nil {
					t.Fatal(err)
				}

				if err := next.AcquireLeadership(ctx, "next"); err != nil {
					t.Fatal(err)
				}

				_, s, err := next.read(ctx)
				if err != nil || next.allProven(s, false) {
					t.Fatalf("takeover reused volatile proof: %v", err)
				}
			}

			unblock()

			err = <-done

			switch outcome {
			case "commit":
				if err != nil {
					t.Fatal(err)
				}

				if err := f.m.ObserveHeartbeat(ctx, pending.Key(), proof.ack); err != nil {
					t.Fatalf("committed member unavailable: %v", err)
				}
			case "failure":
				if !errors.Is(err, failure) {
					t.Fatalf("expected commit failure: %v", err)
				}
			case "takeover":
				if !errors.Is(err, ErrNotLeader) {
					t.Fatalf("stale issuer escaped fencing: %v", err)
				}
			}

			if outcome != "commit" {
				if err := f.m.ObserveHeartbeat(ctx, pending.Key(), proof.ack); err == nil {
					t.Fatal("failed commit published membership")
				}
			}

			if outcome != "takeover" {
				p := f.state().Members[id.Key().String()]
				if p.ProofAt != proof.at || !p.Drained {
					t.Fatal("in-flight commit lost concurrent proof")
				}
			}
		})
	}
}

func TestConcurrentHeartbeatAndProofPreserveReplayBarrier(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	proof := f.proof(id, true)
	start := make(chan struct{})
	proofs, heartbeats := make(chan error, 32), make(chan error, 32)

	for range 32 {
		go func() {
			<-start

			proofs <- f.m.RecordTLSProof(t.Context(), id.Key(), proof)
		}()
		go func() {
			<-start

			heartbeats <- f.m.ObserveHeartbeat(t.Context(), id.Key(), proof.ack)
		}()
	}

	close(start)

	accepted := 0

	for range 32 {
		if err := <-proofs; err == nil {
			accepted++
		}

		if err := <-heartbeats; err != nil {
			t.Fatal(err)
		}
	}

	if accepted != 1 {
		t.Fatalf("same handshake accepted %d times", accepted)
	}

	p := f.state().Members[id.Key().String()]
	if p.ProofAt != proof.at || !p.Drained || p.ProofFence != "leader-1" {
		t.Fatal("concurrent heartbeat erased proof evidence")
	}
}

func TestColdParticipantReadDoesNotBlockWarmObservation(t *testing.T) {
	f := newFixture(t)
	id := node("warm", "boot")
	proof := f.proof(id, true)

	cold := node("cold", "boot")
	for i := 0; participantBucket(cold.Key().String()) == participantBucket(id.Key().String()); i++ {
		cold = node(fmt.Sprintf("cold-%d", i), "boot")
	}

	if err := f.m.Admit(t.Context(), cold); err != nil {
		t.Fatal(err)
	}

	if err := f.m.ObserveHeartbeat(t.Context(), id.Key(), proof.ack); err != nil {
		t.Fatal(err)
	}

	ref := f.m.localState.Load().Shards[participantBucket(cold.Key().String())]
	entered, release := make(chan struct{}), make(chan struct{})

	var once sync.Once

	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	var blocked atomic.Bool

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Name == ref.Name && blocked.CompareAndSwap(false, true) {
			close(entered)

			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return c.Get(ctx, key, obj, opts...)
	}})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- f.m.ObserveHeartbeat(ctx, cold.Key(), proof.ack) }()

	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("cold read not reached")
	}

	warm := make(chan error, 1)

	go func() { warm <- f.m.RecordTLSProof(ctx, id.Key(), proof) }()

	select {
	case err := <-warm:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cold shard I/O blocked warm proof")
	}
	// Retirement can commit while the observation holds an old snapshot. That
	// observation must not publish credit for the retired member on completion.
	if err := f.m.Retire(ctx, cold.Key()); err != nil {
		t.Fatal(err)
	}

	unblock()

	if err := <-done; !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale observation accepted: %v", err)
	}
}
