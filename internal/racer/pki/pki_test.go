// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fixture struct {
	t   *testing.T
	m   *Manager
	c   client.WithWatch
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, now: time.Now().UTC().Truncate(time.Second)}
	f.c = fake.NewClientBuilder().WithScheme(scheme).Build()

	m, err := New(f.c, "racer", Options{Now: func() time.Time { return f.now }, LeafLifetime: time.Hour, ClockSkew: time.Minute, RotateAfter: 24 * time.Hour, CALifetime: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	f.m = m
	if err = m.AcquireLeadership(t.Context(), "leader-1"); err != nil {
		t.Fatal(err)
	}

	if err = m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	return f
}

func node(pod, boot string) Identity {
	return Identity{Kind: Node, Universe: strings.Repeat("a", 64), Node: strings.Repeat("b", 64), PodUID: pod, BootID: boot}
}

func csrKey(t *testing.T) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Hostile requested identities and constraints must not survive issuance.
	uri, _ := url.Parse("spiffe://evil/controlplane")
	caDER, _ := asn1.Marshal(struct {
		CA      bool
		PathLen int
	}{true, 2})

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "override", Organization: []string{"system:masters"}}, DNSNames: []string{"evil.example"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{uri}, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: caDER}}}, key)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func (f *fixture) state() *state {
	f.t.Helper()

	_, s, err := f.m.read(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}

	return s
}

func (f *fixture) issue(identity Identity, probe bool) IssuedCertificate {
	f.t.Helper()
	csr, _ := csrKey(f.t)

	var (
		cert IssuedCertificate
		err  error
	)

	if probe {
		cert, err = f.m.IssueProbe(f.t.Context(), csr, identity)
	} else {
		cert, err = f.m.Issue(f.t.Context(), csr, identity)
	}

	if err != nil {
		f.t.Fatal(err)
	}

	return cert
}

func (f *fixture) proof(identity Identity, drained bool) Proof {
	f.t.Helper()
	issued := f.issue(identity, identity.Kind == ControlPlane)

	certs, err := parseCertificates(issued.CertificatePEM)
	if err != nil {
		f.t.Fatal(err)
	}

	s := f.state()

	uri, err := identity.URI()
	if err != nil {
		f.t.Fatal(err)
	}

	return Proof{peerFingerprint: digest(certs[0].Raw), peerURI: uri, peerRoot: issued.RootDigest, localRoot: s.proofRoot(), bundleDigest: s.bundle().Digest(), at: f.now, ack: Acknowledgment{Generation: s.Generation, Digest: s.bundle().Digest(), OldConnectionsDrained: drained}, peerIsServer: identity.Kind == ControlPlane}
}

func TestIssuanceDerivesIdentitiesAndPersists(t *testing.T) {
	f := newFixture(t)
	for _, identity := range []Identity{node("pod-a", "boot-a"), {Kind: ControlPlane, PodUID: "cp-a", BootID: "boot-a"}} {
		issued := f.issue(identity, false)

		certs, err := parseCertificates(issued.CertificatePEM)
		if err != nil {
			t.Fatal(err)
		}

		cert := certs[0]

		uri, _ := identity.URI()
		if cert.IsCA || cert.Subject.CommonName != "" || len(cert.Subject.Organization) != 0 || len(cert.IPAddresses) != 0 || len(cert.URIs) != 1 || cert.URIs[0].String() != uri {
			t.Fatalf("CSR identities escaped server template: %+v", cert)
		}

		if identity.Kind == Node {
			if len(cert.DNSNames) != 0 || len(cert.ExtKeyUsage) != 2 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || cert.ExtKeyUsage[1] != x509.ExtKeyUsageClientAuth {
				t.Fatal("wrong node SAN/EKU")
			}
		} else if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || len(cert.DNSNames) != 1 || cert.DNSNames[0] != "racer-controlplane.racer.svc" {
			t.Fatal("wrong CP SAN/EKU")
		}

		p := f.state().Members[identity.Key().String()]
		if p == nil || p.Leaves[digest(cert.Raw)].Root != issued.RootDigest {
			t.Fatal("certificate returned without durable enrollment")
		}
	}

	loaded, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err = loaded.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(f.state().Members) != 2 {
		t.Fatal("missing persisted members")
	}

	csr, _ := csrKey(t)
	if _, err = loaded.Issue(t.Context(), csr, node("pod", "boot")); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower issued: %v", err)
	}

	for _, bad := range []Identity{node("pod/override", "boot"), {Kind: Node, Universe: strings.Repeat("A", 64), Node: strings.Repeat("b", 64), PodUID: "pod", BootID: "boot"}, {Kind: ControlPlane, Node: "override", PodUID: "cp", BootID: "boot"}} {
		if _, err = f.m.Issue(t.Context(), csr, bad); err == nil {
			t.Fatal("accepted invalid derived identity")
		}
	}

	if _, err = f.m.Issue(t.Context(), []byte("malformed"), node("pod", "boot")); err == nil {
		t.Fatal("accepted malformed CSR")
	}
}

func TestRotationDurableBarriersExpiryAndRestart(t *testing.T) {
	f := newFixture(t)

	identities := []Identity{node("active", "boot"), node("idle", "boot"), node("draining", "boot"), {Kind: ControlPlane, PodUID: "takeover", BootID: "boot"}}
	for _, identity := range identities {
		f.issue(identity, false)
	}

	initial := f.state()
	old := initial.Active
	expiry := initial.Authorities[0].LastIssuedExpiry

	if err := f.m.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	s := f.state()
	if s.Phase != "overlap" || len(s.Authorities) != 2 || s.Active != old || s.Generation != 2 {
		t.Fatalf("bad overlap: %+v", s)
	}

	if err := f.m.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(f.state().Authorities) != 2 {
		t.Fatal("third root created")
	}

	for _, identity := range identities {
		if err := f.m.ObserveHeartbeat(t.Context(), identity.Key(), Acknowledgment{Generation: s.Generation, Digest: s.bundle().Digest(), OldConnectionsDrained: true}); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "overlap" {
		t.Fatal("header-only acknowledgment switched issuance")
	}

	for _, identity := range identities[:3] {
		if err := f.m.RecordTLSProof(t.Context(), identity.Key(), f.proof(identity, false)); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "overlap" {
		t.Fatal("missing CP proof did not block")
	}

	if err := f.m.RecordTLSProof(t.Context(), identities[3].Key(), f.proof(identities[3], false)); err != nil {
		t.Fatal(err)
	}
	// Leadership takeover invalidates even previously complete proofs.
	f.now = f.now.Add(time.Second)

	restarted, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err = restarted.AcquireLeadership(t.Context(), "leader-2"); err != nil {
		t.Fatal(err)
	}

	if err = f.m.Reconcile(t.Context()); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("stale leader accepted: %v", err)
	}

	f.m = restarted
	if err = f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "overlap" {
		t.Fatal("takeover reused stale proof")
	}

	for _, identity := range identities {
		if err = f.m.RecordTLSProof(t.Context(), identity.Key(), f.proof(identity, false)); err != nil {
			t.Fatal(err)
		}
	}

	if err = f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	s = f.state()
	if s.Phase != "switched" || s.Active == old || s.Generation != 3 {
		t.Fatal("issuance did not switch")
	}

	if f.issue(identities[0], false).RootDigest == old {
		t.Fatal("production leaf still issued by old root")
	}

	f.now = f.now.Add(time.Second)
	for _, identity := range identities {
		if err = f.m.RecordTLSProof(t.Context(), identity.Key(), f.proof(identity, true)); err != nil {
			t.Fatal(err)
		}
	}

	if err = f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(f.state().Authorities) != 2 {
		t.Fatal("old CA removed before old leaf expiry plus skew")
	}

	f.now = expiry.Add(f.m.options.ClockSkew).Add(time.Second)
	if err = f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(f.state().Authorities) != 2 || len(f.state().Members) != 4 {
		t.Fatal("stale heartbeat evicted members or bypassed fresh proof")
	}

	for _, identity := range identities[:3] {
		if err = f.m.RecordTLSProof(t.Context(), identity.Key(), f.proof(identity, true)); err != nil {
			t.Fatal(err)
		}
	}
	// Authoritatively retire the missing CP; all remaining processes have proof.
	if err = f.m.Retire(t.Context(), identities[3].Key()); err != nil {
		t.Fatal(err)
	}

	if err = f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	s = f.state()
	if s.Phase != "stable" || len(s.Authorities) != 1 || s.Generation != 4 || s.Active == old {
		t.Fatal("rotation failed to complete")
	}

	if err = f.m.Admit(t.Context(), identities[3]); err == nil {
		t.Fatal("retired boot re-admitted")
	}

	if err = f.m.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestProofCannotCrossBootOrAcceptWrongRoot(t *testing.T) {
	f := newFixture(t)
	id := node("pod", "boot-a")
	f.issue(id, false)

	if err := f.m.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	proof := f.proof(id, false)
	other := id
	other.BootID = "boot-b"
	f.issue(other, true)

	if err := f.m.RecordTLSProof(t.Context(), other.Key(), proof); err == nil {
		t.Fatal("proof crossed boot identity")
	}

	bad := proof

	bad.localRoot = f.state().Active
	if err := f.m.RecordTLSProof(t.Context(), id.Key(), bad); err == nil {
		t.Fatal("old-root session accepted")
	}

	bad = proof

	bad.ack.Digest = strings.Repeat("0", 64)
	if err := f.m.RecordTLSProof(t.Context(), id.Key(), bad); err == nil {
		t.Fatal("wrong bundle accepted")
	}

	if err := f.m.RecordTLSProof(t.Context(), id.Key(), Proof{}); err == nil {
		t.Fatal("zero proof accepted")
	}

	if err := f.m.RecordTLSProof(t.Context(), id.Key(), proof); err != nil {
		t.Fatal(err)
	}

	if err := f.m.RecordTLSProof(t.Context(), id.Key(), proof); err == nil {
		t.Fatal("replayed proof accepted")
	}
}

func TestBootstrapRefusesLostOrMalformedState(t *testing.T) {
	for _, mode := range []string{"lost-secret", "malformed-secret", "malformed-bundle", "empty-tombstone"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			secret := &corev1.Secret{}
			cm := &corev1.ConfigMap{}

			if err := f.c.Get(t.Context(), f.m.objectKey(SecretName), secret); err != nil {
				t.Fatal(err)
			}

			if err := f.c.Get(t.Context(), f.m.objectKey(ConfigMapName), cm); err != nil {
				t.Fatal(err)
			}

			switch mode {
			case "lost-secret", "empty-tombstone":
				if err := f.c.Delete(t.Context(), secret); err != nil {
					t.Fatal(err)
				}

				if mode == "empty-tombstone" {
					cm.Data = map[string]string{}
					if err := f.c.Update(t.Context(), cm); err != nil {
						t.Fatal(err)
					}
				}
			case "malformed-secret":
				secret.Data[StateKey] = []byte(`{"version":1}`)
				if err := f.c.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			case "malformed-bundle":
				cm.Data[BundleKey] = "garbage"
				if err := f.c.Update(t.Context(), cm); err != nil {
					t.Fatal(err)
				}
			}

			m, err := New(f.c, "racer", f.m.options)
			if err != nil {
				t.Fatal(err)
			}

			err = m.AcquireLeadership(t.Context(), "next-leader")
			if mode == "malformed-bundle" && err == nil {
				err = m.Reconcile(t.Context())
			}

			if err == nil {
				t.Fatal("silently regenerated or repaired corrupt trust")
			}
		})
	}
}

func TestCASRetriesAndFencesConcurrentTakeover(t *testing.T) {
	f := newFixture(t)

	var updates atomic.Int32

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == SecretName && updates.Add(1) == 1 {
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, SecretName, errors.New("injected conflict"))
		}

		return c.Update(ctx, obj, opts...)
	}})
	if err := f.m.Admit(t.Context(), node("pod", "boot")); err != nil {
		t.Fatal(err)
	}

	if updates.Load() != 2 {
		t.Fatalf("CAS was not retried: %d", updates.Load())
	}

	updates.Store(0)

	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == SecretName && updates.Add(1) == 1 {
			next, err := New(f.c, "racer", f.m.options)
			if err != nil {
				return err
			}

			if err = next.AcquireLeadership(ctx, "next-fence"); err != nil {
				return err
			}
		}

		return c.Update(ctx, obj, opts...)
	}})
	if err := f.m.Admit(t.Context(), node("stale", "boot")); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("stale writer survived takeover: %v", err)
	}

	if f.state().Members[node("stale", "boot").Key().String()] != nil {
		t.Fatal("stale state committed")
	}
}

func TestInterruptedPublicationResumesAndScheduledTrigger(t *testing.T) {
	f := newFixture(t)
	f.m.client = interceptor.NewClient(f.c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == ConfigMapName {
			return errors.New("publication unavailable")
		}

		return c.Update(ctx, obj, opts...)
	}})

	f.now = f.state().NextRotation
	if err := f.m.Reconcile(t.Context()); err == nil {
		t.Fatal("expected publication failure")
	}

	if f.state().Phase != "overlap" {
		t.Fatal("rotation root was not persisted first")
	}

	csr, _ := csrKey(t)
	if _, err := f.m.Issue(t.Context(), csr, node("pod", "boot")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("issued ahead of publication: %v", err)
	}

	m, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err = m.AcquireLeadership(t.Context(), "restart"); err != nil {
		t.Fatal(err)
	}

	if err = m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err = m.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledLeaderCannotWrite(t *testing.T) {
	f := newFixture(t)

	m, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	if err = m.AcquireLeadership(ctx, "cancelable"); err != nil {
		t.Fatal(err)
	}

	cancel()

	if err = m.Admit(t.Context(), node("pod", "boot")); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("canceled leader wrote: %v", err)
	}
}

func TestTrustBundleWireSchemaAndMalformed(t *testing.T) {
	f := newFixture(t)

	b := f.state().bundle()
	if !strings.HasPrefix(string(b.JSON()), `{"version":1,"generation":1,"active":"`) {
		t.Fatalf("unexpected wire JSON: %s", b.JSON())
	}

	for _, bad := range [][]byte{[]byte(`{}`), append(b.JSON(), []byte(` {}`)...), []byte(strings.Replace(string(b.JSON()), `"version":1`, `"version":2`, 1)), []byte(strings.Replace(string(b.JSON()), `"generation":1`, `"generation":0`, 1)), []byte(strings.Replace(string(b.JSON()), `"version":1`, `"extra":1,"version":1`, 1))} {
		if _, err := ParseBundle(bad); err == nil {
			t.Fatalf("accepted malformed bundle %s", bad)
		}
	}

	b.Certificates += b.Certificates
	if _, err := ParseBundle(b.JSON()); err == nil {
		t.Fatal("accepted duplicate root")
	}
}

func hotFor(t *testing.T, m *Manager, id Identity, probe bool) (*HotTLS, IssuedCertificate, []byte) {
	t.Helper()
	csr, key := csrKey(t)

	var (
		issued IssuedCertificate
		err    error
	)

	if probe {
		issued, err = m.IssueProbe(t.Context(), csr, id)
	} else {
		issued, err = m.Issue(t.Context(), csr, id)
	}

	if err != nil {
		t.Fatal(err)
	}

	h := NewHotTLS()
	if err = h.Update(issued.Bundle.JSON(), issued.CertificatePEM, key); err != nil {
		t.Fatal(err)
	}

	return h, issued, key
}

func TestHotTLSRetainsMalformedUpdatesAndFreshProof(t *testing.T) {
	f := newFixture(t)
	// Production TLS uses real wall time; avoid test clock truncation at the fence.
	f.m.options.Now = time.Now
	clientID := node("probe", "boot")
	serverID := Identity{Kind: ControlPlane, PodUID: "cp", BootID: "boot"}
	clientHot, _, _ := hotFor(t, f.m, clientID, false)
	serverHot, issued, key := hotFor(t, f.m, serverID, false)

	before, _ := serverHot.snapshot()
	for _, update := range []struct{ bundle, cert, key []byte }{{[]byte("garbage"), issued.CertificatePEM, key}, {issued.Bundle.JSON(), []byte("garbage"), key}, {issued.Bundle.JSON(), issued.CertificatePEM, []byte("garbage")}} {
		if err := serverHot.Update(update.bundle, update.cert, update.key); err == nil {
			t.Fatal("malformed update accepted")
		}

		after, _ := serverHot.snapshot()
		if after != before {
			t.Fatal("malformed update replaced working context")
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	type serverResult struct {
		proof Proof
		err   error
	}

	result := make(chan serverResult, 1)
	ack := Acknowledgment{Generation: issued.Bundle.Generation, Digest: issued.Bundle.Digest(), OldConnectionsDrained: true}

	go func() {
		_, finish, err := serverHot.HandshakeProof(ctx, a, true, "")
		if err != nil {
			result <- serverResult{err: err}
			return
		}

		proof, err := finish(ack)
		result <- serverResult{proof: proof, err: err}
	}()

	_, finish, err := clientHot.HandshakeProof(ctx, b, false, "racer-controlplane.racer.svc")
	if err != nil {
		t.Fatal(err)
	}

	proof, err := finish(ack)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.m.RecordTLSProof(t.Context(), serverID.Key(), proof); err != nil {
		t.Fatal(err)
	}

	if _, err = finish(ack); err == nil {
		t.Fatal("finish reused")
	}

	server := <-result
	if server.err != nil {
		t.Fatal(server.err)
	}

	if err = f.m.RecordTLSProof(t.Context(), clientID.Key(), server.proof); err != nil {
		t.Fatal(err)
	}

	if serverHot.ServerConfig(tls.RequireAndVerifyClientCert).GetConfigForClient == nil {
		t.Fatal("server context is not hot")
	}

	if err = f.m.TriggerRotation(t.Context()); err != nil {
		t.Fatal(err)
	}

	newBundle, err := f.m.Bundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if err = serverHot.Update(newBundle.JSON(), issued.CertificatePEM, key); err != nil {
		t.Fatal(err)
	}

	if err = serverHot.Update(issued.Bundle.JSON(), issued.CertificatePEM, key); err == nil {
		t.Fatal("trust rollback accepted")
	}
}

func TestTLSProofRejectsUnauthenticatedPeer(t *testing.T) {
	f := newFixture(t)
	f.m.options.Now = time.Now
	h, _, _ := hotFor(t, f.m, node("client", "boot"), false)
	server, _, _ := hotFor(t, f.m, Identity{Kind: ControlPlane, PodUID: "cp", BootID: "boot"}, false)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)

	go func() { done <- tls.Server(a, server.ServerConfig(tls.NoClientCert)).HandshakeContext(ctx) }()

	if _, _, err := h.HandshakeProof(ctx, b, false, "wrong-identity.racer.svc"); err == nil {
		t.Fatal("wrong server identity supplied a proof")
	}

	if err := <-done; err == nil {
		t.Fatal("server accepted rejected handshake")
	}
}

func TestBootstrapTombstonePreventsConcurrentCreation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "racer"}}).Build()

	m, err := New(c, "racer", Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err = m.AcquireLeadership(t.Context(), "leader"); !errors.Is(err, ErrLostState) {
		t.Fatalf("ignored bootstrap tombstone: %v", err)
	}
}

func TestOverlapProofTransport(t *testing.T) {
	for _, nodeProof := range []bool{false, true} {
		t.Run(map[bool]string{false: "CP-server-auth-only", true: "old-node-client-pending-server"}[nodeProof], func(t *testing.T) {
			f := newFixture(t)
			f.m.options.Now = time.Now

			clientID := Identity{Kind: ControlPlane, PodUID: "leader", BootID: "boot"}
			if nodeProof {
				clientID = node("node", "boot")
			}

			h, old, key := hotFor(t, f.m, clientID, false)
			if err := f.m.TriggerRotation(t.Context()); err != nil {
				t.Fatal(err)
			}

			serverID := Identity{Kind: ControlPlane, PodUID: "replica", BootID: "boot"}

			server, pending, _ := hotFor(t, f.m, serverID, true)
			if err := h.Update(pending.Bundle.JSON(), old.CertificatePEM, key); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()

			done := make(chan error, 1)
			ack := Acknowledgment{Generation: pending.Bundle.Generation, Digest: pending.Bundle.Digest()}

			go func() {
				if !nodeProof {
					done <- tls.Server(a, server.ServerConfig(tls.NoClientCert)).HandshakeContext(ctx)
					return
				}

				_, finish, err := server.HandshakeProof(ctx, a, true, "")
				if err != nil {
					done <- err
					return
				}

				proof, err := finish(ack)
				if err == nil {
					err = f.m.RecordTLSProof(ctx, clientID.Key(), proof)
				}

				done <- err
			}()

			conn, finish, err := h.HandshakeProof(ctx, b, false, "racer-controlplane.racer.svc")
			if err != nil {
				t.Fatal(err)
			}

			if conn.ConnectionState().NegotiatedProtocol != "http/1.1" {
				t.Fatal("unexpected ALPN")
			}

			if err = <-done; err != nil {
				t.Fatal(err)
			}

			if !nodeProof {
				proof, err := finish(ack)
				if err != nil {
					t.Fatal(err)
				}

				if err = f.m.RecordTLSProof(ctx, serverID.Key(), proof); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestBootstrapRefusesExistingDeploymentState(t *testing.T) {
	for _, object := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-state", Namespace: "racer", Labels: map[string]string{"racer.unbounded-cloud.io/state": "commit"}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-replica-old", Namespace: "racer"}, Data: map[string]string{"certificate": "old-cert"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-replica-proof", Namespace: "racer"}, Data: map[string]string{"proof-certificate": "old-cert"}},
	} {
		t.Run(object.GetName(), func(t *testing.T) {
			f := newFixture(t)
			if err := f.c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: "racer"}}); err != nil {
				t.Fatal(err)
			}

			if err := f.c.Delete(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "racer"}}); err != nil {
				t.Fatal(err)
			}

			if err := f.c.Create(t.Context(), object); err != nil {
				t.Fatal(err)
			}

			m, err := New(f.c, "racer", f.m.options)
			if err != nil {
				t.Fatal(err)
			}

			if err = m.AcquireLeadership(t.Context(), "replacement"); !errors.Is(err, ErrLostState) {
				t.Fatalf("bootstrapped existing deployment: %v", err)
			}
		})
	}
}

func TestPublishMembersAndRotationNonce(t *testing.T) {
	f := newFixture(t)

	id := Identity{Kind: ControlPlane, PodUID: "pending-pod", BootID: "pending"}
	if err := f.m.Admit(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	members, err := f.m.Members(t.Context())
	if err != nil || len(members) != 1 || members[0] != id {
		t.Fatalf("members: %v %v", members, err)
	}

	cm := &corev1.ConfigMap{}
	if err = f.c.Get(t.Context(), f.m.objectKey(ConfigMapName), cm); err != nil {
		t.Fatal(err)
	}

	cm.Annotations[RotationAnnotation] = "operator-1"
	if err = f.c.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}

	if err = f.m.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "stable" {
		t.Fatal("Publish advanced rotation")
	}

	if err = f.m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().RotationNonce != "operator-1" || f.state().Phase != "overlap" {
		t.Fatal("nonce not committed with rotation")
	}

	if err = f.m.Retire(t.Context(), id.Key()); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err = f.m.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if f.state().Phase != "stable" || f.state().Generation != 4 {
		t.Fatal("nonce retriggered")
	}

	restarted, err := New(f.c, "racer", f.m.options)
	if err != nil {
		t.Fatal(err)
	}

	if err = restarted.AcquireLeadership(t.Context(), "leader-2"); err != nil {
		t.Fatal(err)
	}

	f.now = f.state().NextRotation.Add(time.Second)

	if err = restarted.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	if f.state().Phase != "overlap" {
		t.Fatal("handled nonce blocked scheduled rotation")
	}
}
