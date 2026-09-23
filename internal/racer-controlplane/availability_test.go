// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWarmStandbyReadinessAndLeaderRequestGate(t *testing.T) {
	_, _, _, replica := replicaFixture(t)

	s := &tlsControl{replica: replica}
	if s.replicaReady(nil) == nil {
		t.Fatal("unstarted replica ready")
	}

	s.listenersReady.Store(true)

	if s.replicaReady(nil) == nil {
		t.Fatal("replica without certificates ready")
	}

	replicaBootstrap(t, replica)

	if s.replicaReady(nil) == nil {
		t.Fatal("standby without a replica proof listener ready")
	}

	replica.proofServing.Store(true)

	if err := s.replicaReady(nil); err != nil {
		t.Fatal("warm standby must be ready", err)
	}

	called := false
	handler := s.leaderOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
	check := func(want int) {
		t.Helper()

		called = false
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v3/test/node", nil))

		if response.Code != want || called != (want == http.StatusNoContent) {
			t.Fatalf("request gate status=%d called=%v", response.Code, called)
		}
	}
	check(http.StatusServiceUnavailable)

	ctx, cancel := context.WithCancel(t.Context())
	s.leaderContext = ctx
	s.ready.Store(true)
	check(http.StatusNoContent)
	cancel()
	check(http.StatusServiceUnavailable)
	s.listenersReady.Store(false)

	if s.replicaReady(nil) == nil {
		t.Fatal("closed listener remained ready")
	}

	s.listenersReady.Store(true)

	replica.expires = time.Now().Add(-time.Second)

	if s.replicaReady(nil) == nil {
		t.Fatal("expired standby remained ready")
	}
}

func TestLeaderRoutePublicationRejectsDelayedPredecessor(t *testing.T) {
	kube, pod, ca, replica := replicaFixture(t)
	replicaBootstrap(t, replica)

	old := &tlsControl{kube: kube, namespace: pod.Namespace, replica: replica, manager: ca, boot: "old-boot"}
	if err := old.clearLocalServingLabel(t.Context()); err != nil {
		t.Fatal(err)
	}

	var before corev1.Pod
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pod), &before); err != nil {
		t.Fatal(err)
	}
	// A successor sweeps a predecessor that has not published its label yet.
	next := &tlsControl{kube: kube, boot: "new-boot"}

	current := before.DeepCopy()
	if err := next.setServingLabel(t.Context(), current, false); err != nil {
		t.Fatal(err)
	}

	if err := old.setServingLabel(t.Context(), &before, true); !apierrors.IsConflict(err) {
		t.Fatalf("delayed old leader route must conflict: %v", err)
	}
}

func TestLeaderRoutingClearsStalePodsAndRestart(t *testing.T) {
	kube, pod, ca, replica := replicaFixture(t)
	replicaBootstrap(t, replica)
	s := &tlsControl{kube: kube, namespace: pod.Namespace, replica: replica, manager: ca, leaderContext: t.Context()}
	s.ready.Store(true)

	old := pod.DeepCopy()
	old.Name, old.UID, old.ResourceVersion = "old-leader", "old-uid", ""

	old.Labels[servingLeaderLabel] = "true"
	if err := kube.Create(t.Context(), old); err != nil {
		t.Fatal(err)
	}

	if err := s.publishServingLeader(t.Context()); err != nil {
		t.Fatal(err)
	}

	var pods corev1.PodList
	if err := kube.List(t.Context(), &pods); err != nil {
		t.Fatal(err)
	}

	for _, actual := range pods.Items {
		if (actual.Labels[servingLeaderLabel] == "true") != (actual.UID == pod.UID) {
			t.Fatal("Service selector retained stale leader or omitted current leader", actual.Name)
		}
	}

	if err := s.clearLocalServingLabel(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}

	if pod.Labels[servingLeaderLabel] != "" {
		t.Fatal("restart retained previous process's route")
	}

	replica.podUID = "wrong-uid"

	if s.clearLocalServingLabel(t.Context()) == nil || s.publishServingLeader(t.Context()) == nil {
		t.Fatal("replacement Pod accepted stale process")
	}
}

func TestFailedSuccessorListenerNeverReady(t *testing.T) {
	_, pod, _, replica := replicaFixture(t)
	replicaBootstrap(t, replica)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeTLSResource(occupied) })

	s := &tlsControl{
		kube: replica.kube, namespace: pod.Namespace, replica: replica, listenersStarted: make(chan struct{}),
		control:    newTLSServer("127.0.0.1:0", http.NotFoundHandler(), replica.hot.ServerConfig(tls.RequireAndVerifyClientCert)),
		enrollment: newTLSServer(occupied.Addr().String(), http.NotFoundHandler(), replica.hot.ServerConfig(tls.NoClientCert)),
	}

	transport := &controlTransport{control: s, proofAddress: "127.0.0.1:0"}
	if transport.Start(t.Context()) == nil || s.replicaReady(nil) == nil {
		t.Fatal("failed successor must not unlock rollout")
	}
}
