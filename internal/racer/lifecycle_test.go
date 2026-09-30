// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestLifecycleLeaderContext(t *testing.T) {
	for _, source := range []string{"parent", "leader", "child"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				leader, loseLeadership := context.WithCancel(t.Context())
				defer loseLeadership()

				parent, stopParent := context.WithTimeout(t.Context(), time.Minute)
				defer stopParent()

				key := connectionKey{}
				parent = context.WithValue(parent, key, source)
				l := newLifecycle(NewPublications())
				l.leader = leader

				child, cancel := l.LeaderContext(parent)
				defer cancel()

				deadline, ok := child.Deadline()

				parentDeadline, _ := parent.Deadline()
				if child.Err() != nil || child.Value(key) != source || !ok || deadline != parentDeadline {
					t.Fatal("child did not retain parent values, deadline, and live context")
				}

				switch source {
				case "parent":
					stopParent()
				case "leader":
					loseLeadership()
				case "child":
					cancel()
				}

				synctest.Wait()

				if !errors.Is(child.Err(), context.Canceled) {
					t.Fatalf("child ignored %s cancellation: %v", source, child.Err())
				}

				if source != "leader" && leader.Err() != nil || source != "parent" && parent.Err() != nil {
					t.Fatal("child cancellation propagated to an independent parent")
				}
			})
		})
	}

	leader, cancel := context.WithCancel(t.Context())
	cancel()

	for _, l := range []*Lifecycle{nil, newLifecycle(NewPublications()), {leader: leader}} {
		child, stop := l.LeaderContext(t.Context())
		if !errors.Is(child.Err(), context.Canceled) {
			t.Fatal("absent or canceled leadership did not immediately cancel child")
		}

		stop()
	}
}

func TestLifecycleReadyNotifications(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Lifecycle, bool)
	}{
		{name: "issuer", set: (*Lifecycle).SetIssuerReady},
		{name: "serving", set: (*Lifecycle).SetServingReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the historical test name, but observe readiness through HTTP:
			// the removed notification channel had no production subscriber.
			f := newServingFixture(t)
			l := f.a.Lifecycle
			handler := f.a.Server.Handler()

			tc.set(l, false)

			for _, step := range []struct {
				name  string
				ready bool
			}{
				{name: "initial false"},
				{name: "become ready", ready: true},
				{name: "remain ready", ready: true},
				{name: "withdraw readiness"},
				{name: "remain unready"},
				{name: "restore readiness", ready: true},
			} {
				t.Run(step.name, func(t *testing.T) {
					tc.set(l, step.ready)

					r := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
					r.TLS = f.requestState(t)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)

					want := http.StatusServiceUnavailable
					if step.ready {
						want = http.StatusOK
					}

					responseBody(t, w.Result(), nil, want)
				})
			}
		})
	}
}

func TestLifecycleFollowerWithPublicationRemainsUnready(t *testing.T) {
	r := initializedTopology(t)
	reconcileTopology(t, r, t.Context())

	if err := r.Publications.Ready(nil); err != nil {
		t.Fatal(err)
	}

	l := newLifecycle(r.Publications)
	l.leader, l.synced = t.Context(), true
	l.SetIssuerReady(true)
	l.SetServingReady(true)

	if err := l.Ready(nil); err != nil {
		t.Fatalf("follower with validated state must receive Service traffic: %v", err)
	}
}

func TestLifecycleGatesAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)

		l := newLifecycle(r.Publications)
		if l.NeedLeaderElection() || l.Ready(nil) == nil {
			t.Fatal("follower ready")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		syncCache := make(chan struct{})
		l.waitForCacheSync = func(ctx context.Context) bool {
			select {
			case <-ctx.Done():
				return false
			case <-syncCache:
				return true
			}
		}
		started := make(chan error, 1)

		go func() { started <- l.Start(ctx) }()

		l.SetIssuerReady(true)
		l.SetServingReady(true)
		reconcileTopology(t, r, ctx)

		if l.Ready(nil) == nil {
			t.Fatal("ready before synchronized inputs")
		}

		close(syncCache)
		synctest.Wait()

		if err := l.Ready(nil); err != nil {
			t.Fatal(err)
		}

		l.SetIssuerReady(false)

		if l.Ready(nil) == nil {
			t.Fatal("ready without usable issuer")
		}

		l.SetIssuerReady(true)
		l.SetServingReady(false)

		if l.Ready(nil) == nil {
			t.Fatal("ready without listener")
		}

		cancel()

		if err := <-started; err != nil {
			t.Fatal(err)
		}

		l.SetIssuerReady(true)
		l.SetServingReady(true)

		if l.Ready(nil) == nil {
			t.Fatal("old leadership resurrected")
		}

		request, stop := l.LeaderContext(t.Context())
		defer stop()

		if !errors.Is(request.Err(), context.Canceled) {
			t.Fatalf("request resurrected: %v", request.Err())
		}

		if err := l.Start(context.Background()); !errors.Is(err, wire.Conflict) {
			t.Fatalf("leadership restarted: %v", err)
		}
	})
}

func TestLifecycleWaitsForPublicationAndCancelsBeforeStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)
		l := newLifecycle(r.Publications)
		l.waitForCacheSync = func(context.Context) bool { return true }

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- l.Start(ctx) }()

		l.SetIssuerReady(true)
		l.SetServingReady(true)
		synctest.Wait()

		waitCtx, stop := context.WithCancel(context.Background())
		stop()

		request, stopRequest := l.LeaderContext(waitCtx)
		defer stopRequest()

		if !errors.Is(request.Err(), context.Canceled) {
			t.Fatalf("request ignores cancellation: %v", request.Err())
		}

		if err := l.Ready(nil); !errors.Is(err, wire.Unavailable) {
			t.Fatalf("ready without publication: %v", err)
		}

		reconcileTopology(t, r, ctx)

		if err := l.Ready(nil); err != nil {
			t.Fatal(err)
		}

		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}
		require.ErrorIs(t, l.Ready(nil), wire.Unavailable)

		beforeStartup := newLifecycle(r.Publications)
		beforeStartup.waitForCacheSync = func(ctx context.Context) bool { return ctx.Err() == nil }
		require.NoError(t, beforeStartup.Start(ctx))
		beforeStartup.SetIssuerReady(true)
		beforeStartup.SetServingReady(true)
		require.ErrorIs(t, beforeStartup.Ready(nil), wire.Unavailable, "canceled startup became ready")
	})
}

func TestInitialEnqueueEmptyInputsAndCoalescing(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	for range 10 {
		if err := initialEnqueue().Start(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}

	if q.Len() != 1 {
		t.Fatalf("startup events not coalesced: %d", q.Len())
	}

	request, stopped := q.Get()
	if stopped || request != singleton(context.Background(), nil)[0] {
		t.Fatalf("initial request: %+v", request)
	}

	q.Done(request)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := initialEnqueue().Start(ctx, q); !errors.Is(err, context.Canceled) || q.Len() != 0 {
		t.Fatalf("canceled startup queued: %v", err)
	}
}

func TestLifecycleHTTPStartupAdmission(t *testing.T) {
	for _, scenario := range []string{"synchronized", "cache failed", "canceled during sync"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				l := newLifecycle(f.a.Server.Publications)
				f.a.Server.Lifecycle = l
				l.SetIssuerReady(true)
				l.SetServingReady(true)

				syncCache := make(chan struct{})
				l.waitForCacheSync = func(ctx context.Context) bool {
					select {
					case <-ctx.Done():
						return false
					case <-syncCache:
						return scenario == "synchronized"
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				done := make(chan error, 1)

				go func() { done <- l.Start(ctx) }()

				synctest.Wait()

				handler := f.a.Server.Handler()
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				before := httptest.NewRecorder()
				handler.ServeHTTP(before, request)
				responseBody(t, before.Result(), nil, http.StatusServiceUnavailable)

				if scenario == "canceled during sync" {
					cancel()
				} else {
					close(syncCache)
				}

				synctest.Wait()

				after := httptest.NewRecorder()
				handler.ServeHTTP(after, request)

				want := http.StatusServiceUnavailable
				if scenario == "synchronized" {
					want = http.StatusOK
				}

				responseBody(t, after.Result(), nil, want)

				cancel()

				err := <-done
				if scenario == "cache failed" {
					if !errors.Is(err, wire.Unavailable) {
						t.Fatalf("cache failure: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}

				stopped := httptest.NewRecorder()
				handler.ServeHTTP(stopped, request)
				responseBody(t, stopped.Result(), nil, http.StatusServiceUnavailable)
			})
		})
	}
}

func TestTopologyWatchFiltering(t *testing.T) {
	cfg := testConfig(t)
	node := memberNode()
	updated := node.DeepCopy()

	updated.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("node readiness enqueued topology")
	}

	updated.Annotations = map[string]string{wire.SharesAnnotation: ""}
	if !nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("absent -> invalid empty annotation lost")
	}

	updated.Annotations = nil

	updated.Labels = map[string]string{wire.ExclusionLabel: "false"}
	if !nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("exclusion presence lost")
	}

	pod := memberPod("pod", 1, "192.0.2.1")
	pod.OwnerReferences[0].Name = cfg.DaemonSetName

	pred := managedPodChanges(cfg)
	if !pred.Create(event.CreateEvent{Object: &pod}) {
		t.Fatal("managed pod ignored")
	}

	changed := pod.DeepCopy()

	changed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if pred.Update(event.UpdateEvent{ObjectOld: &pod, ObjectNew: changed}) {
		t.Fatal("pod readiness enqueued topology")
	}

	changed.OwnerReferences = nil
	if pred.Create(event.CreateEvent{Object: changed}) || !pred.Update(event.UpdateEvent{ObjectOld: &pod, ObjectNew: changed}) {
		t.Fatal("ownership loss filtering")
	}

	changed = pod.DeepCopy()

	changed.Namespace = "unrelated"
	if pred.Create(event.CreateEvent{Object: changed}) {
		t.Fatal("foreign namespace pod admitted")
	}

	if keys := podNodeKeys(&pod); len(keys) != 1 || keys[0] != pod.Spec.NodeName {
		t.Fatalf("node index: %v", keys)
	}

	pod.Spec.NodeName = ""
	if len(podNodeKeys(&pod)) != 0 {
		t.Fatal("unassigned pod indexed")
	}

	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace, cm.ResourceVersion = cfg.VersionConfigMapName, cfg.Namespace, "1"
	newCM := cm.DeepCopy()

	newCM.ResourceVersion = "2"
	if versionChanges(cfg).Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: newCM}) {
		t.Fatal("CAS-only write caused reconcile loop")
	}

	newCM.Data = map[string]string{"sequence": "2"}
	if !versionChanges(cfg).Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: newCM}) {
		t.Fatal("version change ignored")
	}
}
