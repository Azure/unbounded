// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCanceledEnrollmentLeavesCommitQueue(t *testing.T) {
	f := newFixture(t)
	csr, _ := csrKey(t)
	entered, release := make(chan struct{}), make(chan struct{})

	var once sync.Once

	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	var reads atomic.Int32

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Name == SecretName && reads.Add(1) == 1 {
			close(entered)

			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return c.Get(ctx, key, obj, opts...)
	}})
	first := make(chan error, 1)

	go func() {
		_, err := f.m.Issue(t.Context(), csr, node("first", "boot"))
		first <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first issuance did not reach storage")
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1500)
	for range cap(done) {
		go func() {
			_, err := f.m.Issue(ctx, csr, node("expired", "boot"))
			done <- err
		}()
	}

	cancel()

	deadline := time.After(2 * time.Second)

	for range cap(done) {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled enrollment returned %v", err)
			}
		case <-deadline:
			t.Fatal("canceled enrollments remain queued behind storage I/O")
		}
	}

	if reads.Load() != 1 {
		t.Fatalf("canceled enrollments reached storage: %d reads", reads.Load())
	}

	unblock()

	if err := <-first; err != nil {
		t.Fatal(err)
	}

	if _, err := f.m.Issue(t.Context(), csr, node("fresh", "boot")); err != nil {
		t.Fatalf("fresh enrollment failed after canceled wave: %v", err)
	}

	if _, err := f.m.Member(t.Context(), node("expired", "boot").Key()); err == nil {
		t.Fatal("canceled enrollment was committed")
	}
}

func TestEnrollmentRetryCommitsOnlyReturnedLeaf(t *testing.T) {
	f := newFixture(t)
	csr, _ := csrKey(t)
	id := node("retry", "boot")

	var updates atomic.Int32

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == SecretName && updates.Add(1) == 1 {
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, SecretName, errors.New("concurrent update"))
		}

		return c.Update(ctx, obj, opts...)
	}})

	issued, err := f.m.Issue(t.Context(), csr, id)
	if err != nil {
		t.Fatal(err)
	}

	certs, err := parseCertificates(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}

	p := f.state().Members[id.Key().String()]
	if updates.Load() != 2 || p == nil || len(p.Leaves) != 1 {
		t.Fatalf("CAS retry did not commit exactly one leaf: updates=%d member=%+v", updates.Load(), p)
	}

	if _, ok := p.Leaves[digest(certs[0].Raw)]; !ok {
		t.Fatal("returned leaf differs from durable retry result")
	}

	if err := f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err != nil {
		t.Fatalf("returned retry certificate is unusable: %v", err)
	}
}
