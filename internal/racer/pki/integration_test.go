// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestOverlapOldNodeClientAndServerOnlyCPProof(t *testing.T) {
	f := newFixture(t)
	f.m.options.Now = time.Now
	nodeID := node("node", "boot")
	cpID := Identity{Kind: ControlPlane, PodUID: "cp", BootID: "boot"}
	nodeHot, nodeCert, nodeKey := hotFor(t, f.m, nodeID, false)

	cpClient, cpCert, cpKey := hotFor(t, f.m, Identity{Kind: ControlPlane, PodUID: "leader", BootID: "boot"}, false)
	if err := f.m.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	server, issued, _ := hotFor(t, f.m, cpID, true)
	for _, item := range []struct {
		hot  *HotTLS
		cert IssuedCertificate
		key  []byte
	}{{nodeHot, nodeCert, nodeKey}, {cpClient, cpCert, cpKey}} {
		if err := item.hot.Update(issued.Bundle.JSON(), item.cert.CertificatePEM, item.key); err != nil {
			t.Fatal(err)
		}
	}

	ack := Acknowledgment{Generation: issued.Bundle.Generation, Digest: issued.Bundle.Digest()}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	proofs := make(chan Proof, 1)
	errorsCh := make(chan error, 1)

	go func() {
		_, finish, err := server.HandshakeProof(ctx, a, true, "")
		if err != nil {
			errorsCh <- err
			return
		}

		proof, err := finish(ack)
		if err != nil {
			errorsCh <- err
			return
		}

		proofs <- proof
	}()

	conn, _, err := nodeHot.HandshakeProof(ctx, b, false, "racer-controlplane.racer.svc")
	if err != nil {
		t.Fatal(err)
	}

	if conn.ConnectionState().NegotiatedProtocol != "http/1.1" {
		t.Fatal("proof negotiated multiplexed protocol")
	}

	select {
	case err := <-errorsCh:
		t.Fatal(err)
	case proof := <-proofs:
		if proof.peerRoot != nodeCert.RootDigest || proof.localRoot != issued.RootDigest || proof.peerRoot == proof.localRoot {
			t.Fatal("test did not exercise old client against pending server")
		}

		if err := f.m.RecordTLSProof(t.Context(), nodeID.Key(), proof); err != nil {
			t.Fatal(err)
		}

		if f.state().Members[nodeID.Key().String()].ProofRoot != issued.RootDigest {
			t.Fatal("node credited client issuer rather than proven server root")
		}
	}

	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()

	go func() { errorsCh <- tls.Server(c, server.ServerConfig(tls.NoClientCert)).HandshakeContext(ctx) }()

	_, finish, err := cpClient.HandshakeProof(ctx, d, false, "racer-controlplane.racer.svc")
	if err != nil {
		t.Fatal(err)
	}

	if err := <-errorsCh; err != nil {
		t.Fatal(err)
	}

	proof, err := finish(ack)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.m.RecordTLSProof(t.Context(), cpID.Key(), proof); err != nil {
		t.Fatal(err)
	}
	// A fresh handshake with an old client stops qualifying after issuance switches.
	if err := f.m.mutate(t.Context(), func(s *state) error {
		s.Active = s.Authorities[1].Digest
		s.Phase = "switched"

		return nextGeneration(s)
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.m.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}

	s := f.state()
	oldProof := f.proof(nodeID, false)
	// Bind the actual old enrolled leaf while retaining the current bundle claims.
	certs, err := parseCertificates(nodeCert.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}

	oldProof.peerFingerprint = digest(certs[0].Raw)
	oldProof.peerRoot = nodeCert.RootDigest

	oldProof.localRoot = s.Active
	if err := f.m.RecordTLSProof(t.Context(), nodeID.Key(), oldProof); err == nil {
		t.Fatal("switched phase accepted old client")
	}
}

func TestScheduledRotationWithHandledNonce(t *testing.T) {
	f := newFixture(t)

	var cm corev1.ConfigMap
	if err := f.c.Get(t.Context(), f.m.objectKey(ConfigMapName), &cm); err != nil {
		t.Fatal(err)
	}

	cm.Annotations[RotationAnnotation] = "operator-1"
	if err := f.c.Update(t.Context(), &cm); err != nil {
		t.Fatal(err)
	}

	if err := f.m.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "stable" {
		t.Fatal("publish advanced rotation before admission")
	}

	id := Identity{Kind: ControlPlane, PodUID: "unstarted", BootID: "pending"}
	if err := f.m.Admit(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	members, err := f.m.Members(t.Context())
	if err != nil || len(members) != 1 || members[0] != id {
		t.Fatalf("durable members: %v %v", members, err)
	}

	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().RotationNonce != "operator-1" || f.state().Phase != "overlap" {
		t.Fatal("nonce not persisted atomically with rotation")
	}

	if err := f.m.Retire(t.Context(), id.Key()); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := f.m.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if f.state().Phase != "stable" || f.state().Generation != 4 {
		t.Fatal("handled nonce retriggered")
	}

	f.now = f.state().NextRotation
	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "overlap" {
		t.Fatal("old nonce blocked scheduled rotation")
	}
}

func TestBootstrapExistingDeploymentState(t *testing.T) {
	for _, object := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-state", Namespace: "racer", Labels: map[string]string{"racer.unbounded-cloud.io/state": "commit"}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-replica-old", Namespace: "racer"}, Data: map[string]string{"certificate": "old-cert"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-replica-proof", Namespace: "racer"}, Data: map[string]string{"proof-certificate": "old-cert"}},
	} {
		t.Run(object.GetName(), func(t *testing.T) {
			f := newFixture(t)
			for _, obj := range []client.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: "racer"}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "racer"}}} {
				if err := f.c.Delete(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			}

			if err := f.c.Create(t.Context(), object); err != nil {
				t.Fatal(err)
			}

			m, err := New(f.c, "racer", Options{})
			if err != nil {
				t.Fatal(err)
			}

			if err := m.AcquireLeadership(t.Context(), "new"); !errors.Is(err, ErrLostState) {
				t.Fatalf("bootstrap replaced existing trust: %v", err)
			}
		})
	}
}

func TestStateCapacityFailsClosed(t *testing.T) {
	f := newFixture(t)
	before := f.state()

	err := f.m.mutate(t.Context(), func(s *state) error { s.RotationNonce = strings.Repeat("x", maxStateBytes); return nil })
	if err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatalf("unbounded state accepted: %v", err)
	}

	after := f.state()
	if after.Generation != before.Generation || after.RotationNonce != before.RotationNonce {
		t.Fatal("failed capacity check modified persisted state")
	}
}

func TestMemberBindingAndHeartbeatNoop(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot")
	issued := f.issue(id, false)

	certs, err := parseCertificates(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err != nil {
		t.Fatal(err)
	}

	other := id
	other.BootID = "other"
	f.issue(other, false)

	if err = f.m.VerifyMember(t.Context(), other.Key(), certs[0].Raw); err == nil {
		t.Fatal("leaf crossed boot")
	}

	ack := Acknowledgment{Generation: issued.Bundle.Generation, Digest: issued.Bundle.Digest()}
	if err = f.m.ObserveHeartbeat(t.Context(), id.Key(), ack); err != nil {
		t.Fatal(err)
	}

	before, _, err := f.m.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if err = f.m.ObserveHeartbeat(t.Context(), id.Key(), ack); err != nil {
		t.Fatal(err)
	}

	after, _, err := f.m.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if after.ResourceVersion != before.ResourceVersion {
		t.Fatal("identical heartbeat rewrote Secret")
	}

	f.now = issued.NotAfter
	if err = f.m.VerifyMember(t.Context(), id.Key(), certs[0].Raw); err == nil {
		t.Fatal("expired enrollment accepted")
	}
}

func TestBundleDigestUsesExactPublishedBytes(t *testing.T) {
	f := newFixture(t)

	bundle, err := f.m.Bundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ParseBundle(bundle.JSON()); err != nil {
		t.Fatal(err)
	}

	if _, err := ParseBundle(append(bundle.JSON(), '\n')); err == nil {
		t.Fatal("accepted different file bytes with canonicalized digest")
	}
}
