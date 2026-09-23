// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/pki"
)

func TestWarmStandbyReadinessAndLeaderRequestGate(t *testing.T) {
	_, _, _, replica := replicaFixture(t)

	s := &tlsControl{replica: replica, kube: replica.kube, namespace: replica.namespace}
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

	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: replica.namespace, Name: replicaComponent}}
	if err := replica.kube.Create(t.Context(), service); err != nil {
		t.Fatal(err)
	}

	if s.replicaReady(nil) == nil {
		t.Fatal("warm standby ready before selector migration")
	}

	service.Spec.Selector = map[string]string{servingLeaderLabel: "true"}
	if err := replica.kube.Update(t.Context(), service); err != nil {
		t.Fatal(err)
	}

	if err := s.replicaReady(nil); err != nil {
		t.Fatal("warm standby must be ready", err)
	}

	var pod corev1.Pod
	if err := replica.kube.Get(t.Context(), client.ObjectKey{Namespace: replica.namespace, Name: replica.podName}, &pod); err != nil {
		t.Fatal(err)
	}

	pod.Labels[servingLeaderLabel] = "true"
	if err := replica.kube.Update(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}

	if s.replicaReady(nil) == nil {
		t.Fatal("racing legacy migration exposed restarted warm standby")
	}

	delete(pod.Labels, servingLeaderLabel)

	if err := replica.kube.Update(t.Context(), &pod); err != nil {
		t.Fatal(err)
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

func TestOldBootCleanupDoesNotRemoveNewBootRoute(t *testing.T) {
	kube, pod, _, replica := replicaFixture(t)

	old := &tlsControl{kube: kube, namespace: pod.Namespace, replica: replica, boot: "old"}
	if err := old.clearLocalServingLabel(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}

	next := &tlsControl{kube: kube, boot: "new"}
	if err := next.setServingLabel(t.Context(), pod, true); err != nil {
		t.Fatal(err)
	}

	if err := old.clearLocalRoute(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}

	if pod.Labels[servingLeaderLabel] != "true" || pod.Annotations[routingBootAnnotation] != "new" {
		t.Fatal("old boot cleanup removed successor route")
	}
}

func TestDelayedLeaderSweepCannotClearSuccessorRoute(t *testing.T) {
	kube, pod, ca, replica := replicaFixture(t)
	replicaBootstrap(t, replica)

	nextCA, err := pki.New(kube, pod.Namespace, pki.Options{})
	if err != nil {
		t.Fatal(err)
	}

	next := &tlsControl{kube: kube, namespace: pod.Namespace, replica: replica, manager: nextCA, boot: "new", leaderContext: t.Context()}
	next.ready.Store(true)

	old := &tlsControl{kube: kube, namespace: pod.Namespace, replica: replica, manager: ca, boot: "old", leaderContext: t.Context()}
	old.ready.Store(true)

	watch, ok := kube.(client.WithWatch)
	if !ok {
		t.Fatal("fake client lacks Watch")
	}

	delayed := false

	old.kube = interceptor.NewClient(watch, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if !delayed {
			delayed = true

			if err := nextCA.AcquireLeadership(ctx, "new"); err != nil {
				t.Fatal(err)
			}

			if err := next.publishServingLeader(ctx); err != nil {
				t.Fatal(err)
			}
		}

		return c.Patch(ctx, object, patch, opts...)
	}})
	if err := old.publishServingLeader(t.Context()); !apierrors.IsConflict(err) {
		t.Fatalf("delayed predecessor sweep must conflict: %v", err)
	}

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}

	if pod.Labels[servingLeaderLabel] != "true" || pod.Annotations[routingBootAnnotation] != "new" {
		t.Fatal("delayed sweep removed successor route")
	}

	if err := old.publishServingLeader(t.Context()); !errors.Is(err, pki.ErrNotLeader) {
		t.Fatalf("stale leader must fail before touching current Pod versions: %v", err)
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
