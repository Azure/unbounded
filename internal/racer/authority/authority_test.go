// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestAuthorityOperationsSharePrivateGate(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	_, err := a.PublishTopology(ctx, func(ctx context.Context) (TopologyObservation, error) {
		blocked, stop := context.WithCancel(ctx)
		stop()

		_, err := a.ReconcileCredentials(blocked)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, a.Observe(blocked), context.Canceled)
		_, err = a.TrustPool()
		require.NoError(t, err, "failed admission must not invalidate trust")

		select {
		case <-a.gate.token:
			t.Fatal("discovery callback escaped operation gate")
		default:
		}

		return f.a.Topology.observeTopology(ctx)
	})
	require.NoError(t, err)
	require.NoError(t, a.Observe(ctx), "returned hints must not retain gate")
}

func TestAuthorityPublicationHistoryIsOperationOwned(t *testing.T) {
	f := newServingFixture(t)
	a, r := f.a.authority, f.a.Topology
	before := cloneAccepted(a.accepted)
	update, err := a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err)

	member := update.Members[testNodeUID]
	member.Shares = 999
	update.Members[testNodeUID] = member

	require.Equal(t, before, a.accepted, "returned hints alias authority history")

	update, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err)
	delete(update.Members, testNodeUID)
	require.Equal(t, before, a.accepted, "annotation hints alias authority history")

	var node corev1.Node
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
	node.Annotations = map[string]string{wire.SharesAnnotation: "7"}
	require.NoError(t, r.Update(t.Context(), &node))
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		return apierrors.NewConflict(corev1.Resource("configmaps"), "version", wire.Conflict)
	}})
	a.publisher.Writer = r.Client
	_, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.True(t, apierrors.IsConflict(err))
	require.Equal(t, before, a.accepted, "failed CAS advanced history")

	r.Client = base
	a.publisher.Writer = base
	_, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err)
	require.EqualValues(t, 7, a.accepted[testNodeUID].Shares)
}

func TestAuthorityHandlesRetainRevocationSemantics(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	image, err := a.Current()
	require.NoError(t, err)
	write, stop, err := image.Admit(t.Context())
	require.NoError(t, err)

	defer stop()

	trust, stopTrust, err := a.AdmitTrust(t.Context())
	require.NoError(t, err)

	defer stopTrust()

	deadline, _ := trust.Context().Deadline()

	require.NoError(t, a.Observe(t.Context()))

	nextDeadline, _ := trust.Context().Deadline()
	require.Equal(t, deadline, nextDeadline)
	require.NoError(t, trust.Check(t.Context()))

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
	node.Annotations = map[string]string{wire.SharesAnnotation: "7"}
	require.NoError(t, f.a.Topology.Update(t.Context(), &node))
	_, err = a.PublishTopology(t.Context(), f.a.Topology.observeTopology)
	require.NoError(t, err)
	require.NoError(t, write.Check(t.Context()), "replacement must preserve admitted image")
	_, _, err = image.Admit(t.Context())
	require.ErrorIs(t, err, wire.Unavailable, "old image cannot admit new responses")
	require.NoError(t, trust.Check(t.Context()), "publication replacement is not trust invalidation")

	secret := &corev1.Secret{}
	require.NoError(t, f.a.Topology.Get(t.Context(), client.ObjectKey{Namespace: a.config.Namespace, Name: a.config.CredentialsSecretName}, secret))
	require.NoError(t, f.a.Topology.Delete(t.Context(), secret))
	require.Error(t, a.Observe(t.Context()))
	require.ErrorIs(t, trust.Check(t.Context()), context.Canceled)
	require.ErrorIs(t, write.Check(t.Context()), context.Canceled)
}

func TestAuthorityObservationFailureDoesNotPublish(t *testing.T) {
	r := initializedTopology(t)
	a := r.authority
	boom := errors.New("discovery failed")
	_, err := a.PublishTopology(t.Context(), func(context.Context) (TopologyObservation, error) { return TopologyObservation{}, boom })
	require.ErrorIs(t, err, boom)
	_, err = a.Current()
	require.ErrorIs(t, err, wire.Unavailable)
	require.Empty(t, a.accepted)
	_, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err, "failed callback did not release gate")
}

func TestAuthorityBlockedOperationsHonorCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		a := f.a.authority

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		results := make(chan error, 4)
		_, err := a.PublishTopology(t.Context(), func(ctx context.Context) (TopologyObservation, error) {
			blocked, cancelBlocked := context.WithCancel(ctx)
			defer cancelBlocked()

			go func() { _, err := a.ReconcileCredentials(blocked); results <- err }()
			go func() { results <- a.Observe(blocked) }()
			go func() {
				_, err := a.PublishTopology(blocked, func(context.Context) (TopologyObservation, error) {
					t.Error("blocked publication entered discovery")
					return TopologyObservation{}, nil
				})
				results <- err
			}()
			go func() {
				_, err := a.Issue(blocked, NodeIdentity{owner: a, bearer: true, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}, f.request)
				results <- err
			}()

			synctest.Wait()

			select {
			case err := <-results:
				t.Fatalf("operation bypassed held gate: %v", err)
			default:
			}

			cancelBlocked()

			for range 4 {
				require.ErrorIs(t, <-results, context.Canceled)
			}

			return f.a.Topology.observeTopology(ctx)
		})
		require.NoError(t, err)
		require.NoError(t, a.TrustReady(), "canceled waiters invalidated accepted trust")
		require.NoError(t, a.Observe(ctx))
	})
}

func TestAuthorityConstructorCopiesConfigWithoutIO(t *testing.T) {
	cfg := testConfig(t)
	a := New(cfg, Dependencies{})
	cfg.Cluster = ""
	require.NotEqual(t, cfg.Cluster, a.config.Cluster)
	require.ErrorIs(t, a.PublicationReady(), wire.Unavailable)
	require.ErrorIs(t, a.TrustReady(), wire.Unavailable)
	require.Empty(t, a.accepted)
}

// Fixtures inject faults into real Authority operations without controller policy.
type topologyFixture struct {
	client.Client
	APIReader    client.Reader
	Config       Config
	Publications *Publications
	Trust        *Trust
	authority    *Authority
}
type credentialsFixture struct {
	client.Client
	APIReader client.Reader
	Config    Config
	Trust     *Trust
	Now       func() time.Time
	authority *Authority
}
type Application struct {
	Topology  *topologyFixture
	Keyring   *credentialsFixture
	Server    *fixtureServer
	authority *Authority
}
type fixtureServer struct {
	Bootstrap    *Bootstrap
	Trust        *Trust
	Publications *Publications
	Config       Config
}

func Assemble(cfg Config, c client.Client, reader client.Reader) *Application {
	a := New(cfg, Dependencies{Writer: c, Reader: reader})

	return &Application{
		authority: a,
		Topology:  &topologyFixture{Client: c, APIReader: reader, Config: cfg, Publications: a.publications, Trust: a.trust, authority: a},
		Keyring:   &credentialsFixture{Client: c, APIReader: reader, Config: cfg, Trust: a.trust, authority: a},
		Server:    &fixtureServer{Bootstrap: a.bootstrap, Trust: a.trust, Publications: a.publications, Config: cfg},
	}
}

func (a *Application) Recover(ctx context.Context, writer client.Writer) error {
	return a.authority.Recover(ctx, writer)
}

func (r *topologyFixture) operations() *Authority {
	r.authority.publisher.Writer = r.Client
	r.authority.publisher.APIReader = r.APIReader
	r.authority.publisher.Config = r.Config.effective()

	return r.authority
}

func (r *topologyFixture) CommitVersion(ctx context.Context, p *PreparedPublication) (*CommittedPublication, error) {
	return r.operations().publisher.CommitVersion(ctx, p)
}

func (r *topologyFixture) observeTopology(ctx context.Context) (TopologyObservation, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return TopologyObservation{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return TopologyObservation{}, err
	}

	catalog, err := BuildCatalog(caches.Items)
	if err != nil {
		return TopologyObservation{}, err
	}

	ids, err := members.ReadWorkloadIdentities(ctx, r.APIReader, r.Config.Namespace, r.Config.DaemonSetName)
	if err != nil {
		return TopologyObservation{}, err
	}

	pods := map[string][]corev1.Pod{}

	for _, node := range nodes.Items {
		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(r.Config.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return TopologyObservation{}, err
		}

		pods[node.Name] = list.Items
	}

	return TopologyObservation{Nodes: nodes, Catalog: catalog, Input: members.Input{Nodes: nodes.Items, PodsByNode: pods, Ownership: ids, PeerPort: 8082}}, nil
}

func (r *credentialsFixture) operations() *Authority {
	a := r.authority
	a.credentials.Writer, a.credentials.APIReader, a.credentials.Config, a.credentials.Now = r.Client, r.APIReader, r.Config.effective(), r.Now

	return a
}

const podNodeIndex = "spec.nodeName"

func podNodeKeys(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}

	return []string{pod.Spec.NodeName}
}

func LoadConfig() (Config, error) {
	return Config{Cluster: "22222222-2222-4222-8222-222222222222", Namespace: "racer", DataplaneServiceAccount: "racer-dataplane", ControllerServiceAccount: "racer-controller", DaemonSetName: "racer-dataplane", CredentialsSecretName: "racer-credentials", VersionConfigMapName: "racer-version", InstallationConfigMapName: "racer-installation", Rotation: RotationPolicy{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour}, CertificateLifetime: wire.CertificateLifetime, SnapshotMaxAge: 30 * time.Second, MaxTokenBytes: 16384}, nil
}

const (
	testNodeUID  = "11111111-1111-4111-8111-111111111111"
	testOtherUID = "22222222-2222-4222-8222-222222222222"
)

// Legacy names exist exclusively inside whitebox tests, never the package API.
type (
	Trust                = trustStore
	Publications         = publicationStore
	Issuer               = issuer
	Bootstrap            = bootstrap
	PreparedPublication  = preparedPublication
	CommittedPublication = committedPublication
	RotationState        = rotationState
	VersionRecord        = versionRecord
)

func NewPublications() *publicationStore { return newPublications() }

var AuthenticateCertificate = authenticateCertificate

func (r *credentialsFixture) now() time.Time { return credentialTime(r.Now) }

func catalogCache(name string, uid types.UID) racerv1.ClusterCache {
	return racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
}

const (
	testDaemonSetUID       types.UID = "33333333-3333-4333-8333-333333333333"
	DataplaneDaemonSetName string    = "racer-dataplane"
)

func memberNode() corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: testNodeUID}}
}

func memberPod(uid types.UID, created int64, ip string) corev1.Pod {
	controller := true
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "racer-" + string(uid), UID: uid, Namespace: "racer", CreationTimestamp: metav1.NewTime(time.Unix(created, 0)), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: DataplaneDaemonSetName, UID: testDaemonSetUID, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{PodIP: ip}}
}

func integrationInstallation(t *testing.T, c client.Client, namespace string) *Application {
	t.Helper()
	cfg := testConfig(t)

	cfg.Namespace = namespace
	for _, obj := range []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: cfg.InstallationConfigMapName}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh", markerInitializationProtocol: stagedInitialization}}} {
		if err := c.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}

	return Assemble(cfg, c, c)
}

type servingFixture struct {
	a       *Application
	request wire.BootstrapRequest
	ctx     context.Context
}

func newServingFixture(t *testing.T) *servingFixture {
	t.Helper()

	node := memberNode()
	node.Name = "worker"
	pod := memberPod("pod-uid", 1, "192.0.2.1")
	pod.Spec.NodeName = node.Name
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: DataplaneDaemonSetName, UID: testDaemonSetUID}}
	r := initializedTopology(t, &node, &pod, ds)
	a := Assemble(r.Config, r.Client, r.APIReader)
	runKeys(t, a.Keyring)
	reconcileTopology(t, a.Topology, t.Context())

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}

	return &servingFixture{a: a, ctx: t.Context(), request: wire.BootstrapRequest{SchemaVersion: 1, Cluster: r.Config.Cluster, Enrollment: testOtherUID, CSRDER: csr, Shares: wire.DefaultShares}}
}

func trustReady(trust *Trust) bool {
	_, err := trust.pool()
	return err == nil
}

func rejectWrites(t *testing.T, base client.WithWatch) client.WithWatch {
	t.Helper()

	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("unexpected Create of committed authority")
			return nil
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("unexpected Update of committed authority")
			return nil
		},
	})
}

func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return members.BuildCatalog(caches)
}

func TestEnvtestAuthority(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the real API-server integration suite")
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, racerv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../../api/racer/v1alpha1/crd"}, ErrorIfCRDPathMissing: true}

	rc, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})

	c, err := client.NewWithWatch(rc, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("staged-initialization", func(t *testing.T) { integrationStagedInitialization(t, c) })
	t.Run("catalog-capacity", func(t *testing.T) { integrationCatalogCapacity(t, c) })
}

func TestPublicKeyringRotationPinsWriteAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{Cluster: "11111111-1111-4111-8111-111111111111", Namespace: "racer", DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane", CredentialsSecretName: "racer-credentials", VersionConfigMapName: "racer-version", InstallationConfigMapName: "racer-installation", SnapshotMaxAge: 5 * time.Second, Rotation: RotationPolicy{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour}}

		scheme := runtime.NewScheme()
		require.NoError(t, corev1.AddToScheme(scheme))
		require.NoError(t, racerv1.AddToScheme(scheme))

		marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh", markerInitializationProtocol: stagedInitialization}}
		c := stagedFakeClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(marker).Build())
		now := time.Now()

		a := New(cfg, Dependencies{Reader: c, Writer: c, Now: func() time.Time { return now }})
		require.NoError(t, a.Recover(t.Context(), c))
		_, err := a.ReconcileCredentials(t.Context())
		require.NoError(t, err)

		old, err := a.Keyring()
		require.NoError(t, err)

		admitted, stop, err := a.AdmitTrust(t.Context())
		require.NoError(t, err)

		defer stop()

		deadline, _ := admitted.Context().Deadline()

		var before bytes.Buffer

		_, err = old.Response().WriteTo(t.Context(), admitted, &before)
		require.NoError(t, err)

		time.Sleep(3 * time.Second)

		now = now.Add(cfg.Rotation.Interval - cfg.Rotation.PrepareFor)

		_, err = a.ReconcileCredentials(t.Context())
		require.NoError(t, err)

		current, err := a.Keyring()
		require.NoError(t, err)
		require.Greater(t, current.Generation(), old.Generation(), "ordinary rotation did not advance bundle")

		fresh, stopFresh, err := a.AdmitTrust(t.Context())
		require.NoError(t, err)

		defer stopFresh()

		_, err = old.Response().WriteTo(t.Context(), fresh, io.Discard)
		require.ErrorIs(t, err, wire.Forbidden, "superseded handle borrowed fresh admission")
		_, err = current.Response().WriteTo(t.Context(), fresh, io.Discard)
		require.NoError(t, err)

		var after bytes.Buffer

		_, err = old.Response().WriteTo(t.Context(), admitted, &after)
		require.NoError(t, err, "rotation revoked admitted write")
		require.Equal(t, before.Bytes(), after.Bytes(), "admitted encoding changed")

		got, _ := admitted.Context().Deadline()
		require.Equal(t, deadline, got, "rotation extended admitted deadline")

		time.Sleep(2 * time.Second)

		_, err = old.Response().WriteTo(t.Context(), admitted, io.Discard)
		require.ErrorIs(t, err, context.DeadlineExceeded, "old write outlived pinned freshness")
	})
}

func TestPublicAuthorityScaffoldAndOpaqueValues(t *testing.T) {
	a := New(Config{}, Dependencies{})
	if !errors.Is(a.TrustReady(), wire.Unavailable) || !errors.Is(a.PublicationReady(), wire.Unavailable) {
		t.Fatal("constructor granted authority")
	}

	if !errors.Is(a.Recover(t.Context(), nil), wire.InvalidRequest) {
		t.Fatal("zero configuration reached I/O")
	}

	if _, err := a.Issue(t.Context(), NodeIdentity{}, wire.BootstrapRequest{}); !errors.Is(err, wire.Unauthenticated) {
		t.Fatal("zero identity accepted", err)
	}

	if _, err := a.Wait(t.Context(), NodeIdentity{}, nil); !errors.Is(err, wire.Unauthenticated) {
		t.Fatal("zero poll identity accepted", err)
	}

	var handle PublicationHandle
	if _, _, err := handle.Admit(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatal("zero publication handle accepted", err)
	}

	if _, err := handle.ForBase(0, "").WriteTo(context.Background(), nil, io.Discard); !errors.Is(err, wire.Forbidden) {
		t.Fatal("unguarded response accepted", err)
	}

	for _, value := range []any{NodeIdentity{}, ReplicaIdentity{}, PublicationHandle{}, KeyringHandle{}, Response{}, Admission{}, *a} {
		typeOf := reflect.TypeOf(value)
		for i := range typeOf.NumField() {
			if typeOf.Field(i).IsExported() {
				t.Fatalf("%s exposes mutable field %s", typeOf.Name(), typeOf.Field(i).Name)
			}
		}
	}
}

func TestIdentityAndServingHandleProvenance(t *testing.T) {
	one, two := newServingFixture(t), newServingFixture(t)

	a, b := one.a.authority, two.a.authority
	_, err := a.Issue(t.Context(), NodeIdentity{owner: a, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}, one.request)
	require.ErrorIs(t, err, wire.Unauthenticated, "certificate identity cannot authorize token-only issuance")

	for _, identity := range []NodeIdentity{{}, {owner: b, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}} {
		_, err := a.Issue(t.Context(), identity, one.request)
		require.ErrorIs(t, err, wire.Unauthenticated)
		_, err = a.Wait(t.Context(), identity, nil)
		require.ErrorIs(t, err, wire.Unauthenticated)
	}

	p, err := a.Current()
	require.NoError(t, err)
	other, stopOther, err := b.AdmitTrust(t.Context())
	require.NoError(t, err)

	defer stopOther()

	_, _, err = p.AdmitWithTrust(t.Context(), other)
	require.ErrorIs(t, err, wire.Forbidden)
	_, err = p.ForBase(0, "").WriteTo(t.Context(), other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)

	var zero PublicationHandle

	_, _, err = zero.Admit(t.Context())
	require.ErrorIs(t, err, wire.Unavailable)
	_, err = zero.ForBase(0, "").WriteTo(t.Context(), nil, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	keyring, err := a.Keyring()
	require.NoError(t, err)
	_, err = keyring.Response().WriteTo(t.Context(), other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	_, err = keyring.Response().WriteTo(t.Context(), nil, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)

	var empty KeyringHandle

	_, err = empty.Response().WriteTo(t.Context(), other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
}

func TestKeyringHandleCannotBorrowRecoveredTrust(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	legacy, stopLegacy, err := a.AdmitTrust(t.Context())
	require.NoError(t, err)

	defer stopLegacy()

	old, err := a.Keyring()
	require.NoError(t, err)
	guard, stop, err := a.AdmitTrust(t.Context())
	require.NoError(t, err)

	defer stop()

	a.trust.invalidate()
	require.ErrorIs(t, legacy.Check(t.Context()), context.Canceled)
	require.NoError(t, a.Observe(t.Context()))
	require.ErrorIs(t, guard.Check(t.Context()), context.Canceled)
	fresh, stopFresh, err := a.AdmitTrust(t.Context())
	require.NoError(t, err)

	defer stopFresh()

	_, err = old.Response().WriteTo(t.Context(), fresh, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	current, err := a.Keyring()
	require.NoError(t, err)
	_, err = current.Response().WriteTo(t.Context(), fresh, io.Discard)
	require.NoError(t, err)
}

func TestAnnotationHintsDoNotAliasNestedHistory(t *testing.T) {
	numa := uint32(3)
	history := AcceptedMembers{testNodeUID: {Node: testNodeUID, RDMANICs: []wire.RDMANIC{{Device: "mlx5_0", Port: 1, NUMANode: &numa}}}}
	hints := cloneAccepted(history)
	*hints[testNodeUID].RDMANICs[0].NUMANode = 99
	require.EqualValues(t, 3, *history[testNodeUID].RDMANICs[0].NUMANode)
}

func TestAuthorityConstructionFreezesAuthenticationAndIssuance(t *testing.T) {
	f := newServingFixture(t)
	cfg := f.a.Topology.Config
	cfg.CertificateLifetime = 2 * time.Minute
	a := New(cfg, Dependencies{Reader: f.a.Topology.APIReader, Writer: f.a.Topology.Client})
	cfg.Cluster = ""
	cfg.CertificateLifetime = time.Second

	require.Equal(t, f.a.Topology.Config.Cluster, a.bootstrap.Config.Cluster)
	require.Equal(t, 2*time.Minute, a.bootstrap.Issuer.Config.CertificateLifetime)
	identity := NodeIdentity{owner: a, bearer: true, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}
	encoded, err := a.Issue(t.Context(), identity, f.request)
	require.NoError(t, err)
	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(response.CertificateChain[0])
	require.NoError(t, err)
	require.Equal(t, 3*time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))
}

func TestCatalogGateCanceledAcquisition(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "held"}[held], func(t *testing.T) {
			gate := newCatalogGate()
			if held {
				if err := gate.Acquire(t.Context()); err != nil {
					t.Fatal(err)
				}
			}

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			for range 100 {
				if err := gate.Acquire(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("already canceled acquisition: %v", err)
				}
			}

			if held {
				gate.Release()
			}

			live, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()

			if err := gate.Acquire(live); err != nil {
				t.Fatalf("canceled acquisition consumed the gate: %v", err)
			}

			gate.Release()
		})
	}
}

func TestCatalogGateSerializesCallers(t *testing.T) {
	gate := newCatalogGate()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	// Deliberately non-atomic: the gate must protect each read/modify/write.
	count := 0

	for range 16 {
		wg.Go(func() {
			for range 100 {
				if err := gate.Acquire(ctx); err != nil {
					t.Errorf("acquire: %v", err)
					return
				}

				count++

				gate.Release()
			}
		})
	}

	wg.Wait()

	if count != 1600 {
		t.Fatalf("lost serialized updates: %d", count)
	}
}

func capacityCaches(count int) []racerv1.ClusterCache {
	caches := make([]racerv1.ClusterCache, count)
	for i := range caches {
		caches[i] = catalogCache(fmt.Sprintf("capacity-%d", i), types.UID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)))
	}

	return caches
}

func TestCatalogCapacityBoundary(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	_, b, _, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, b)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("default admitted maximum: %d", capacity)
	// Check the conservative byte envelope independently: max generation, four
	// roots, two generations of both purposes, and the longest state.
	for _, count := range []int{capacity, capacity + 1} {
		keys := []map[string]any{}

		for _, cache := range capacityCaches(count) {
			for range 2 {
				for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
					keys = append(keys, map[string]any{"cache": cache.UID, "id": make([]byte, 16), "purpose": purpose, "state": wire.PreparedKey, "material": make([]byte, 32)})
				}
			}
		}

		var out bytes.Buffer

		err := json.NewEncoder(&out).Encode(map[string]any{"schema_version": wire.SchemaVersion, "cluster": r.Config.Cluster, "generation": fmt.Sprint(uint64(math.MaxUint64)), "peer_trust_roots": [][]byte{make([]byte, reservedRootBytes), make([]byte, reservedRootBytes), make([]byte, reservedRootBytes), make([]byte, reservedRootBytes)}, "cache_keys": keys})
		if err != nil || (out.Len() <= wire.MaxBundleBytes) != (count == capacity) {
			t.Fatalf("capacity=%d count=%d bytes=%d: %v", capacity, count, out.Len(), err)
		}
	}
	// Even an empty catalog cannot make an unbounded number of roots fit. Reject
	// pathological policy before consuming the one-way initialization claim.
	r.Config.Rotation.Interval, r.Config.Rotation.PrepareFor = time.Nanosecond, time.Nanosecond
	if _, err := catalogCapacity(r.Config, b); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("unbounded trust reserve: %v", err)
	}
}

func TestCatalogAdmissionRotationCycles(t *testing.T) {
	for _, policy := range []RotationPolicy{
		{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour},
		{Interval: 6 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour},
		{Interval: 12 * time.Hour, PrepareFor: 12 * time.Hour, RetainFor: 48 * time.Hour},
		{Interval: 7 * 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 24 * time.Hour},
	} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			r, now := testKeyring(t)
			r.Config.Rotation = policy
			runKeys(t, r)
			shared, b, _, _ := keyState(t, r)

			capacity, err := catalogCapacity(r.Config, b)
			if err != nil {
				t.Fatal(err)
			}

			for _, cache := range capacityCaches(capacity - 1) {
				if err := r.Create(t.Context(), &cache); err != nil {
					t.Fatal(err)
				}
			}
			// Cross a decimal-width boundary immediately and continue through
			// enough cycles to reach the steady-state retirement high watermark.
			b.Generation = 99

			shared.Data["bundle.json"], _ = wire.EncodeBundle(b)
			if err := r.Update(t.Context(), shared); err != nil {
				t.Fatal(err)
			}

			runKeys(t, r)

			maxRoots := exerciseCapacityRotations(t, r, now, capacity)

			if policy.Interval == 6*time.Hour && maxRoots < 8 {
				t.Fatalf("did not exercise multiple retiring generations: %d", maxRoots)
			}
		})
	}
}

func exerciseCapacityRotations(t *testing.T, r *credentialsFixture, now *time.Time, capacity int) int {
	t.Helper()

	maxRoots := 0

	for range 30 {
		_, before, previous, _ := keyState(t, r)
		*now = previous.nextTransition()

		runKeys(t, r)
		_, after, state, _ := keyState(t, r)

		maxRoots = max(maxRoots, len(after.PeerTrustRoots))
		if len(keyedCaches(after)) != capacity || !trustReady(r.Trust) || after.Generation <= before.Generation {
			t.Fatal("rotation at capacity lost admission, readiness, or progress")
		}

		for id, deadline := range previous.Retiring {
			if now.Before(deadline) && !state.Retiring[id].Equal(deadline) {
				t.Fatal("capacity shortened retirement")
			}
		}
	}

	return maxRoots
}

func TestCatalogAdmissionGrowthRemovalAndRestart(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	a := Assemble(r.Config, r.Client, r.APIReader)
	first := reconcileTopology(t, a.Topology, t.Context())
	_, initial, _, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, initial)
	if err != nil {
		t.Fatal(err)
	}

	caches := capacityCaches(capacity + 1)
	for _, cache := range caches {
		require.NoError(t, r.Create(t.Context(), &cache))
	}

	if got := reconcileTopology(t, a.Topology, t.Context()); got != first {
		t.Fatal("published growth before its keys were committed")
	}

	var logs strings.Builder

	logger := funcr.New(func(_, msg string) { logs.WriteString(msg) }, funcr.Options{})

	ctx := ctrl.LoggerInto(t.Context(), logger)
	if _, err := r.operations().ReconcileCredentials(ctx); err != nil || !trustReady(r.Trust) {
		t.Fatalf("growth disabled healthy service: %v", err)
	}

	if !strings.Contains(logs.String(), "rotation_capacity") || !strings.Contains(logs.String(), caches[capacity].Name) {
		t.Fatal("capacity rejection was not observable")
	}

	_, admitted, _, _ := keyState(t, r)

	ids := keyedCaches(admitted)
	if !ids[wire.CacheID(testNodeUID)] || len(ids) != capacity || ids[wire.CacheID(caches[capacity-1].UID)] {
		t.Fatal("growth displaced established UID or ignored sorted free-slot order")
	}

	assertPublishedKeys(t, a.Topology, capacity)
	// Removing a rejected candidate must neither change keys nor consume a
	// publication sequence. A restart retains the admitted set from the Secret.
	before := reconcileTopology(t, a.Topology, t.Context())
	require.NoError(t, r.Delete(t.Context(), &caches[capacity]))

	r = Assemble(r.Config, r.Client, r.APIReader).Keyring
	runKeys(t, r)

	if after := reconcileTopology(t, a.Topology, t.Context()); after != before {
		t.Fatal("rejected deletion changed publication")
	}

	if err := r.Delete(t.Context(), &caches[0]); err != nil {
		t.Fatal(err)
	}

	assertPublishedKeys(t, a.Topology, capacity-1)
	runKeys(t, r)
	assertPublishedKeys(t, a.Topology, capacity)

	_, replaced, _, _ := keyState(t, r)
	if keyedCaches(replaced)[wire.CacheID(caches[0].UID)] || !keyedCaches(replaced)[wire.CacheID(caches[capacity-1].UID)] {
		t.Fatal("deletion did not admit next waiting UID")
	}

	assertMissingCredentialsSuspendTopology(t, r, a.Topology)
}

func assertMissingCredentialsSuspendTopology(t *testing.T, r *credentialsFixture, topology *topologyFixture) {
	t.Helper()
	// Missing/corrupt durable credentials remain fail-closed in both controllers.
	shared := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}}
	if err := r.Delete(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	if _, err := topology.operations().PublishTopology(t.Context(), topology.observeTopology); err == nil {
		t.Fatal("missing credentials accepted by topology")
	}

	if _, err := topology.Publications.Current(); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("missing credentials did not suspend publication: %v", err)
	}
}

func assertPublishedKeys(t *testing.T, r *topologyFixture, count int) {
	t.Helper()
	p := reconcileTopology(t, r, t.Context())

	v, err := wire.DecodePublication(strings.NewReader(p.encoded))
	if err != nil || len(v.Caches) != count {
		t.Fatalf("published caches=%d, want %d: %v", len(v.Caches), count, err)
	}

	_, b, _, _ := keyState(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)
	for _, cache := range v.Caches {
		if !keyedCaches(b)[cache.ID] {
			t.Fatal("published cache without both active keys")
		}
	}
}

func TestCatalogAdmissionDeterministicColdStart(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	_, b, _, _ := keyState(t, r)
	b.CacheKeys = nil

	capacity, err := catalogCapacity(r.Config, b)
	if err != nil {
		t.Fatal(err)
	}

	input := capacityCaches(capacity + 2)
	slices.Reverse(input)

	catalog, err := BuildCatalog(input)
	if err != nil {
		t.Fatal(err)
	}

	got, err := admitCatalog(context.Background(), r.Config, catalog, b)
	if err != nil || !slices.Equal(got, catalog[:capacity]) {
		t.Fatalf("cold admission is not a sorted UID prefix: %v", err)
	}
}

func TestCatalogCapacityRejectsBeforeInitializationClaim(t *testing.T) {
	r, _ := testKeyring(t)

	r.Config.Rotation.Interval, r.Config.Rotation.PrepareFor = time.Nanosecond, time.Nanosecond
	if _, err := r.operations().ReconcileCredentials(t.Context()); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("impossible root reserve: %v", err)
	}

	cm, _, err := readVersion(t.Context(), r.APIReader, r.Config)
	if err != nil || cm.Annotations[credentialClaim] != "" {
		t.Fatalf("impossible policy consumed credential claim: %v", err)
	}
}

func TestCatalogAdmissionLegacyOvercommitDoesNotEvict(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	shared, b, state, _ := keyState(t, r)

	capacity, err := catalogCapacity(r.Config, b)
	if err != nil {
		t.Fatal(err)
	}

	caches := capacityCaches(capacity)
	for _, cache := range caches {
		if err := r.Create(t.Context(), &cache); err != nil {
			t.Fatal(err)
		}
	}

	caches = append(caches, catalogCache("cache", testNodeUID))

	catalog, err := BuildCatalog(caches)
	if err != nil {
		t.Fatal(err)
	}
	// Model the older controller's active-only admission without removing the
	// planner's independent final wire-size check.
	b, state, _, err = planRotation(r.Config.Rotation, b, state, catalog, *now, nextGeneration(b.Generation))
	if err != nil {
		t.Fatal(err)
	}

	b.Generation++

	shared.Data["bundle.json"], err = wire.EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}

	shared.Data["rotation.json"], _ = json.Marshal(state)
	if err := r.Update(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	if _, err := r.operations().ReconcileCredentials(t.Context()); !errors.Is(err, wire.TooLarge) || trustReady(r.Trust) {
		t.Fatalf("legacy overcommit silently accepted: %v", err)
	}

	after, preserved, _, _ := keyState(t, r)
	if after.ResourceVersion != shared.ResourceVersion || len(keyedCaches(preserved)) != capacity+1 {
		t.Fatal("legacy overcommit evicted durable credentials")
	}
}

func TestCatalogAdmissionSerializesPublicationAndPruning(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	a := Assemble(r.Config, r.Client, r.APIReader)
	read := make(chan struct{})
	proceed := make(chan struct{})
	a.Topology.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if key.Name == r.Config.CredentialsSecretName {
				close(read)
				<-proceed
			}

			return err
		},
	})
	done := make(chan error, 1)

	go func() {
		_, err := a.Topology.operations().PublishTopology(t.Context(), a.Topology.observeTopology)
		done <- err
	}()

	<-read
	// The keyring must be excluded for the entire read/commit/install window.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := a.authority.gate.Acquire(ctx)
	if err == nil {
		a.authority.gate.Release()
		close(proceed)
		<-done
		t.Fatal("keyring can prune an in-progress topology candidate")
	}

	close(proceed)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected gate wait error: %v", err)
	}

	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	if err := a.authority.gate.Acquire(ctx); err != nil {
		t.Fatalf("publication did not release admission gate: %v", err)
	}

	a.authority.gate.Release()
}

func integrationCatalogCapacity(t *testing.T, c client.Client) {
	a := integrationInstallation(t, c, "catalog-capacity")
	// Use a root-heavy valid policy to exercise real admission with a small
	// catalog. Unit tests above run the default maximum through repeated cycles.
	// Reserve generations by activation interval, not interval plus preparation.
	// 381 roots leave room for a small catalog with two symmetric generations;
	// 382 roots would leave no capacity for a complete active/prepared key pair.
	a.Keyring.Config.Rotation = RotationPolicy{Interval: time.Hour, PrepareFor: time.Hour, RetainFor: 379 * time.Hour}

	a.Topology.Config = a.Keyring.Config
	if err := a.Recover(t.Context(), a.Topology.Client); err != nil {
		t.Fatal(err)
	}

	runKeys(t, a.Keyring)
	_, b, _, _ := keyState(t, a.Keyring)

	capacity, err := catalogCapacity(a.Keyring.Config, b)
	if err != nil || capacity < 1 || capacity > 5 {
		t.Fatalf("integration capacity: %d %v", capacity, err)
	}

	for i := range capacity + 1 {
		cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("capacity-real-%d", i)}}
		if err := c.Create(t.Context(), cache); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := c.Delete(context.Background(), cache); err != nil {
				t.Error(err)
			}
		})
	}

	assertPublishedKeys(t, a.Topology, 0)
	runKeys(t, a.Keyring)
	assertPublishedKeys(t, a.Topology, capacity)

	if !trustReady(a.Server.Trust) {
		t.Fatal("API-backed capacity rejection withdrew readiness")
	}
}

func testIssuer(r *credentialsFixture) *Issuer {
	return &Issuer{APIReader: r.APIReader, Config: r.Config.effective(), Trust: r.Trust, CatalogGate: r.authority.gate, Now: r.Now}
}

// TrustRoots is a test adapter for authoritative signing observations. Production
// serving uses local Trust; only issuance and reconciliation read durable roots.
func (i *Issuer) TrustRoots(ctx context.Context) (*x509.CertPool, error) {
	state, err := i.loadSigning(ctx, credentialTime(i.Now))
	if err != nil {
		return nil, err
	}

	return state.roots, nil
}

func issuanceRequest(t *testing.T, r *credentialsFixture) (NodeIdentity, wire.BootstrapRequest, ed25519.PublicKey) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "untrusted"}, DNSNames: []string{"attacker"}, URIs: []*url.URL{{Scheme: "spiffe", Host: "attacker", Path: "/node/attacker"}}}, key)
	require.NoError(t, err)

	return NodeIdentity{cluster: r.Config.Cluster, node: wire.NodeID(testNodeUID), expires: r.now().Add(time.Hour)}, wire.BootstrapRequest{SchemaVersion: wire.SchemaVersion, Cluster: r.Config.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr, Shares: wire.DefaultShares}, pub
}

func decodeIssuedResponse(t *testing.T, encoded []byte) wire.BootstrapResponse {
	t.Helper()
	require.NotEmpty(t, encoded)
	require.LessOrEqual(t, len(encoded), wire.MaxBootstrapBytes)
	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	require.NoError(t, err)

	return response
}

func TestIssuerCertificateContractAndTrustRotation(t *testing.T) {
	r, now := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, pub := issuanceRequest(t, r)
	encoded, err := issuer.Issue(context.Background(), identity, request)
	require.NoError(t, err)
	response := decodeIssuedResponse(t, encoded)
	cert, err := x509.ParseCertificate(response.CertificateChain[0])
	require.NoError(t, err)
	require.Equal(t, identity.Node(), response.Node)
	require.Equal(t, request.Cluster, response.Cluster)
	require.Equal(t, request.Enrollment, response.Enrollment)
	require.Len(t, response.CertificateChain, 2)
	require.False(t, cert.IsCA)
	require.Empty(t, cert.Subject.CommonName)
	require.Empty(t, cert.DNSNames)
	require.Len(t, cert.URIs, 1)
	require.Equal(t, "spiffe://"+string(identity.Cluster())+"/node/"+string(identity.Node()), cert.URIs[0].String())
	require.Equal(t, x509.KeyUsageDigitalSignature, cert.KeyUsage)
	require.True(t, cert.NotAfter.Equal(now.Add(wire.CertificateLifetime)))
	require.True(t, cert.NotBefore.Equal(now.Add(-certificateClockSkew)))
	require.Equal(t, pub, cert.PublicKey.(ed25519.PublicKey))

	roots, err := issuer.TrustRoots(context.Background())
	require.NoError(t, err)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: *now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: *now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	require.Error(t, err, "node can act as HTTPS server")
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	runKeys(t, r)

	identity.expires = now.Add(time.Hour)
	encoded, err = issuer.Issue(context.Background(), identity, request)
	require.NoError(t, err)
	staged := decodeIssuedResponse(t, encoded)
	require.Equal(t, response.CertificateChain[1], staged.CertificateChain[1], "prepared issuer signed early")
	_, _, preparation, _ := keyState(t, r)
	*now = preparation.ActivateAt

	runKeys(t, r)

	identity.expires = now.Add(time.Hour)
	encoded, err = issuer.Issue(context.Background(), identity, request)
	require.NoError(t, err)
	active := decodeIssuedResponse(t, encoded)
	require.NotEqual(t, response.CertificateChain[1], active.CertificateChain[1], "new issuer not activated")

	roots, err = issuer.TrustRoots(context.Background())
	require.NoError(t, err)
	oldLeaf, err := x509.ParseCertificate(staged.CertificateChain[0])
	require.NoError(t, err)
	_, err = oldLeaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: *now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err, "old leaf lost overlap")

	*now = now.Add(r.Config.Rotation.RetainFor)
	runKeys(t, r)

	roots, err = issuer.TrustRoots(context.Background())
	require.NoError(t, err)
	// Use a time at which the old leaf was valid to isolate root removal.
	_, err = oldLeaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: oldLeaf.NotBefore.Add(time.Minute), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	require.Error(t, err, "retired root remains trusted")
}

func TestIssuerRejectsUntrustedRequests(t *testing.T) {
	r, _ := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)

	identity, request, _ := issuanceRequest(t, r)
	for _, scenario := range []string{"zero identity", "expired identity", "wrong cluster", "unsupported version", "bad enrollment", "malformed csr", "bad proof", "wrong algorithm", "oversized", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			id, req := identity, request

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			switch scenario {
			case "zero identity":
				id = NodeIdentity{}
			case "expired identity":
				id.expires = r.now()
			case "wrong cluster":
				req.Cluster = wire.ClusterID(testNodeUID)
			case "unsupported version":
				req.SchemaVersion++
			case "bad enrollment":
				req.Enrollment = "bad"
			case "malformed csr":
				req.CSRDER = []byte("invalid DER")
			case "bad proof":
				req.CSRDER = bytes.Clone(req.CSRDER)
				req.CSRDER[len(req.CSRDER)-1] ^= 1
			case "wrong algorithm":
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				require.NoError(t, err)
				req.CSRDER, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
				require.NoError(t, err)
			case "oversized":
				req.CSRDER = make([]byte, wire.MaxBootstrapBytes+1)
			case "canceled":
				cancel()
			}

			response, err := issuer.Issue(ctx, id, req)
			require.Error(t, err, "untrusted issuance accepted")
			require.Nil(t, response)
		})
	}
}

func TestIssuerShortLifetimeAndRetirement(t *testing.T) {
	r, now := testKeyring(t)
	r.Config.CertificateLifetime = 2 * time.Minute
	r.Config.Rotation = RotationPolicy{5 * time.Minute, 20 * time.Second, 2 * time.Minute}
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)
	encoded, err := issuer.Issue(context.Background(), identity, request)
	require.NoError(t, err)
	response := decodeIssuedResponse(t, encoded)
	leaf, err := x509.ParseCertificate(response.CertificateChain[0])
	require.NoError(t, err)
	require.True(t, leaf.NotAfter.Equal(now.Add(2*time.Minute)))
	require.True(t, leaf.NotBefore.Equal(now.Add(-certificateClockSkew)))
	_, _, initial, _ := keyState(t, r)
	*now = initial.NextRotation

	runKeys(t, r)
	_, _, prepared, _ := keyState(t, r)
	*now = prepared.ActivateAt

	runKeys(t, r)

	encoded, err = issuer.Issue(context.Background(), identity, request)
	require.NoError(t, err)
	renewed := decodeIssuedResponse(t, encoded)
	require.NotEqual(t, response.CertificateChain[1], renewed.CertificateChain[1], "short rotation issuer activation")

	*now = now.Add(2 * time.Minute)

	runKeys(t, r)
	_, bundle, _, material := keyState(t, r)
	require.False(t, containsRoot(bundle, initial.ActiveIssuer))
	require.Len(t, material.Keys, 1, "short rotation did not retire old public/private issuer")
}

func TestIssuerFullEncodedRequestBound(t *testing.T) {
	r, _ := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	for _, size := range []int{47 * 1024, 49 * 1024} {
		request.CSRDER, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: strings.Repeat("x", size)}}, key)
		require.NoError(t, err)
		require.Less(t, len(request.CSRDER), wire.MaxBootstrapBytes, "fixture must fit the raw DER bound")

		encoded, err := issuer.Issue(context.Background(), identity, request)
		if size == 49*1024 {
			require.ErrorIs(t, err, wire.TooLarge)
			require.Nil(t, encoded)

			continue
		}

		require.NoError(t, err)
		response := decodeIssuedResponse(t, encoded)
		require.Equal(t, request.Enrollment, response.Enrollment)
		require.Equal(t, identity.Node(), response.Node, "large valid request lost correlation")
	}
}

func TestIssuerConcurrentIssuanceAndReconciliation(t *testing.T) {
	r, _ := testKeyring(t)
	issuer := testIssuer(r)
	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 4 {
				if _, err := issuer.Issue(context.Background(), identity, request); err != nil {
					t.Error(err)
				}

				if _, err := issuer.TrustRoots(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}

	for range 4 {
		runKeys(t, r)
	}

	wg.Wait()
}

func writeSigningCredentials(t *testing.T, r *credentialsFixture, b wire.KeyringBundle, s RotationState, m issuerMaterial) {
	t.Helper()

	bundle, err := wire.EncodeBundle(b)
	require.NoError(t, err)
	rotation, err := json.Marshal(s)
	require.NoError(t, err)
	material, err := json.Marshal(m)
	require.NoError(t, err)

	secret := &corev1.Secret{}
	require.NoError(t, r.APIReader.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}, secret))
	secret.Data = map[string][]byte{"issuer.json": material, "bundle.json": bundle, "rotation.json": rotation}
	require.NoError(t, r.Update(t.Context(), secret))
}

func editSigningCertificate(t *testing.T, m signingMaterial, edit func(*x509.Certificate)) signingMaterial {
	t.Helper()

	cert, key, err := parseSigning(m)
	require.NoError(t, err)
	edit(cert)
	m.Certificate, err = x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	require.NoError(t, err)

	return m
}

func TestSigningRejectsCorruptPrivateEntries(t *testing.T) {
	for _, role := range []string{"active", "prepared", "retiring", "extra", "pending"} {
		for _, corruption := range []string{"missing", "root binding", "certificate", "trailing certificate bytes", "private key", "key mismatch", "not CA", "constraints", "key usage", "self signature"} {
			t.Run(role+"/"+corruption, func(t *testing.T) {
				r, now := testKeyring(t)
				runKeys(t, r)
				_, b, s, m := keyState(t, r)
				id := signingRole(t, r, role, &b, &s, m)
				writeSigningCredentials(t, r, b, s, m)
				_, err := loadSigning(t.Context(), r.APIReader, r.Config, *now)
				require.NoError(t, err, "valid %s rejected", role)
				bad := corruptSigning(t, r, corruption, m.Keys[id])
				delete(m.Keys, id)

				if corruption == "root binding" {
					m.Keys["wrong fingerprint"] = bad
				} else if corruption != "missing" {
					rebindSigning(&b, &s, m, id, bad, corruption != "certificate" && corruption != "trailing certificate bytes")
				}

				writeSigningCredentials(t, r, b, s, m)
				state, err := loadSigning(t.Context(), r.APIReader, r.Config, *now)
				require.ErrorIs(t, err, wire.Unavailable)
				require.Nil(t, state.certificate)
				require.Nil(t, state.key)
				require.Nil(t, state.roots)
				_, err = testIssuer(r).TrustRoots(t.Context())
				require.ErrorIs(t, err, wire.Unavailable)
				_, err = r.Trust.pool()
				require.Error(t, err, "observed corruption retained trust")
				_, err = r.operations().ReconcileCredentials(t.Context())
				require.ErrorIs(t, err, wire.Unavailable)
			})
		}
	}
}

func signingRole(t *testing.T, r *credentialsFixture, role string, b *wire.KeyringBundle, s *RotationState, m issuerMaterial) string {
	t.Helper()

	if role == "active" {
		return s.ActiveIssuer
	}

	cert, key, err := generateIssuer(r.now(), r.Config)
	require.NoError(t, err)

	id := rootID(cert)

	m.Keys[id] = signingMaterial{Certificate: cert, PrivateKey: key}
	if role == "extra" || role == "pending" {
		writeSigningCredentials(t, r, *b, *s, m)
		_, err := loadSigning(t.Context(), r.APIReader, r.Config, r.now())
		require.ErrorIs(t, err, wire.Unavailable, "unpublished private material accepted")
	}

	b.PeerTrustRoots = append(b.PeerTrustRoots, cert)

	if role == "prepared" {
		s.PreparedIssuer = id
		s.ActivateAt = s.NextRotation.Add(r.Config.Rotation.PrepareFor)
	} else {
		s.Retiring[id] = s.NextRotation.Add(time.Hour)
	}

	return id
}

func corruptSigning(t *testing.T, r *credentialsFixture, corruption string, bad signingMaterial) signingMaterial {
	t.Helper()

	switch corruption {
	case "certificate":
		bad.Certificate = []byte("invalid DER")
	case "trailing certificate bytes":
		bad.Certificate = append(bytes.Clone(bad.Certificate), 0)
	case "private key":
		bad.PrivateKey = []byte("invalid PKCS8")
	case "key mismatch":
		_, key, err := generateIssuer(r.now(), r.Config)
		require.NoError(t, err)

		bad.PrivateKey = key
	case "not CA":
		bad = editSigningCertificate(t, bad, func(c *x509.Certificate) {
			c.IsCA, c.MaxPathLenZero, c.MaxPathLen = false, false, -1
		})
	case "constraints":
		bad = editSigningCertificate(t, bad, func(c *x509.Certificate) { c.BasicConstraintsValid = false })
	case "key usage":
		bad = editSigningCertificate(t, bad, func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature })
	case "self signature":
		bad.Certificate = bytes.Clone(bad.Certificate)
		bad.Certificate[len(bad.Certificate)-1] ^= 1
	}

	return bad
}

func rebindSigning(b *wire.KeyringBundle, s *RotationState, m issuerMaterial, oldID string, material signingMaterial, replaceRoot bool) {
	id := rootID(material.Certificate)

	delete(m.Keys, oldID)
	m.Keys[id] = material

	for i, root := range b.PeerTrustRoots {
		if replaceRoot && rootID(root) == oldID {
			b.PeerTrustRoots[i] = material.Certificate
		}
	}

	if s.ActiveIssuer == oldID {
		s.ActiveIssuer = id
	}

	if s.PreparedIssuer == oldID {
		s.PreparedIssuer = id
	}

	if at, ok := s.Retiring[oldID]; ok {
		delete(s.Retiring, oldID)
		s.Retiring[id] = at
	}
}

func TestSigningActiveLifetimeBoundary(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	_, _, s, m := keyState(t, r)
	cert, _, err := parseSigning(m.Keys[s.ActiveIssuer])
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		at    time.Time
		valid bool
	}{
		{"not yet valid", cert.NotBefore.Add(-time.Second), false},
		{"starts now", cert.NotBefore, true},
		{"full leaf lifetime", cert.NotAfter.Add(-r.Config.CertificateLifetime), true},
		{"short by one second", cert.NotAfter.Add(-r.Config.CertificateLifetime).Add(time.Second), false},
		{"expired", cert.NotAfter, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := loadSigning(t.Context(), r.APIReader, r.Config, tc.at)
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, cert.Raw, state.certificate.Raw)
				require.True(t, state.certificate.PublicKey.(ed25519.PublicKey).Equal(state.key.Public()))
			} else {
				require.ErrorIs(t, err, wire.Unavailable, "invalid active lifetime accepted")
			}
		})
	}
}

func TestSigningPoolOnlyIncludesTimeValidPublishedRoots(t *testing.T) {
	r, now := testKeyring(t)
	runKeys(t, r)
	_, b, s, m := keyState(t, r)
	active, _, err := parseSigning(m.Keys[s.ActiveIssuer])
	require.NoError(t, err)

	want := x509.NewCertPool()
	want.AddCert(active)

	for _, role := range []string{"starts now", "expires now", "future", "extra", "pending"} {
		cert, key, err := generateIssuer(*now, r.Config)
		require.NoError(t, err)
		material := editSigningCertificate(t, signingMaterial{Certificate: cert, PrivateKey: key}, func(c *x509.Certificate) {
			switch role {
			case "starts now":
				c.NotBefore = *now
			case "expires now":
				c.NotAfter = *now
			case "future":
				c.NotBefore = now.Add(time.Second)
			}
		})
		id := rootID(material.Certificate)

		m.Keys[id] = material
		if role == "extra" || role == "pending" {
			writeSigningCredentials(t, r, b, s, m)
			_, err := loadSigning(t.Context(), r.APIReader, r.Config, *now)
			require.ErrorIs(t, err, wire.Unavailable, "unpublished private material accepted")
			delete(m.Keys, id)

			continue
		}

		b.PeerTrustRoots = append(b.PeerTrustRoots, material.Certificate)
		s.Retiring[id] = s.NextRotation.Add(time.Hour)

		if role == "starts now" {
			root, _, err := parseSigning(material)
			require.NoError(t, err)
			want.AddCert(root)
		}
	}

	writeSigningCredentials(t, r, b, s, m)
	state, err := loadSigning(t.Context(), r.APIReader, r.Config, *now)
	require.NoError(t, err)
	require.True(t, state.roots.Equal(want))
	require.Equal(t, active.Raw, state.certificate.Raw)

	for id := range s.Retiring {
		s.Retiring[id] = time.Time{}
		break
	}

	writeSigningCredentials(t, r, b, s, m)
	_, err = loadSigning(t.Context(), r.APIReader, r.Config, *now)
	require.ErrorIs(t, err, wire.Unavailable, "zero retirement deadline accepted")
}

func TestLeafClockSkewPreservesExpirationAndUsage(t *testing.T) {
	r, now := testKeyring(t)
	// Simulate an issuing leader ahead of a follower's clock.
	*now = now.Add(30 * time.Second)

	runKeys(t, r)
	identity, request, _ := issuanceRequest(t, r)
	encoded, err := testIssuer(r).Issue(t.Context(), identity, request)
	require.NoError(t, err)
	response := decodeIssuedResponse(t, encoded)
	leaf, err := x509.ParseCertificate(response.CertificateChain[0])
	require.NoError(t, err)
	root, err := x509.ParseCertificate(response.CertificateChain[1])
	require.NoError(t, err)
	require.True(t, leaf.NotBefore.Equal(now.Add(-time.Minute)))
	require.True(t, leaf.NotAfter.Equal(now.Add(r.Config.CertificateLifetime)))

	roots := x509.NewCertPool()
	roots.AddCert(root)

	for _, tc := range []struct {
		name  string
		at    time.Time
		usage x509.ExtKeyUsage
		valid bool
	}{
		{"follower behind", now.Add(-30 * time.Second), x509.ExtKeyUsageClientAuth, true},
		{"skew boundary", now.Add(-time.Minute), x509.ExtKeyUsageClientAuth, true},
		{"excess skew", now.Add(-time.Minute - time.Second), x509.ExtKeyUsageClientAuth, false},
		{"before expiry", leaf.NotAfter.Add(-time.Second), x509.ExtKeyUsageClientAuth, true},
		{"expired", leaf.NotAfter.Add(time.Second), x509.ExtKeyUsageClientAuth, false},
		{"server usage", *now, x509.ExtKeyUsageServerAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: tc.at, KeyUsages: []x509.ExtKeyUsage{tc.usage}})
			require.Equal(t, tc.valid, err == nil, "verification: %v", err)
		})
	}

	state := &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf, root}, VerifiedChains: [][]*x509.Certificate{{leaf, root}}}
	_, err = AuthenticateCertificate(t.Context(), r.Trust, r.Config, state)
	require.NoError(t, err, "follower rejected skewed leader's certificate")
	// Expired leaves still fail each authorization, even on a verified connection.
	leaf.NotAfter = time.Now().Add(-time.Second)
	_, err = AuthenticateCertificate(t.Context(), r.Trust, r.Config, state)
	require.ErrorIs(t, err, wire.Unauthenticated, "expired certificate accepted")
	// Advancing past signing capacity still fails closed, rather than clipping expiry.
	*now = root.NotAfter.Add(-r.Config.CertificateLifetime + time.Second)
	identity.expires = now.Add(time.Hour)
	_, err = testIssuer(r).Issue(t.Context(), identity, request)
	require.ErrorIs(t, err, wire.Unavailable, "insufficient issuer lifetime accepted")
}

func authenticatedBootstrapFixture(t *testing.T) (*servingFixture, authv1.TokenReviewStatus, string) {
	t.Helper()
	f := newServingFixture(t)
	cfg, c := f.a.authority.config, f.a.Topology.Client

	var pods corev1.PodList
	require.NoError(t, c.List(t.Context(), &pods))
	require.Len(t, pods.Items, 1)
	pod := &pods.Items[0]
	pod.Spec.ServiceAccountName = cfg.DataplaneServiceAccount
	require.NoError(t, c.Update(t.Context(), pod))

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.DataplaneServiceAccount, UID: "service-account"}}
	require.NoError(t, c.Create(t.Context(), sa))
	status := authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{wire.TokenAudience}, User: authv1.UserInfo{
		Username: "system:serviceaccount:" + cfg.Namespace + ":" + cfg.DataplaneServiceAccount, UID: string(sa.UID),
		Extra: map[string]authv1.ExtraValue{
			"authentication.kubernetes.io/pod-name":  {pod.Name},
			"authentication.kubernetes.io/pod-uid":   {string(pod.UID)},
			"authentication.kubernetes.io/node-name": {pod.Spec.NodeName},
			"authentication.kubernetes.io/node-uid":  {testNodeUID},
		},
	}}
	payload := fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())

	return f, status, "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestBootstrapTokenBindingAndEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*authv1.TokenReviewStatus, *string)
		want error
	}{
		{"valid", func(*authv1.TokenReviewStatus, *string) {}, nil},
		{"unauthenticated", func(s *authv1.TokenReviewStatus, _ *string) { s.Authenticated = false }, wire.Unauthenticated},
		{"review error", func(s *authv1.TokenReviewStatus, _ *string) { s.Error = "denied" }, wire.Unauthenticated},
		{"audience", func(s *authv1.TokenReviewStatus, _ *string) { s.Audiences = nil }, wire.Unauthenticated},
		{"account", func(s *authv1.TokenReviewStatus, _ *string) { s.User.Username = "other" }, wire.Forbidden},
		{"account uid", func(s *authv1.TokenReviewStatus, _ *string) { s.User.UID = "other" }, wire.Forbidden},
		{"missing uid", func(s *authv1.TokenReviewStatus, _ *string) { s.User.UID = "" }, wire.Unauthenticated},
		{"pod uid", func(s *authv1.TokenReviewStatus, _ *string) {
			s.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"other"}
		}, wire.Forbidden},
		{"ambiguous pod", func(s *authv1.TokenReviewStatus, _ *string) {
			s.User.Extra["authentication.kubernetes.io/pod-name"] = authv1.ExtraValue{"one", "two"}
		}, wire.Unauthenticated},
		{"missing pod", func(s *authv1.TokenReviewStatus, _ *string) {
			s.User.Extra["authentication.kubernetes.io/pod-name"] = authv1.ExtraValue{"missing"}
		}, wire.Forbidden},
		{"node uid", func(s *authv1.TokenReviewStatus, _ *string) {
			s.User.Extra["authentication.kubernetes.io/node-uid"] = authv1.ExtraValue{"other"}
		}, wire.Forbidden},
		{"malformed token", func(_ *authv1.TokenReviewStatus, token *string) { *token = "invalid" }, wire.Unauthenticated},
		{"invalid base64", func(_ *authv1.TokenReviewStatus, token *string) { *token = "header.!.signature" }, wire.Unauthenticated},
		{"missing expiration", func(_ *authv1.TokenReviewStatus, token *string) { *token = "header.e30.signature" }, wire.Unauthenticated},
		{"expired", func(_ *authv1.TokenReviewStatus, token *string) {
			*token = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
		}, wire.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, status, token := authenticatedBootstrapFixture(t)
			tc.edit(&status, &token)

			a := f.a.authority
			a.bootstrap.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				review := obj.(*authv1.TokenReview)
				require.Equal(t, token, review.Spec.Token)
				require.Equal(t, []string{wire.TokenAudience}, review.Spec.Audiences)
				review.Status = status

				return nil
			}})
			request := httptest.NewRequest("POST", "/bootstrap", nil)
			request.Header.Set("Authorization", "Bearer "+token)

			identity, err := a.Authenticate(t.Context(), request)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				require.Equal(t, NodeIdentity{}, identity)

				return
			}

			require.NoError(t, err)
			require.Equal(t, wire.NodeID(testNodeUID), identity.Node())
			require.Equal(t, a.config.Cluster, identity.Cluster())
			require.True(t, identity.Expires().After(time.Now()))
			encoded, hint, err := a.EnrollWithHint(t.Context(), request, f.request)
			require.NoError(t, err)
			require.Equal(t, identity.Node(), decodeIssuedResponse(t, encoded).Node)
			require.Equal(t, types.UID(testNodeUID), hint.Node.UID)
			require.Equal(t, f.request.Shares, hint.Shares)
			require.Equal(t, identity.Expires(), hint.Expires)
		})
	}
}

func TestBootstrapBlockDevices(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		want    string
		warning bool
	}{
		{name: "absent"},
		{name: "empty"},
		{name: "valid", pattern: `^nvme-eui\.[0-9a-f]+$`, want: `^nvme-eui\.[0-9a-f]+$`},
		{name: "invalid", pattern: "[", warning: true},
		{name: "oversized", pattern: strings.Repeat("a", 1025), warning: true},
		{name: "byte limit", pattern: strings.Repeat("é", 513), warning: true},
		{name: "at limit", pattern: strings.Repeat("a", 1024), want: strings.Repeat("a", 1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, status, token := authenticatedBootstrapFixture(t)
			a := f.a.authority
			a.bootstrap.Client = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					obj.(*authv1.TokenReview).Status = status
					return nil
				},
			})
			nodeReads := 0
			a.bootstrap.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}

					if node, ok := obj.(*corev1.Node); ok {
						nodeReads++
						if nodeReads == 1 {
							node.Annotations = map[string]string{wire.BlockDevicesAnnotation: "stale"}
						} else if tc.name == "absent" {
							node.Annotations = nil
						} else {
							node.Annotations = map[string]string{wire.BlockDevicesAnnotation: tc.pattern}
						}
					}

					return nil
				},
			})

			var logs strings.Builder

			logger := funcr.New(func(_, msg string) { logs.WriteString(msg) }, funcr.Options{})
			ctx := ctrl.LoggerInto(t.Context(), logger)
			request := httptest.NewRequest(http.MethodPost, wire.BootstrapPath, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			encoded, hint, err := a.EnrollWithHint(ctx, request, f.request)
			require.NoError(t, err)
			require.Equal(t, 2, nodeReads, "configuration must use the post-issuance live Node")
			response := decodeIssuedResponse(t, encoded)
			require.Equal(t, tc.want, response.BlockDevices)
			require.Equal(t, wire.NodeID(testNodeUID), response.Node)
			require.Equal(t, types.UID(testNodeUID), hint.Node.UID)
			require.Equal(t, f.request.Shares, hint.Shares)

			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &fields))
			_, present := fields["block_devices"]
			require.Equal(t, tc.want != "", present)
			require.Equal(t, tc.warning, strings.Contains(logs.String(), "warning: ignoring block device annotation"))

			if tc.warning {
				require.Contains(t, logs.String(), wire.BlockDevicesAnnotation)
				require.Contains(t, logs.String(), "file-backed storage")
			}
		})
	}
}
