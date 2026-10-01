// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestReplicaInstallationAndFreshness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leader := initializedTopology(t)
		publication := reconcileTopology(t, leader, t.Context())
		follower := Assemble(leader.Config, leader.Client, leader.APIReader)

		process, cancel := context.WithCancel(t.Context())
		defer cancel()

		follower.Server.Publications.bindProcess(process)

		image, err := wire.DecodePublication(strings.NewReader(publication.encoded))
		if err != nil {
			t.Fatal(err)
		}

		bad := image

		bad.Sequence++
		if err := follower.Replication.installReplica(t.Context(), process, bad); err == nil {
			t.Fatal("unconfirmed counters installed")
		}

		if err := follower.Replication.installReplica(t.Context(), process, image); err != nil {
			t.Fatal(err)
		}

		current, err := follower.Server.Publications.Current()
		if err != nil || current.encoded != publication.encoded {
			t.Fatal("replica did not install canonical image", err)
		}

		time.Sleep(20 * time.Second)

		if err := follower.Server.Publications.confirm(publication.record); err != nil {
			t.Fatal(err)
		}

		time.Sleep(20 * time.Second)

		if follower.Server.Publications.Ready(nil) != nil {
			t.Fatal("unchanged authoritative confirmation did not renew freshness")
		}

		time.Sleep(11 * time.Second)

		if follower.Server.Publications.Ready(nil) == nil {
			t.Fatal("expired image still serves")
		}

		if follower.Server.Publications.current != current {
			t.Fatal("interruption discarded validated image")
		}

		if err := follower.Replication.installReplica(t.Context(), process, image); err != nil {
			t.Fatal(err)
		}

		rollback := publication.record

		rollback.ContentHash = strings.Repeat("0", 64)
		if follower.Server.Publications.confirm(rollback) == nil {
			t.Fatal("same-counter corruption accepted")
		}

		cancel()

		if follower.Server.Publications.Ready(nil) == nil {
			t.Fatal("process cancellation ignored")
		}
	})
}

func TestReplicaServingSurvivesPublisherCancellation(t *testing.T) {
	r := initializedTopology(t)
	r.Publications.bindProcess(t.Context())
	publisher, cancel := context.WithCancel(t.Context())
	publication := reconcileTopology(t, r, publisher)

	cancel()

	if _, err := r.Publications.Current(); err != nil {
		t.Fatal("publisher lifetime leaked into serving", err)
	}

	if publication.leadership.Err() != nil {
		t.Fatal("image bound to publisher instead of process")
	}
}

func TestReplicaObservationsFailClosed(t *testing.T) {
	f := newServingFixture(t)
	r := f.a.Replication
	r.observe(f.ctx)

	if f.a.Server.Ready(nil) != nil {
		t.Fatal("valid observation withdrew readiness")
	}

	r.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("offline")
	}})
	r.observe(f.ctx)

	if f.a.Server.Ready(nil) != nil {
		t.Fatal("transport interruption discarded recent state")
	}

	r.APIReader = f.a.Topology.APIReader

	cm, _, err := readVersion(f.ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	cm.Data["sequence"] = "0"
	if err := r.Client.Update(f.ctx, cm); err != nil {
		t.Fatal(err)
	}

	r.observe(f.ctx)

	if f.a.Server.Ready(nil) == nil {
		t.Fatal("observed invalid authority still serves")
	}
}

func TestReplicaLeaderDiscovery(t *testing.T) {
	f := newServingFixture(t)

	r := f.a.Replication
	if err := coordv1.AddToScheme(r.Client.Scheme()); err != nil {
		t.Fatal(err)
	}

	r.Config.ControllerServiceAccount = "racer-controller"
	r.Config.ReplicationPort = 8443
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "controller", UID: "controller-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "racer-controller"}, Status: corev1.PodStatus{PodIP: "192.0.2.10"}}

	lease := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "racer-controller"}, Spec: coordv1.LeaseSpec{HolderIdentity: ptr.To("controller/controller-uid"), RenewTime: ptr.To(metav1.NewMicroTime(time.Now())), LeaseDurationSeconds: ptr.To(int32(15))}}
	for _, obj := range []client.Object{pod, lease} {
		if err := r.Client.Create(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	if address, err := r.leaderAddress(f.ctx); err != nil || address != "192.0.2.10:8443" {
		t.Fatal(address, err)
	}

	lease.Spec.HolderIdentity = ptr.To("controller/replaced-uid")
	if err := r.Client.Update(f.ctx, lease); err != nil {
		t.Fatal(err)
	}

	if _, err := r.leaderAddress(f.ctx); err == nil {
		t.Fatal("replaced leader Pod accepted")
	}
}

func TestReplicationRouteAuthorizationAndEarlyListener(t *testing.T) {
	f := newServingFixture(t)
	r := f.a.Replication
	r.Config.ControllerServiceAccount = "racer-controller"
	r.leader = f.ctx
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "controller", UID: "controller-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "racer-controller"}}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "racer-controller", UID: "controller-sa"}}
	for _, obj := range []client.Object{pod, sa} {
		if err := r.Client.Create(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	username := "system:serviceaccount:" + r.Config.Namespace + ":racer-controller"
	audience := ReplicationAudience
	r.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		review := obj.(*authv1.TokenReview)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != ReplicationAudience {
			t.Fatal("wrong review audience")
		}

		review.Status = authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{audience}, User: authv1.UserInfo{Username: username, UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}

		return nil
	}})

	for _, unchanged := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocked flush unchanged=%v failure=%v", unchanged, fail), func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, replicationPath, nil)
				request.TLS = f.requestState(t)
				request.Header.Set("Authorization", "Bearer "+f.token)

				want := http.StatusOK

				if unchanged {
					p, err := r.Publications.Current()
					if err != nil {
						t.Fatal(err)
					}

					request.URL.RawQuery = fmt.Sprintf("after=%d", p.record.Sequence)
					want = http.StatusNoContent
				}

				w := &blockingResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{}), blockFlush: true, fail: fail}

				unblock := sync.OnceFunc(func() { close(w.unblock) })
				defer unblock()

				done := make(chan any, 1)

				go func() { defer func() { done <- recover() }(); f.a.Server.Handler().ServeHTTP(w, request) }()

				select {
				case <-w.entered:
				case <-time.After(8 * time.Second):
					t.Fatal("explicit flush not reached")
				}

				if len(f.a.Server.writes) != 1 {
					t.Fatal("write admission released before flush")
				}

				held := f.a.Server.replicationPolls.count() == 1

				if !held {
					t.Fatal("poll admission released before flush")
				}

				duplicate := httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(duplicate, request.Clone(f.ctx))

				if duplicate.Code != http.StatusTooManyRequests {
					t.Fatalf("duplicate during flush: %d", duplicate.Code)
				}

				unblock()

				var wantAbort any
				if fail {
					wantAbort = http.ErrAbortHandler
				}

				if aborted := <-done; aborted != wantAbort || w.Code != want {
					t.Fatalf("flush result %v status %d", aborted, w.Code)
				}

				if len(f.a.Server.writes) != 0 {
					t.Fatal("write admission leaked after flush")
				}

				held = f.a.Server.replicationPolls.count() != 0

				if held {
					t.Fatal("poll admission leaked after flush")
				}
			})
		}
	}

	for _, tc := range []struct {
		name string
		code int
	}{{"controller", 200}, {"duplicate poll", 429}, {"dataplane", 403}, {"wrong audience", 401}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "duplicate poll" {
				f.a.Server.initializeAdmission()

				if !f.a.Server.replicationPolls.acquire(string(pod.UID)) {
					t.Fatal("could not reserve replication poll")
				}
				defer f.a.Server.replicationPolls.release(string(pod.UID))
			}

			if tc.name == "dataplane" {
				username = "system:serviceaccount:" + r.Config.Namespace + ":racer-dataplane"
			}

			if tc.name == "wrong audience" {
				audience = wire.TokenAudience
			}

			request := httptest.NewRequest(http.MethodGet, replicationPath, nil)
			request.TLS = f.requestState(t)
			request.Header.Set("Authorization", "Bearer "+f.token)

			response := httptest.NewRecorder()
			f.a.Server.Handler().ServeHTTP(response, request)

			if response.Code != tc.code {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	// TLS must be reachable before public readiness, including before trust is
	// initialized. Public routes remain unavailable; internal auth is independent.
	f.a.Server.Trust.invalidate()
	endpoint := f.start(t)
	peer := f.client(t, nil)
	response, err := peer.Get(endpoint + replicationPath)
	responseBody(t, response, err, http.StatusUnauthorized)
}
