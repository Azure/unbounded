// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("RACER_CLUSTER_ID", testOtherUID)
	t.Setenv("POD_NAMESPACE", "racer")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func testTopology(t *testing.T, objects ...client.Object) *TopologyReconciler {
	t.Helper()
	cfg := testConfig(t)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, racerv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation-uid"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh"}})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithIndex(&corev1.Pod{}, podNodeIndex, podNodeKeys).Build()

	return Assemble(cfg, c, c).Topology
}

func initializedTopology(t *testing.T, objects ...client.Object) *TopologyReconciler {
	t.Helper()

	r := testTopology(t, objects...)
	if err := r.authority.Recover(t.Context(), r.Client); err != nil {
		t.Fatal(err)
	}

	return r
}

// Captured publication data is decoded through the public response operation,
// never an installation proof. It belongs exclusively to the integration test.
type (
	CommittedPublication struct {
		handle     *authority.PublicationHandle
		encoded    string
		delta      string
		deltaBase  string
		record     VersionRecord
		leadership context.Context
	}
	VersionRecord struct {
		Cluster           wire.ClusterID
		Sequence          wire.Sequence
		MembershipVersion wire.MembershipVersion
		ContentHash       string
		MembershipHash    string
	}
)

func capturePublication(t *testing.T, a *authority.Authority) *CommittedPublication {
	t.Helper()

	h, err := a.Current()
	if err != nil {
		t.Fatal(err)
	}

	return captureHandle(t, h)
}

func captureHandle(t *testing.T, h *authority.PublicationHandle) *CommittedPublication {
	t.Helper()

	var b bytes.Buffer

	ctx, cancel, err := h.WriteContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	if _, err := h.ForBase("").WriteTo(ctx, &b); err != nil {
		t.Fatal(err)
	}

	p, err := wire.DecodePublication(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	content, members, err := wire.ContentHashes(p)
	if err != nil {
		t.Fatal(err)
	}

	return &CommittedPublication{handle: h, encoded: b.String(), record: VersionRecord{Cluster: p.Cluster, Sequence: p.Sequence, MembershipVersion: p.MembershipVersion, ContentHash: content, MembershipHash: members}, leadership: ctx}
}

func (p *CommittedPublication) writeContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	return p.handle.WriteContext(ctx)
}

func reconcileTopology(t *testing.T, r *TopologyReconciler, ctx context.Context) *CommittedPublication {
	t.Helper()

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	return capturePublication(t, r.authority)
}

func acceptedMembers(t *testing.T, r *TopologyReconciler) AcceptedMembers {
	t.Helper()

	p, err := r.authority.Current()
	if err != nil {
		return nil
	}

	captured := captureHandle(t, p)

	image, err := wire.DecodePublication(bytes.NewBufferString(captured.encoded))
	if err != nil {
		t.Fatal(err)
	}

	members := make(AcceptedMembers, len(image.Members))
	for _, m := range image.Members {
		members[m.Node] = m
	}

	return members
}

func runKeys(t *testing.T, r *KeyringReconciler) ctrl.Result {
	t.Helper()

	result, err := r.Reconcile(t.Context(), ctrl.Request{})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	return result
}

type (
	RotationState struct {
		NextRotation   time.Time            `json:"next_rotation"`
		ActivateAt     time.Time            `json:"activate_at"`
		ActiveIssuer   string               `json:"active_issuer"`
		PreparedIssuer string               `json:"prepared_issuer"`
		Retiring       map[string]time.Time `json:"retiring"`
	}
	signingMaterial struct {
		PrivateKey  []byte `json:"private_key"`
		Certificate []byte `json:"certificate"`
	}
	issuerMaterial struct {
		Keys map[string]signingMaterial `json:"keys"`
	}
)

func keyState(t *testing.T, r *KeyringReconciler) (*corev1.Secret, wire.KeyringBundle, RotationState, issuerMaterial) {
	t.Helper()

	var secret corev1.Secret

	deps := fixtureDependencies[r.authority]
	if err := deps.reader.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}, &secret); err != nil {
		t.Fatal(err)
	}

	b, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	if err != nil {
		t.Fatal(err)
	}

	var state RotationState
	if err := json.Unmarshal(secret.Data["rotation.json"], &state); err != nil {
		t.Fatal(err)
	}

	var material issuerMaterial
	if err := json.Unmarshal(secret.Data["issuer.json"], &material); err != nil {
		t.Fatal(err)
	}

	return &secret, b, state, material
}

// Fault injection is a Kubernetes dependency supplied at construction, not an
// authority mutation hook. Tests may change the transport under that dependency.
type fixtureDependency struct {
	client.Client
	reader client.Reader
	now    func() time.Time
}

func (d *fixtureDependency) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return d.reader.Get(ctx, key, obj, opts...)
}

func (d *fixtureDependency) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return d.reader.List(ctx, list, opts...)
}

var fixtureDependencies = map[*authority.Authority]*fixtureDependency{}

func assembleFixture(cfg Config, c client.Client, reader client.Reader) *Application {
	d := &fixtureDependency{Client: c, reader: reader, now: time.Now}
	a := Assemble(cfg, d, d)
	// Supply a clock through construction; no setter is exposed by authority.
	owner := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: d, Reader: d, Now: func() time.Time { return d.now() }})
	a.authority = owner
	a.Topology.authority = owner
	a.Keyring.authority = owner
	a.Server.authority = owner
	a.Replication.authority = owner
	a.Lifecycle.authority = owner
	a.Topology.Client = c
	a.Topology.APIReader = reader
	a.Replication.Client = c
	a.Replication.APIReader = reader
	fixtureDependencies[owner] = d

	return a
}

func decodeIssuedResponse(t *testing.T, encoded []byte) wire.BootstrapResponse {
	t.Helper()

	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	return response
}

func issuanceRequest(t *testing.T, r *KeyringReconciler) (NodeIdentity, wire.BootstrapRequest, ed25519.PublicKey) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}

	return NodeIdentity{}, wire.BootstrapRequest{SchemaVersion: 1, Cluster: r.Config.Cluster, Enrollment: testOtherUID, CSRDER: csr, Shares: wire.DefaultShares}, pub
}

func fixtureIdentity(t *testing.T, f *servingFixture) NodeIdentity {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)

	identity, err := f.a.authority.Authenticate(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}

	return identity
}

func (s *Server) runtimeFixtureConfig() Config { s.initializeAdmission(); return s.config }

type podAuthorizationReader struct {
	client.Reader
	pod *corev1.Pod
}

func (r podAuthorizationReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch value := obj.(type) {
	case *corev1.Pod:
		*value = *r.pod.DeepCopy()
		return nil
	case *corev1.Node:
		*value = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: r.pod.Spec.NodeName, UID: testNodeUID}}
		return nil
	default:
		return r.Reader.Get(ctx, key, obj, opts...)
	}
}

type reviewWriter struct {
	client.Writer
	status authv1.TokenReviewStatus
}

func (w reviewWriter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if review, ok := obj.(*authv1.TokenReview); ok {
		review.Status = w.status
		return ctx.Err()
	}

	return w.Writer.Create(ctx, obj, opts...)
}

// Preserve authorization-only scenarios through public token authentication.
func authorizePod(ctx context.Context, reader client.Reader, cfg Config, pod *corev1.Pod, saUID string) error {
	status := authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{wire.TokenAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:" + cfg.Namespace + ":" + cfg.DataplaneServiceAccount, UID: saUID, Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}, "authentication.kubernetes.io/node-name": {pod.Spec.NodeName}, "authentication.kubernetes.io/node-uid": {testNodeUID}}}}
	if pod.UID == "" {
		status.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"missing"}
	}

	a := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: reviewWriter{status: status}, Reader: podAuthorizationReader{Reader: reader, pod: pod}})
	token := "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".signature"
	r := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	_, err := a.Authenticate(ctx, r)

	return err
}

func invalidateFixtureTrust(t *testing.T, f *servingFixture) {
	t.Helper()
	secret, _, _, _ := keyState(t, f.a.Keyring)

	secret.Data["bundle.json"] = []byte(`{}`)
	if err := f.a.Topology.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	if err := f.a.authority.Observe(t.Context()); err == nil {
		t.Fatal("invalid credential observation succeeded")
	}
}

const (
	installationUIDAnnotation = "racer.unbounded-cloud.io/installation-uid"
	credentialClaim           = "racer.unbounded-cloud.io/credentials"
)

func readInstallation(ctx context.Context, reader client.Reader, cfg Config, fresh bool) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap

	err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName}, &cm)

	return &cm, err
}

func readVersion(ctx context.Context, reader client.Reader, cfg Config) (*corev1.ConfigMap, VersionRecord, error) {
	if err := authority.ValidateInstallation(ctx, reader, cfg.Namespace, string(cfg.Cluster)); err != nil {
		return nil, VersionRecord{}, err
	}

	var cm corev1.ConfigMap
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}, &cm); err != nil {
		return nil, VersionRecord{}, err
	}

	seq, err := strconv.ParseUint(cm.Data["sequence"], 10, 64)
	if err != nil {
		return nil, VersionRecord{}, err
	}

	members, err := strconv.ParseUint(cm.Data["membership_version"], 10, 64)
	if err != nil {
		return nil, VersionRecord{}, err
	}

	return &cm, VersionRecord{Cluster: wire.ClusterID(cm.Data["cluster"]), Sequence: wire.Sequence(seq), MembershipVersion: wire.MembershipVersion(members), ContentHash: cm.Data["content_hash"], MembershipHash: cm.Data["membership_hash"]}, nil
}

func ensureInstalled(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config) error {
	return authority.New(cfg.authorityConfig(), authority.Dependencies{Reader: reader, Writer: writer}).Recover(ctx, writer)
}

func containsRoot(b wire.KeyringBundle, id string) bool {
	for _, root := range b.PeerTrustRoots {
		sum := sha256.Sum256(root)
		if hex.EncodeToString(sum[:]) == id {
			return true
		}
	}

	return false
}

func configureFixtureAge(t *testing.T, f *servingFixture, age time.Duration) {
	t.Helper()

	cfg := f.a.Server.Config
	cfg.SnapshotMaxAge = age
	d := fixtureDependencies[f.a.authority]
	a := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: d, Reader: d, Now: func() time.Time { return d.now() }})
	f.a.authority = a
	f.a.Topology.authority = a
	f.a.Keyring.authority = a
	f.a.Server.authority = a
	f.a.Replication.authority = a
	f.a.Lifecycle.authority = a

	fixtureDependencies[a] = d
	if err := a.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}

	reconcileTopology(t, f.a.Topology, f.ctx)
}

func replaceFixtureCredentials(t *testing.T, f *servingFixture) {
	t.Helper()
	other := newServingFixture(t)
	candidate, _, _, _ := keyState(t, other.a.Keyring)
	current, bundle, _, _ := keyState(t, f.a.Keyring)

	var (
		replacement wire.KeyringBundle
		err         error
	)

	replacement, err = wire.DecodeBundle(bytes.NewReader(candidate.Data["bundle.json"]))
	if err != nil {
		t.Fatal(err)
	}

	replacement.Generation = bundle.Generation + 1

	candidate.Data["bundle.json"], err = wire.EncodeBundle(replacement)
	if err != nil {
		t.Fatal(err)
	}

	current.Data = candidate.Data
	if err := f.a.Topology.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}

	if err := f.a.authority.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}
}

var withdrawnSecrets = map[*authority.Authority]*corev1.Secret{}

func withdrawServerTrust(t *testing.T, s *Server) {
	t.Helper()

	d := fixtureDependencies[s.authority]

	var secret corev1.Secret
	if err := d.Client.Get(t.Context(), client.ObjectKey{Namespace: s.Config.Namespace, Name: s.Config.CredentialsSecretName}, &secret); err != nil {
		t.Fatal(err)
	}

	if withdrawnSecrets[s.authority] == nil {
		withdrawnSecrets[s.authority] = secret.DeepCopy()
	}

	secret.Data["bundle.json"] = []byte(`{}`)
	if err := d.Update(t.Context(), &secret); err != nil {
		t.Fatal(err)
	}

	if _, err := s.authority.ReconcileCredentials(t.Context()); err == nil {
		t.Fatal("invalid trust accepted")
	}
}

func restoreServerTrust(t *testing.T, s *Server) {
	t.Helper()

	d := fixtureDependencies[s.authority]

	saved := withdrawnSecrets[s.authority]
	if saved == nil {
		return
	}

	var secret corev1.Secret
	if err := d.Client.Get(t.Context(), client.ObjectKeyFromObject(saved), &secret); err != nil {
		t.Fatal(err)
	}

	secret.Data = saved.DeepCopy().Data
	if err := d.Update(t.Context(), &secret); err != nil {
		t.Fatal(err)
	}

	if _, err := s.authority.ReconcileCredentials(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func withdrawPublication(t *testing.T, r *TopologyReconciler) func() {
	t.Helper()

	cm, _, err := readVersion(t.Context(), r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	saved := cm.DeepCopy()

	cm.Data["sequence"] = "0"
	if err := r.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}

	if _, err := r.authority.PublishTopology(t.Context(), r.observeTopology); err == nil {
		t.Fatal("invalid publication accepted")
	}

	return func() {
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(cm), cm); err != nil {
			t.Fatal(err)
		}

		cm.Data = saved.Data
		if err := r.Update(t.Context(), cm); err != nil {
			t.Fatal(err)
		}
	}
}

type capturedResponse struct{ encoded string }

func (p *CommittedPublication) ForBase(hash string) capturedResponse {
	if hash != "" && hash == p.deltaBase {
		return capturedResponse{p.delta}
	}

	return capturedResponse{p.encoded}
}

func (p capturedResponse) writeTo(ctx context.Context, w io.Writer) (int64, error) {
	var total int64

	for rest := p.encoded; rest != ""; {
		n, err := requestWriter{ctx: ctx, writer: w}.Write([]byte(rest[:min(len(rest), 32768)]))

		total += int64(n)
		if err != nil {
			return total, err
		}

		rest = rest[n:]
	}

	return total, ctx.Err()
}

func versionData(v VersionRecord) map[string]string {
	return map[string]string{"cluster": string(v.Cluster), "sequence": strconv.FormatUint(uint64(v.Sequence), 10), "membership_version": strconv.FormatUint(uint64(v.MembershipVersion), 10), "content_hash": v.ContentHash, "membership_hash": v.MembershipHash}
}

func advanceFixturePublication(t *testing.T, r *TopologyReconciler) {
	t.Helper()

	members := AcceptedMembers{testNodeUID: {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}}
	replicationSmokePublish(t, t.Context(), r, members)
}

func parseSigning(m signingMaterial) (*x509.Certificate, ed25519.PrivateKey, error) {
	cert, err := x509.ParseCertificate(m.Certificate)
	if err != nil {
		return nil, nil, err
	}

	key, err := x509.ParsePKCS8PrivateKey(m.PrivateKey)
	if err != nil {
		return nil, nil, err
	}

	return cert, key.(ed25519.PrivateKey), nil
}

func waitFixturePublication(ctx context.Context, a *authority.Authority, after wire.Sequence) (*authority.PublicationHandle, error) {
	for {
		p, changed, err := a.CurrentAndSubscribe()
		if err != nil {
			return nil, err
		}

		if p.Sequence() > after {
			return p, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func rootID(der []byte) string { sum := sha256.Sum256(der); return hex.EncodeToString(sum[:]) }
func generateIssuer(now time.Time, cfg Config) ([]byte, []byte, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(cfg.Rotation.Interval + cfg.Rotation.PrepareFor + cfg.Rotation.RetainFor + 2*cfg.CertificateLifetime), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		return nil, nil, err
	}

	encoded, err := x509.MarshalPKCS8PrivateKey(key)

	return der, encoded, err
}

func largeFixturePublication(t *testing.T, f *servingFixture) {
	t.Helper()

	members := make(AcceptedMembers, 50000)

	for i := range 50000 {
		id := wire.NodeID(fmt.Sprintf("33333333-3333-4333-8333-%012d", i))
		members[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}
	}

	replicationSmokePublish(t, f.ctx, f.a.Topology, members)
}

func testKeyring(t *testing.T) (*KeyringReconciler, *time.Time) {
	t.Helper()

	cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: testNodeUID}}
	r := initializedTopology(t, cache)
	a := assembleFixture(r.Config, r.Client, r.APIReader)
	now := time.Now().UTC().Truncate(time.Second)
	fixtureDependencies[a.authority].now = func() time.Time { return now }

	return a.Keyring, &now
}

func holdFixtureGate(t *testing.T, f *servingFixture) func() {
	t.Helper()

	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		_, err := f.a.authority.PublishTopology(t.Context(), func(context.Context) (TopologyObservation, error) {
			close(entered)
			<-release

			return TopologyObservation{}, wire.Unavailable
		})
		if err == nil {
			t.Error("failed discovery succeeded")
		}
	}()

	<-entered

	return func() { close(release); <-done }
}

func rotateFixtureTrust(t *testing.T, f *servingFixture) {
	t.Helper()
	secret, bundle, _, _ := keyState(t, f.a.Keyring)
	bundle.Generation++

	encoded, err := wire.EncodeBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}

	secret.Data["bundle.json"] = encoded
	if err := f.a.Topology.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}

	if _, err := f.a.authority.ReconcileCredentials(t.Context()); err != nil {
		t.Fatal(err)
	}
}
