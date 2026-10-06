// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	member := r.Accepted[testNodeUID]
	member.Shares = 999
	r.Accepted[testNodeUID] = member

	require.Equal(t, before, a.accepted, "legacy view aliases authority history")

	update, err := a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err)
	delete(update.Members, testNodeUID)
	require.Equal(t, before, a.accepted, "annotation hints alias authority history")

	var node corev1.Node
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
	node.Annotations[wire.SharesAnnotation] = "7"
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
	write, stop, err := image.WriteContext(t.Context())
	require.NoError(t, err)

	defer stop()

	trust, stopTrust, err := a.TrustContext(t.Context())
	require.NoError(t, err)

	defer stopTrust()

	deadline, _ := trust.Deadline()

	require.NoError(t, a.Observe(t.Context()))

	nextDeadline, _ := trust.Deadline()
	require.Equal(t, deadline, nextDeadline)
	require.NoError(t, trust.Err())

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
	node.Annotations[wire.SharesAnnotation] = "7"
	require.NoError(t, f.a.Topology.Update(t.Context(), &node))
	_, err = a.PublishTopology(t.Context(), f.a.Topology.observeTopology)
	require.NoError(t, err)
	require.ErrorIs(t, write.Err(), context.Canceled, "replacement must synchronously revoke old image")
	require.NoError(t, trust.Err(), "publication replacement is not trust invalidation")

	secret := &corev1.Secret{}
	require.NoError(t, f.a.Topology.Get(t.Context(), client.ObjectKey{Namespace: a.config.Namespace, Name: a.config.CredentialsSecretName}, secret))
	require.NoError(t, f.a.Topology.Delete(t.Context(), secret))
	require.Error(t, a.Observe(t.Context()))
	require.ErrorIs(t, trust.Err(), context.Canceled)
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

// Test-local engines preserve whitebox crash-boundary scenarios. They are not
// exported production bridges and cannot be imported by root integrations.
type TopologyReconciler struct {
	client.Client
	APIReader    client.Reader
	Config       Config
	Publications *Publications
	Trust        *Trust
	CatalogGate  *CatalogGate
	Accepted     AcceptedMembers
	authority    *Authority
}
type KeyringReconciler struct {
	client.Client
	APIReader   client.Reader
	Config      Config
	Trust       *Trust
	CatalogGate *CatalogGate
	Now         func() time.Time
}
type Application struct {
	Topology  *TopologyReconciler
	Keyring   *KeyringReconciler
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
		Topology:  &TopologyReconciler{Client: c, APIReader: reader, Config: cfg, Publications: a.publications, Trust: a.trust, CatalogGate: a.gate, Accepted: make(AcceptedMembers), authority: a},
		Keyring:   &KeyringReconciler{Client: c, APIReader: reader, Config: cfg, Trust: a.trust, CatalogGate: a.gate},
		Server:    &fixtureServer{Bootstrap: a.bootstrap, Trust: a.trust, Publications: a.publications, Config: cfg},
	}
}

func (a *Application) Recover(ctx context.Context, writer client.Writer) error {
	return a.authority.Recover(ctx, writer)
}
func (r *TopologyReconciler) runtimeConfig() Config { return r.Config.effective() }
func (r *TopologyReconciler) engine() *publisher {
	return &publisher{Writer: r.Client, APIReader: r.APIReader, Config: r.runtimeConfig(), Publications: r.Publications, Trust: r.Trust}
}

func (r *TopologyReconciler) CommitVersion(ctx context.Context, p *PreparedPublication) (*CommittedPublication, error) {
	return r.engine().CommitVersion(ctx, p)
}

func (r *TopologyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if r.authority == nil {
		r.authority = &Authority{accepted: r.Accepted}
	}

	r.authority.publisher = r.engine()

	r.authority.gate = r.CatalogGate
	if r.authority.gate == nil {
		r.authority.gate = newCatalogGate()
	}

	update, err := r.authority.PublishTopology(ctx, r.observeTopology)
	if err == nil {
		r.Accepted = cloneAccepted(update.Members)
		err = r.annotate(ctx, update)
	}

	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: 10 * time.Millisecond}, nil
	}

	return ctrl.Result{}, err
}

func (r *TopologyReconciler) observeTopology(ctx context.Context) (TopologyObservation, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return TopologyObservation{}, err
	}

	var volumes racerv1.ClusterVolumeList
	if err := r.APIReader.List(ctx, &volumes); err != nil {
		return TopologyObservation{}, err
	}

	catalog, err := BuildCatalog(volumes.Items)
	if err != nil {
		return TopologyObservation{}, err
	}

	ids, err := readManagedWorkloadIdentities(ctx, r.APIReader, r.Config)
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

	return TopologyObservation{Nodes: nodes, Catalog: catalog, Input: members.Input{Nodes: nodes.Items, PodsByNode: pods, Ownership: ids.observed(), PeerPort: 8082}}, nil
}

func (r *TopologyReconciler) annotate(ctx context.Context, update TopologyHints) error {
	for i := range update.Nodes.Items {
		node := &update.Nodes.Items[i]

		member, ok := update.Members[wire.NodeID(node.UID)]
		if !ok {
			continue
		}

		encoded, err := json.Marshal(member)
		if err != nil {
			return err
		}

		if node.Annotations[admittedMemberAnnotation] == string(encoded) {
			continue
		}

		before := node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}

		node.Annotations[admittedMemberAnnotation] = string(encoded)
		if err := r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	return nil
}
func (r *KeyringReconciler) runtimeConfig() Config { return r.Config.effective() }
func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	gate := r.CatalogGate
	if gate == nil {
		gate = newCatalogGate()
	}

	a := &Authority{gate: gate, credentials: &credentials{Writer: r.Client, APIReader: r.APIReader, Config: r.runtimeConfig(), Trust: r.Trust, Now: r.Now}}

	delay, err := a.ReconcileCredentials(ctx)
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: 10 * time.Millisecond}, nil
	}

	return ctrl.Result{RequeueAfter: delay}, err
}

const (
	podNodeIndex       = "spec.nodeName"
	retryConflictDelay = 10 * time.Millisecond
)

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
	CatalogGate          = catalogGate
	Issuer               = issuer
	Bootstrap            = bootstrap
	PreparedPublication  = preparedPublication
	CommittedPublication = committedPublication
	RotationState        = rotationState
	VersionRecord        = versionRecord
)

func NewPublications() *publicationStore { return newPublications() }

var (
	AuthenticateCertificate = authenticateCertificate
	PlanRotation            = planRotationOnly
)

func (r *KeyringReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC().Truncate(time.Second)
	}

	return time.Now().UTC().Truncate(time.Second)
}

func catalogVolume(name string, uid types.UID) racerv1.ClusterVolume {
	return racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
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
	for _, obj := range []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: cfg.InstallationConfigMapName}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh"}}} {
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

func BuildCatalog(volumes []racerv1.ClusterVolume) ([]wire.CacheDefinition, error) {
	return members.BuildCatalog(volumes)
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

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../../deploy/racer/crd"}, ErrorIfCRDPathMissing: true}

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
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}

		if err := racerv1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}

		marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh"}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(marker).Build()
		now := time.Now()

		a := New(cfg, Dependencies{Reader: c, Writer: c, Now: func() time.Time { return now }})
		if err := a.Recover(t.Context(), c); err != nil {
			t.Fatal(err)
		}

		if _, err := a.ReconcileCredentials(t.Context()); err != nil {
			t.Fatal(err)
		}

		old, err := a.Keyring()
		if err != nil {
			t.Fatal(err)
		}

		admitted, stop, err := a.TrustContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer stop()

		deadline, _ := admitted.Deadline()

		var before bytes.Buffer
		if _, err := old.Response().WriteTo(admitted, &before); err != nil {
			t.Fatal(err)
		}

		time.Sleep(3 * time.Second)

		now = now.Add(cfg.Rotation.Interval - cfg.Rotation.PrepareFor)

		if _, err := a.ReconcileCredentials(t.Context()); err != nil {
			t.Fatal(err)
		}

		current, err := a.Keyring()
		if err != nil {
			t.Fatal(err)
		}

		if current.Generation() <= old.Generation() {
			t.Fatal("ordinary rotation did not advance bundle")
		}

		fresh, stopFresh, err := a.TrustContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer stopFresh()

		if _, err := old.Response().WriteTo(fresh, io.Discard); !errors.Is(err, wire.Forbidden) {
			t.Fatal("superseded handle borrowed fresh admission", err)
		}

		if _, err := current.Response().WriteTo(fresh, io.Discard); err != nil {
			t.Fatal(err)
		}

		var after bytes.Buffer
		if _, err := old.Response().WriteTo(admitted, &after); err != nil {
			t.Fatal("rotation revoked admitted write", err)
		}

		if !bytes.Equal(before.Bytes(), after.Bytes()) {
			t.Fatal("admitted encoding changed")
		}

		if got, _ := admitted.Deadline(); got != deadline {
			t.Fatal("rotation extended admitted deadline")
		}

		time.Sleep(2 * time.Second)

		if _, err := old.Response().WriteTo(admitted, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("old write outlived pinned freshness", err)
		}
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
	if _, _, err := handle.WriteContext(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatal("zero publication handle accepted", err)
	}

	if _, err := handle.ForBase("").WriteTo(context.Background(), io.Discard); !errors.Is(err, wire.Forbidden) {
		t.Fatal("unguarded response accepted", err)
	}

	for _, value := range []any{NodeIdentity{}, ReplicaIdentity{}, PublicationHandle{}, KeyringHandle{}, Response{}, *a} {
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
	other, stopOther, err := b.TrustContext(t.Context())
	require.NoError(t, err)

	defer stopOther()

	_, _, err = p.WriteContextWithTrust(t.Context(), other)
	require.ErrorIs(t, err, wire.Forbidden)
	_, err = p.ForBase("").WriteTo(other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)

	var zero PublicationHandle

	_, _, err = zero.WriteContext(t.Context())
	require.ErrorIs(t, err, wire.Unavailable)
	_, err = zero.ForBase("").WriteTo(t.Context(), io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	keyring, err := a.Keyring()
	require.NoError(t, err)
	_, err = keyring.Response().WriteTo(other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	_, err = keyring.Response().WriteTo(t.Context(), io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)

	var empty KeyringHandle

	_, err = empty.Response().WriteTo(other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
}

func TestKeyringHandleCannotBorrowRecoveredTrust(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	legacy, stopLegacy, err := a.trust.writeContext(t.Context())
	require.NoError(t, err)

	defer stopLegacy()

	old, err := a.Keyring()
	require.NoError(t, err)
	guard, stop, err := a.TrustContext(t.Context())
	require.NoError(t, err)

	defer stop()

	a.trust.invalidate()
	require.ErrorIs(t, legacy.Err(), context.Canceled)
	require.NoError(t, a.Observe(t.Context()))
	require.ErrorIs(t, guard.Err(), context.Canceled)
	fresh, stopFresh, err := a.TrustContext(t.Context())
	require.NoError(t, err)

	defer stopFresh()

	_, err = old.Response().WriteTo(fresh, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	current, err := a.Keyring()
	require.NoError(t, err)
	_, err = current.Response().WriteTo(fresh, io.Discard)
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

	require.Equal(t, f.a.Topology.Config.Cluster, a.bootstrap.runtimeConfig().Cluster)
	require.Equal(t, 2*time.Minute, a.bootstrap.Issuer.runtimeConfig().CertificateLifetime)
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

func capacityVolumes(count int) []racerv1.ClusterVolume {
	volumes := make([]racerv1.ClusterVolume, count)
	for i := range volumes {
		volumes[i] = catalogVolume(fmt.Sprintf("capacity-%d", i), types.UID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)))
	}

	return volumes
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

		for _, volume := range capacityVolumes(count) {
			for range 2 {
				for _, purpose := range []wire.KeyPurpose{wire.PageKey, wire.OriginCredentialsKey} {
					keys = append(keys, map[string]any{"cache": volume.UID, "id": make([]byte, 16), "purpose": purpose, "state": wire.PreparedKey, "material": make([]byte, 32)})
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

			for _, volume := range capacityVolumes(capacity - 1) {
				if err := r.Create(t.Context(), &volume); err != nil {
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

			if policy.Interval == 6*time.Hour && maxRoots < 8 {
				t.Fatalf("did not exercise multiple retiring generations: %d", maxRoots)
			}
		})
	}
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

	volumes := capacityVolumes(capacity + 1)
	for _, volume := range volumes {
		if err := r.Create(t.Context(), &volume); err != nil {
			t.Fatal(err)
		}
	}

	if got := reconcileTopology(t, a.Topology, t.Context()); got != first {
		t.Fatal("published growth before its keys were committed")
	}

	var logs strings.Builder

	logger := funcr.New(func(_, msg string) { logs.WriteString(msg) }, funcr.Options{})

	ctx := ctrl.LoggerInto(t.Context(), logger)
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil || !trustReady(r.Trust) {
		t.Fatalf("growth disabled healthy service: %v", err)
	}

	if !strings.Contains(logs.String(), "rotation_capacity") || !strings.Contains(logs.String(), volumes[capacity].Name) {
		t.Fatal("capacity rejection was not observable")
	}

	_, admitted, _, _ := keyState(t, r)

	ids := keyedCaches(admitted)
	if !ids[wire.CacheID(testNodeUID)] || len(ids) != capacity || ids[wire.CacheID(volumes[capacity-1].UID)] {
		t.Fatal("growth displaced established UID or ignored sorted free-slot order")
	}

	assertPublishedKeys(t, a.Topology, capacity)
	// Removing a rejected candidate must neither change keys nor consume a
	// publication sequence. A restart retains the admitted set from the Secret.
	before := reconcileTopology(t, a.Topology, t.Context())
	if err := r.Delete(t.Context(), &volumes[capacity]); err != nil {
		t.Fatal(err)
	}

	r = Assemble(r.Config, r.Client, r.APIReader).Keyring
	runKeys(t, r)

	if after := reconcileTopology(t, a.Topology, t.Context()); after != before {
		t.Fatal("rejected deletion changed publication")
	}

	if err := r.Delete(t.Context(), &volumes[0]); err != nil {
		t.Fatal(err)
	}

	assertPublishedKeys(t, a.Topology, capacity-1)
	runKeys(t, r)
	assertPublishedKeys(t, a.Topology, capacity)

	_, replaced, _, _ := keyState(t, r)
	if keyedCaches(replaced)[wire.CacheID(volumes[0].UID)] || !keyedCaches(replaced)[wire.CacheID(volumes[capacity-1].UID)] {
		t.Fatal("deletion did not admit next waiting UID")
	}
	// Missing/corrupt durable credentials remain fail-closed in both controllers.
	shared := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.CredentialsSecretName}}
	if err := r.Delete(t.Context(), shared); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Topology.Reconcile(t.Context(), ctrl.Request{}); err == nil {
		t.Fatal("missing credentials accepted by topology")
	}

	if _, err := a.Topology.Publications.Current(); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("missing credentials did not suspend publication: %v", err)
	}
}

func assertPublishedKeys(t *testing.T, r *TopologyReconciler, count int) {
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

	input := capacityVolumes(capacity + 2)
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
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.TooLarge) {
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

	volumes := capacityVolumes(capacity)
	for _, volume := range volumes {
		if err := r.Create(t.Context(), &volume); err != nil {
			t.Fatal(err)
		}
	}

	volumes = append(volumes, catalogVolume("cache", testNodeUID))

	catalog, err := BuildCatalog(volumes)
	if err != nil {
		t.Fatal(err)
	}
	// Model the older controller's active-only admission without removing the
	// planner's independent final wire-size check.
	b, state, err = PlanRotation(r.Config.Rotation, b, state, catalog, *now)
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

	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, wire.TooLarge) || trustReady(r.Trust) {
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
		_, err := a.Topology.Reconcile(t.Context(), ctrl.Request{})
		done <- err
	}()

	<-read
	// The keyring must be excluded for the entire read/commit/install window.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := a.Keyring.CatalogGate.Acquire(ctx)
	if err == nil {
		a.Keyring.CatalogGate.Release()
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

	if err := a.Keyring.CatalogGate.Acquire(ctx); err != nil {
		t.Fatalf("publication did not release admission gate: %v", err)
	}

	a.Keyring.CatalogGate.Release()
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
		volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("capacity-real-%d", i)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
		if err := c.Create(t.Context(), volume); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := c.Delete(context.Background(), volume); err != nil {
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
