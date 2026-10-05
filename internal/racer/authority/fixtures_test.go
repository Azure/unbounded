// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/membership"
	"github.com/Azure/unbounded/internal/racer/wire"
)

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

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return TopologyObservation{}, err
	}

	catalog, err := BuildCatalog(caches.Items)
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

	return TopologyObservation{Nodes: nodes, Catalog: catalog, Input: membership.Input{Nodes: nodes.Items, PodsByNode: pods, Ownership: ids.observed(), PeerPort: 8082}}, nil
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
