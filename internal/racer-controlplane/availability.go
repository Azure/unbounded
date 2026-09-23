// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer"
)

const servingLeaderLabel = racer.MetadataPrefix + "serving-leader"

func (s *tlsControl) serving() bool {
	return s.ready.Load() && s.leaderContext.Err() == nil
}

func (s *tlsControl) leaderOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !s.serving() {
			http.Error(w, "TLS leader is not serving", http.StatusServiceUnavailable)
			return
		}

		next.ServeHTTP(w, req)
	})
}

func (s *tlsControl) replicaReady(req *http.Request) error {
	if !s.listenersReady.Load() || !s.replica.proofServing.Load() {
		return errors.New("TLS listeners are not serving")
	}

	return s.replica.Ready(req)
}

// Labels are routing hints, never authorization. Clear the previous process's
// hint before a restarted Pod can become Ready. UID and resource-version CAS
// prevent a delayed patch from changing a replacement Pod.
func (s *tlsControl) setServingLabel(ctx context.Context, pod *corev1.Pod, serving bool) error {
	before := pod.DeepCopy()
	if serving {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}

		pod.Labels[servingLeaderLabel] = "true"
	} else {
		delete(pod.Labels, servingLeaderLabel)
	}
	// Touch even candidates that have not published yet, fencing their delayed
	// publication through the Pod resource version.
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[racer.MetadataPrefix+"routing-boot"] = s.boot

	return s.kube.Patch(ctx, pod, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (s *tlsControl) clearLocalServingLabel(ctx context.Context) error {
	var pod corev1.Pod
	if err := s.kube.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: s.replica.podName}, &pod); err != nil {
		return err
	}

	if pod.UID != s.replica.podUID {
		return errors.New("local control-plane Pod UID changed")
	}

	return s.setServingLabel(ctx, &pod, false)
}

// Only the elected, fenced, listening leader publishes Service membership.
// Remove stale predecessor hints first; cancellation gates request handling even
// while EndpointSlice/kube-proxy updates are still in flight.
func (s *tlsControl) publishServingLeader(ctx context.Context) error {
	var pods corev1.PodList
	if err := s.kube.List(ctx, &pods, client.InNamespace(s.namespace), client.MatchingLabels{racer.MetadataPrefix + "component": replicaComponent}); err != nil {
		return err
	}

	if err := s.manager.Publish(ctx); err != nil {
		return err
	}

	for i := range pods.Items {
		if err := s.setServingLabel(ctx, &pods.Items[i], false); err != nil {
			return err
		}
	}

	var pod corev1.Pod
	if err := s.kube.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: s.replica.podName}, &pod); err != nil {
		return err
	}
	// Read the Pod CAS version before checking the durable fence. A successor
	// either invalidates this version in its sweep or is visible in the fence.
	if err := s.manager.Publish(ctx); err != nil {
		return err
	}

	if !s.serving() || pod.UID != s.replica.podUID || !pod.DeletionTimestamp.IsZero() {
		return errors.New("leader Pod is no longer serving")
	}

	return s.setServingLabel(ctx, &pod, true)
}

// Bind every production port on followers too, so a broken successor cannot
// count as available and cause Kubernetes to remove a working old replica.
// HTTP handlers and the proof accept loop remain gated on elected leadership.
type controlTransport struct {
	control      *tlsControl
	proofAddress string
}

func (*controlTransport) NeedLeaderElection() bool { return false }

func (t *controlTransport) Start(ctx context.Context) error {
	s := t.control
	if err := s.clearLocalServingLabel(ctx); err != nil {
		return err
	}

	var listeners []net.Listener

	defer func() {
		s.listenersReady.Store(false)

		for _, listener := range listeners {
			closeTLSResource(listener)
		}
		// Routing cleanup is best-effort after local fail-closed shutdown. A new
		// leader also removes stale hints when this Pod cannot reach the API.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()

		if err := s.clearLocalServingLabel(cleanup); err != nil {
			log.Printf("clear serving leader route: %v", err)
		}
	}()

	for _, address := range []string{s.control.Addr, s.enrollment.Addr, t.proofAddress} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}

		listeners = append(listeners, listener)
	}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer closeTLSResource(s.control)
	defer closeTLSResource(s.enrollment)

	finished := make(chan error, 3)

	go func() { finished <- s.control.ServeTLS(listeners[0], "", "") }()
	go func() { finished <- s.enrollment.ServeTLS(listeners[1], "", "") }()
	go func() {
		finished <- serveTrustProof(serveCtx, &leaderListener{Listener: listeners[2], serving: s.serving}, s.replica.ProofTLS(), s.manager)
	}()

	s.listenersReady.Store(true)
	close(s.listenersStarted)

	select {
	case <-ctx.Done():
		return nil
	case err := <-finished:
		return err
	}
}

type leaderListener struct {
	net.Listener
	serving func() bool
}

func (l *leaderListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		if l.serving() {
			return conn, nil
		}

		closeTLSResource(conn)
	}
}
