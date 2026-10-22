// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package advertise

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
)

type scheduledDHT struct {
	*fakes.DHT
	mu             sync.Mutex
	scheduled      map[string]bool
	beforeProvide  func(context.Context, digest.Digest) error
	beforeWithdraw func(context.Context, digest.Digest) error
}

func newScheduledDHT() *scheduledDHT {
	return &scheduledDHT{DHT: fakes.NewDHT(), scheduled: map[string]bool{}}
}

func (d *scheduledDHT) Provide(ctx context.Context, key digest.Digest) error {
	if d.beforeProvide != nil {
		if err := d.beforeProvide(ctx, key); err != nil {
			return err
		}
	}

	if err := d.DHT.Provide(ctx, key); err != nil {
		return err
	}

	d.mu.Lock()
	d.scheduled[key.String()] = true
	d.mu.Unlock()

	return nil
}

func (d *scheduledDHT) Withdraw(ctx context.Context, key digest.Digest) error {
	if d.beforeWithdraw != nil {
		if err := d.beforeWithdraw(ctx, key); err != nil {
			return err
		}
	}

	if err := d.DHT.Withdraw(ctx, key); err != nil {
		return err
	}

	d.mu.Lock()
	delete(d.scheduled, key.String())
	d.mu.Unlock()

	return nil
}

func (d *scheduledDHT) isScheduled(key digest.Digest) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.scheduled[key.String()]
}

func assertNoPendingOperations(t *testing.T, a *Advertiser) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.pending) != 0 {
		t.Fatalf("retained %d per-digest gates after operations finished", len(a.pending))
	}
}

func TestAdvertiser_RegistrationAndWithdrawalStayOrdered(t *testing.T) {
	for _, source := range []string{"notify", "reconcile"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := mkDigest("overlapping-add-delete")
				inv := &mockInventory{}
				inv.set([]digest.Digest{d}, nil)

				dht := newScheduledDHT()
				started := make(chan struct{})
				release := make(chan struct{})
				provided := make(chan struct{})
				provideResult := make(chan bool, 1)
				withdrawResult := make(chan bool, 1)
				dht.beforeProvide = func(ctx context.Context, _ digest.Digest) error {
					close(started)

					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				a := New(inv, dht, WithMetrics(MetricsHooks{
					OnWithdraw: func() { <-provided },
				}))

				go func() {
					if source == "notify" {
						provideResult <- a.Notify(context.Background(), d, true)
					} else {
						provideResult <- a.Reconcile(context.Background()) == nil
					}

					close(provided)
				}()

				<-started
				inv.set(nil, nil)

				go func() { withdrawResult <- a.Notify(context.Background(), d, false) }()

				synctest.Wait()

				if dht.WithdrawCount(d) != 0 {
					t.Error("withdrawal overtook an in-flight registration")
				}

				close(release)

				if !<-provideResult || !<-withdrawResult {
					t.Fatal("overlapping registration or withdrawal failed")
				}

				if err := a.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}

				if a.IsAnnounced(d) || dht.isScheduled(d) {
					t.Fatal("deleted content retained an announcement or periodic reprovide")
				}

				assertNoPendingOperations(t, a)
			})
		})
	}
}

func TestAdvertiser_ReconcileWithdrawalAndRegistrationStayOrdered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := mkDigest("overlapping-delete-add")
		inv := &mockInventory{}
		dht := newScheduledDHT()
		removed := make(chan struct{})

		var waitForRemoval atomic.Bool

		a := New(inv, dht, WithMetrics(MetricsHooks{
			OnProvide: func() {
				if waitForRemoval.Load() {
					<-removed
				}
			},
		}))
		if !a.Notify(context.Background(), d, true) {
			t.Fatal("initial registration failed")
		}

		waitForRemoval.Store(true)

		started := make(chan struct{})
		release := make(chan struct{})
		dht.beforeWithdraw = func(ctx context.Context, _ digest.Digest) error {
			close(started)

			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		withdrawResult := make(chan error, 1)
		provideResult := make(chan bool, 1)

		go func() {
			withdrawResult <- a.Reconcile(context.Background())

			close(removed)
		}()

		<-started
		inv.set([]digest.Digest{d}, nil)

		go func() { provideResult <- a.Notify(context.Background(), d, true) }()

		synctest.Wait()

		if dht.ProvideCount(d) != 1 {
			t.Error("registration overtook an in-flight withdrawal")
		}

		close(release)

		if err := <-withdrawResult; err != nil {
			t.Fatal(err)
		}

		if !<-provideResult {
			t.Fatal("registration failed")
		}

		if err := a.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}

		if !a.IsAnnounced(d) || !dht.isScheduled(d) {
			t.Fatal("present content lost its announcement or periodic reprovide")
		}

		assertNoPendingOperations(t, a)
	})
}

func TestAdvertiser_DifferentDigestsRemainConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow, fast := mkDigest("slow"), mkDigest("fast")
		dht := newScheduledDHT()
		started := make(chan struct{})
		release := make(chan struct{})
		dht.beforeProvide = func(ctx context.Context, d digest.Digest) error {
			if d != slow {
				return nil
			}

			close(started)

			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		a := New(&mockInventory{}, dht)
		slowResult := make(chan bool, 1)
		fastResult := make(chan bool, 1)

		go func() { slowResult <- a.Notify(context.Background(), slow, true) }()

		<-started

		go func() { fastResult <- a.Notify(context.Background(), fast, true) }()

		synctest.Wait()

		select {
		case ok := <-fastResult:
			if !ok {
				t.Error("unrelated registration failed")
			}
		default:
			t.Error("an unrelated digest was blocked by the slow registration")
		}

		close(release)

		if !<-slowResult {
			t.Fatal("slow registration failed")
		}

		synctest.Wait()
		assertNoPendingOperations(t, a)
	})
}

func TestAdvertiser_CanceledWaiterDoesNotReleaseActiveOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := mkDigest("canceled-withdrawal")
		dht := newScheduledDHT()
		started := make(chan struct{})
		release := make(chan struct{})
		dht.beforeProvide = func(ctx context.Context, _ digest.Digest) error {
			close(started)

			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		a := New(&mockInventory{}, dht)
		provideResult := make(chan bool, 1)
		withdrawResult := make(chan bool, 1)

		go func() { provideResult <- a.Notify(context.Background(), d, true) }()

		<-started

		ctx, cancel := context.WithCancel(context.Background())

		go func() { withdrawResult <- a.Notify(ctx, d, false) }()

		synctest.Wait()
		cancel()

		if <-withdrawResult {
			t.Error("canceled withdrawal reported success")
		}

		if dht.WithdrawCount(d) != 0 {
			t.Error("canceled waiter reached the backend")
		}

		close(release)

		if !<-provideResult {
			t.Fatal("active registration was disrupted by a canceled waiter")
		}

		if !a.Notify(context.Background(), d, false) {
			t.Fatal("gate could not be reused after cancellation")
		}

		if a.IsAnnounced(d) || dht.isScheduled(d) {
			t.Fatal("withdrawal did not remove the registration")
		}

		assertNoPendingOperations(t, a)
	})
}

func TestAdvertiser_FailedOperationsPreserveStateAndReleaseGate(t *testing.T) {
	d := mkDigest("failed-registration")
	dht := newScheduledDHT()
	a := New(&mockInventory{}, dht)
	failure := errors.New("backend failure")

	dht.beforeProvide = func(context.Context, digest.Digest) error { return failure }
	if a.Notify(context.Background(), d, true) || a.IsAnnounced(d) || dht.isScheduled(d) {
		t.Fatal("failed registration changed the announced or scheduled set")
	}

	assertNoPendingOperations(t, a)

	dht.beforeProvide = nil

	if !a.Notify(context.Background(), d, true) {
		t.Fatal("registration could not be retried after failure")
	}

	dht.beforeWithdraw = func(context.Context, digest.Digest) error { return failure }
	if a.Notify(context.Background(), d, false) || !a.IsAnnounced(d) || !dht.isScheduled(d) {
		t.Fatal("failed withdrawal removed the existing registration")
	}

	assertNoPendingOperations(t, a)

	dht.beforeWithdraw = nil

	if !a.Notify(context.Background(), d, false) {
		t.Fatal("withdrawal could not be retried after failure")
	}

	assertNoPendingOperations(t, a)
}
