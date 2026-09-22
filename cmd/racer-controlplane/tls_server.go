// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/pki"
)

type tlsConnectionKey struct{}

type tlsControl struct {
	manager             *pki.Manager
	replica             *replicaTLS
	control, enrollment *http.Server
	ready               atomic.Bool
	boot                string
	kube                client.Client
	namespace           string
	pkiReady            chan struct{}
}

func setupTLSControl(manager ctrl.Manager, config *Server, listen, enrollListen, namespace string, interval time.Duration) error {
	direct, err := client.New(manager.GetConfig(), client.Options{Scheme: manager.GetScheme()})
	if err != nil {
		return err
	}

	ca, err := pki.New(direct, namespace, pki.Options{RotateAfter: interval})
	if err != nil {
		return err
	}

	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}

	boot := hex.EncodeToString(nonce[:])
	hot := pki.NewHotTLS()

	replica, err := newReplicaTLS(direct, namespace, os.Getenv("RACER_POD_NAME"), types.UID(os.Getenv("RACER_POD_UID")), boot, ca, hot)
	if err != nil {
		return err
	}

	if err := manager.Add(replica); err != nil {
		return err
	}

	connections := new(tlsConnections)
	replica.SetDrainedCheck(connections.drained)
	replica.SetInstalledHook(connections.rotate)

	enrollment := &enrollmentServer{kube: direct, review: config.reviewClient, namespace: namespace}
	enrollment.renewal = func(ctx context.Context, key types.NamespacedName, uid, boot string) (enrollmentIdentity, error) {
		return retainedEnrollmentIdentity(ctx, direct, ca, key, uid, boot)
	}
	enrollment.selected = func(id enrollmentIdentity) bool {
		config.mu.Lock()
		defer config.mu.Unlock()

		if config.source == nil {
			return false
		}

		u, err := hex.DecodeString(id.universe)
		if err != nil || len(u) != 32 {
			return false
		}

		topology := config.source.topologies[[32]byte(u)]
		if topology == nil {
			return false
		}

		name, ok := topology.byID[id.node]

		return ok && topology.g.Nodes[name].PodUID == id.podUID
	}
	enrollment.issue = func(ctx context.Context, csr string, id enrollmentIdentity) (enrollmentResponse, error) {
		issued, err := ca.Issue(ctx, []byte(csr), pki.Identity{Kind: pki.Node, Universe: id.universe, Node: id.node, PodUID: id.podUID, BootID: id.boot, ContainerID: id.containerID})
		if err != nil {
			return enrollmentResponse{}, err
		}

		chain := string(issued.CertificatePEM)

		remaining := []byte(issued.Bundle.Certificates)
		for len(remaining) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil {
				return enrollmentResponse{}, errors.New("invalid issuer bundle")
			}

			digest := sha256.Sum256(block.Bytes)
			if hex.EncodeToString(digest[:]) == issued.RootDigest {
				chain += string(pem.EncodeToMemory(block))
				break
			}

			remaining = rest
		}

		return enrollmentResponse{Certificate: chain, Generation: issued.Bundle.Generation, Issuer: issued.RootDigest}, nil
	}
	controlMux := http.NewServeMux()
	config.trustHeartbeat = func(req *http.Request, uid string) error {
		key := pki.MemberKey{PodUID: uid, BootID: req.Header.Get("X-Racer-Boot")}
		if err := ca.VerifyMember(req.Context(), key, req.TLS.VerifiedChains[0][0].Raw); err != nil {
			return err
		}

		ack, err := trustAcknowledgment(req)
		if err != nil {
			return err
		}

		// Bundle projection can lag publication; keep topology delivery available
		// while refusing rotation credit for a stale acknowledgment.
		bundle, err := ca.Bundle(req.Context())
		if err != nil {
			return err
		}

		if ack.Generation != bundle.Generation || ack.Digest != bundle.Digest() {
			return nil
		}

		return ca.ObserveHeartbeat(req.Context(), key, ack)
	}
	controlMux.HandleFunc("GET /v3/{universe}/{node}", config.control)

	enrollMux := http.NewServeMux()
	enrollMux.HandleFunc("POST /v3/enroll", enrollment.enroll)

	pkiReady := make(chan struct{})
	config.pkiReady = pkiReady
	s := &tlsControl{
		manager: ca, replica: replica, boot: boot, kube: direct, namespace: namespace, pkiReady: pkiReady,
		control:    newTLSServer(listen, controlMux, hot.ServerConfig(tls.RequireAndVerifyClientCert)),
		enrollment: newTLSServer(enrollListen, enrollMux, hot.ServerConfig(tls.NoClientCert)),
	}
	s.control.ConnState = connections.state
	s.enrollment.ConnState = connections.state

	if err := manager.AddReadyzCheck("leader-tls", func(req *http.Request) error {
		if !s.ready.Load() {
			return errors.New("TLS leader is not serving")
		}

		return replica.Ready(req)
	}); err != nil {
		return err
	}

	return manager.Add(s)
}

func trustAcknowledgment(req *http.Request) (pki.Acknowledgment, error) {
	generation, err := strconv.ParseUint(req.Header.Get("X-Racer-Trust-Generation"), 10, 64)
	if err != nil || generation == 0 {
		return pki.Acknowledgment{}, errors.New("invalid trust generation")
	}

	digest, err := hex.DecodeString(req.Header.Get("X-Racer-Trust-Digest"))
	if err != nil || len(digest) != 32 {
		return pki.Acknowledgment{}, errors.New("invalid trust digest")
	}

	old, err := strconv.ParseUint(req.Header.Get("X-Racer-Old-Connections"), 10, 64)
	if err != nil {
		return pki.Acknowledgment{}, errors.New("invalid old connection count")
	}

	if req.TLS == nil || len(req.TLS.VerifiedChains) == 0 {
		return pki.Acknowledgment{}, errInvalidCredential
	}

	chain := req.TLS.VerifiedChains[0]
	if len(chain) == 0 {
		return pki.Acknowledgment{}, errInvalidCredential
	}

	issuer := sha256.Sum256(chain[len(chain)-1].Raw)
	if req.Header.Get("X-Racer-Certificate-Issuer") != hex.EncodeToString(issuer[:]) {
		return pki.Acknowledgment{}, errors.New("certificate issuer header differs from verified TLS issuer")
	}

	return pki.Acknowledgment{Generation: generation, Digest: hex.EncodeToString(digest), OldConnectionsDrained: old == 0}, nil
}

func newTLSServer(address string, handler http.Handler, config *tls.Config) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, TLSConfig: config,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return context.WithValue(ctx, tlsConnectionKey{}, time.Now())
		},
	}
}

func (*tlsControl) NeedLeaderElection() bool { return true }

func (s *tlsControl) Start(ctx context.Context) error {
	if err := s.manager.AcquireLeadership(ctx, s.boot); err != nil {
		return err
	}

	if err := s.manager.Publish(ctx); err != nil {
		return err
	}
	// Publish the initial CA before topology creation can make a fresh
	// installation indistinguishable from an installation with lost CA state.
	close(s.pkiReady)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Replica reconciliation also issues this leader's initial certificate.
	for {
		if err := s.replica.ReconcileLeader(ctx); err != nil {
			log.Printf("replica TLS initialization: %v", err)
		}

		if s.replica.Ready(nil) == nil {
			break
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}

	controlListener, err := net.Listen("tcp", s.control.Addr)
	if err != nil {
		return err
	}
	defer closeTLSResource(controlListener)

	enrollListener, err := net.Listen("tcp", s.enrollment.Addr)
	if err != nil {
		return err
	}
	defer closeTLSResource(enrollListener)

	proofListener, err := net.Listen("tcp", ":8446")
	if err != nil {
		return err
	}
	defer closeTLSResource(proofListener)

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errorsCh := make(chan error, 3)

	go func() { errorsCh <- serveTrustProof(serveCtx, proofListener, s.replica.ProofTLS(), s.manager) }()
	go func() { errorsCh <- s.control.ServeTLS(controlListener, "", "") }()
	go func() { errorsCh <- s.enrollment.ServeTLS(enrollListener, "", "") }()

	s.ready.Store(true)
	defer s.ready.Store(false)
	defer closeTLSResource(s.control)
	defer closeTLSResource(s.enrollment)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errorsCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		case <-ticker.C:
			if err := s.replica.ReconcileLeader(ctx); err != nil {
				log.Printf("replica TLS reconciliation: %v", err)
				continue
			}

			if err := s.retireDeletedNodes(ctx); err != nil {
				log.Printf("dataplane TLS retirement: %v", err)
				continue
			}

			if err := s.manager.Reconcile(ctx); err != nil {
				if errors.Is(err, pki.ErrNotLeader) {
					return err
				}
				// Invalid updates keep the installed TLS snapshot; the persisted
				// authority refuses further issuance until state is repaired.
				log.Printf("CA reconciliation: %v", err)
			}
		}
	}
}

func (s *tlsControl) retireDeletedNodes(ctx context.Context) error {
	members, err := s.manager.Members(ctx)
	if err != nil {
		return err
	}

	var pods corev1.PodList
	// An unfiltered authoritative namespace list is necessary: losing labels,
	// readiness, or a cached watch entry does not prove process retirement.
	if err := s.kube.List(ctx, &pods, client.InNamespace(s.namespace)); err != nil {
		return err
	}

	live := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		live[string(pods.Items[i].UID)] = &pods.Items[i]
	}

	for _, member := range members {
		pod := live[member.PodUID]
		if member.Kind == pki.Node && (pod == nil || containerAuthoritativelyStopped(pod, "dataplane", member.ContainerID)) {
			if err := s.manager.Retire(ctx, member.Key()); err != nil {
				return err
			}
		}
	}

	return nil
}
