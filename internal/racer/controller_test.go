// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestMixedNetworkConfiguration(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":  "11111111-1111-1111-1111-111111111111",
		"RACER_CONTROL_URL": "https://controller:8443", "RACER_DATAPLANE_IMAGE": "racer:5e1",
		"RACER_HOST_NETWORK": "true",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	for _, input := range []string{`[]`, `["node-b","node-a"]`} {
		values["RACER_POD_NETWORK_NODES"] = input
		_, err := members.ConfigFromLookup(lookup)
		require.NoError(t, err)
	}

	for _, input := range []string{"", "null", `{}`, `"node-a"`, `[1]`, `[null]`, `[""]`, `["Node-A"]`, `["node-a","node-a"]`, `["node-a"] trailing`} {
		values["RACER_POD_NETWORK_NODES"] = input
		_, err := members.ConfigFromLookup(lookup)
		require.ErrorIs(t, err, wire.InvalidRequest, input)
	}

	values["RACER_POD_NETWORK_NODES"] = `["node-a"]`
	values["RACER_HOST_NETWORK"] = "false"
	_, err := members.ConfigFromLookup(lookup)
	require.ErrorIs(t, err, wire.InvalidRequest)
}

func TestMixedNetworkBuilders(t *testing.T) {
	cfg := workloadConfig(t)
	legacy, err := members.DesiredDaemonSet(cfg)
	require.NoError(t, err)
	sets, err := members.DesiredDaemonSets(cfg)
	require.NoError(t, err)
	require.Equal(t, []*appsv1.DaemonSet{legacy}, sets)

	cfg.HostNetwork = true
	cfg.PeerPort, cfg.DiagnosticsPort = 18082, 19090
	cfg.PodNetworkNodes = []string{"node-b", "node-a"}
	_, err = members.DesiredDaemonSet(cfg)
	require.ErrorIs(t, err, wire.InvalidRequest, "legacy planner must fail closed")
	sets, err = members.DesiredDaemonSets(cfg)
	require.NoError(t, err)
	require.Len(t, sets, 2)
	host, pod := sets[0], sets[1]
	require.Equal(t, DataplaneDaemonSetName, host.Name)
	require.Equal(t, PodNetworkDaemonSetName, pod.Name)
	require.Equal(t, legacy.Spec.Selector, host.Spec.Selector)
	require.True(t, host.Spec.Template.Spec.HostNetwork)
	require.False(t, pod.Spec.Template.Spec.HostNetwork)
	require.Equal(t, corev1.DNSClusterFirst, pod.Spec.Template.Spec.DNSPolicy)
	require.Equal(t, host.Spec.Template.Spec.Volumes, pod.Spec.Template.Spec.Volumes)
	require.Equal(t, host.Spec.Template.Spec.ServiceAccountName, pod.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, host.Spec.Template.Spec.Containers, pod.Spec.Template.Spec.Containers)

	for i, ds := range sets {
		selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
		require.NoError(t, err)
		require.True(t, selector.Matches(labels.Set(ds.Spec.Template.Labels)))
		require.False(t, selector.Matches(labels.Set(sets[1-i].Spec.Template.Labels)))
	}

	hostTerms := host.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	podTerms := pod.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms

	require.Len(t, hostTerms, 1)
	require.Len(t, hostTerms[0].MatchFields, 2)
	require.Len(t, podTerms, 2)

	for i, node := range []string{"node-a", "node-b"} {
		require.Equal(t, corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node}}, hostTerms[0].MatchFields[i])
		require.Equal(t, []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}, podTerms[i].MatchFields)
		require.Equal(t, hostTerms[0].MatchExpressions, podTerms[i].MatchExpressions)
	}

	require.Equal(t, []string{"node-b", "node-a"}, cfg.PodNetworkNodes)
}

func TestMixedNetworkIdentities(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	host := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: DataplaneDaemonSetName, UID: "host-current"}}
	podnet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "pod-current"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(host, podnet).Build()
	ids, err := readManagedWorkloadIdentities(t.Context(), reader, Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName})
	require.NoError(t, err)

	for _, ds := range []*appsv1.DaemonSet{host, podnet} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}}
		require.Equal(t, ds.Name == host.Name, ids.Owns(pod))

		for _, mutate := range []func(*corev1.Pod){
			func(p *corev1.Pod) { p.Namespace = "other" },
			func(p *corev1.Pod) { p.OwnerReferences[0].UID = "stale" },
			func(p *corev1.Pod) { p.OwnerReferences[0].UID = "" },
			func(p *corev1.Pod) { p.OwnerReferences[0].Name = "arbitrary" },
			func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
			func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "apps/v2" },
			func(p *corev1.Pod) { p.OwnerReferences = nil; p.Labels = ds.Labels },
		} {
			bad := pod.DeepCopy()
			mutate(bad)
			require.False(t, ids.Owns(bad))
		}

		require.False(t, (DataplaneWorkloadIdentities{}).Owns(pod))
	}

	require.False(t, ids.Owns(nil))
	require.NoError(t, reader.Delete(t.Context(), podnet))
	ids, err = readManagedWorkloadIdentities(t.Context(), reader, Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName})
	require.NoError(t, err)
	require.Equal(t, host.Name, ids.Name)
	require.Equal(t, host.UID, ids.UID)
}

func TestAssemble(t *testing.T) {
	// Nil Kubernetes dependencies make unintended constructor API calls fail.
	a := Assemble(Config{}, nil, nil)
	require.NotNil(t, a.authority, "bootstrap must have an issuer")
	require.Same(t, a.authority, a.Topology.authority)
	require.Same(t, a.authority, a.Keyring.authority, "controllers, issuance, and serving must share trust and catalog gate")
	require.Same(t, a.Lifecycle, a.Server.Lifecycle)
	require.Same(t, a.Replication, a.Server.Leader)
	require.NotNil(t, a.Server)

	for _, cfg := range []Config{a.Topology.config, a.Keyring.config, a.Replication.config} {
		require.Equal(t, wire.CertificateLifetime, cfg.CertificateLifetime)
		require.Equal(t, 30*time.Second, cfg.SnapshotMaxAge)
	}

	require.Equal(t, a.Topology.config.SnapshotMaxAge, a.Replication.config.SnapshotMaxAge)
	require.Equal(t, a.Keyring.config.SnapshotMaxAge, a.Replication.config.SnapshotMaxAge)
	require.Error(t, a.authority.PublicationReady())
	require.False(t, a.Server.NeedLeaderElection())
	require.False(t, a.Replication.NeedLeaderElection())
	require.ErrorIs(t, a.Server.Ready(nil), wire.Unavailable)

	// Composition freezes observer inputs before exposing any server entry point.
	want := a.Replication.config

	cfg := Config{SnapshotMaxAge: time.Hour}
	require.NotEqual(t, want, cfg)
	require.Equal(t, want, a.Replication.config, "replication settings must be captured before serving composition")
}

func TestFailClosedEntryPoints(t *testing.T) {
	a := Assemble(Config{}, nil, nil)
	ctx := context.Background()

	operations := map[string]func() error{
		"topology": func() error { _, err := a.Topology.Reconcile(ctx, ctrl.Request{}); return err },
		"keyring":  func() error { _, err := a.Keyring.Reconcile(ctx, ctrl.Request{}); return err },
		"workload": func() error { _, err := members.DesiredDaemonSet(members.Config{}); return err },
		"server":   func() error { return a.Server.Start(ctx) },
		"run":      func() error { return Run(ctx, Config{}) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil {
				t.Fatalf("entry point did not fail closed: %v", err)
			}
		})
	}
}

func TestScaffoldRoutesCannotAuthenticate(t *testing.T) {
	handler := Assemble(Config{}, nil, nil).Server.Handler()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, wire.BootstrapPath},
		{http.MethodGet, wire.SnapshotPath},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))

			if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"code":"unavailable"}` {
				t.Fatalf("unexpected scaffold response: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSingletonCoalescesObjects(t *testing.T) {
	requests := singleton(context.Background(), nil)
	if len(requests) != 1 || requests[0].Name != "racer" || requests[0].Namespace != "" {
		t.Fatalf("unexpected singleton requests: %v", requests)
	}
}

func TestInitialEnqueueEmptyInputsAndCoalescing(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	for range 10 {
		if err := initialEnqueue().Start(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}

	if q.Len() != 1 {
		t.Fatalf("startup events not coalesced: %d", q.Len())
	}

	request, stopped := q.Get()
	if stopped || request != singleton(context.Background(), nil)[0] {
		t.Fatalf("initial request: %+v", request)
	}

	q.Done(request)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := initialEnqueue().Start(ctx, q); !errors.Is(err, context.Canceled) || q.Len() != 0 {
		t.Fatalf("canceled startup queued: %v", err)
	}
}

func TestTopologyWatchFiltering(t *testing.T) {
	cfg := testConfig(t)
	node := memberNode()
	updated := node.DeepCopy()

	updated.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("node readiness enqueued topology")
	}

	updated.Annotations = map[string]string{wire.SharesAnnotation: ""}
	if !nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("absent -> invalid empty annotation lost")
	}

	updated.Annotations = nil

	updated.Labels = map[string]string{wire.ExclusionLabel: "false"}
	if !nodeChanges().Update(event.UpdateEvent{ObjectOld: &node, ObjectNew: updated}) {
		t.Fatal("exclusion presence lost")
	}

	pod := memberPod("pod", 1, "192.0.2.1")
	pod.OwnerReferences[0].Name = cfg.DaemonSetName

	pred := managedPodChanges(cfg)
	if !pred.Create(event.CreateEvent{Object: &pod}) {
		t.Fatal("managed pod ignored")
	}

	changed := pod.DeepCopy()

	changed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if pred.Update(event.UpdateEvent{ObjectOld: &pod, ObjectNew: changed}) {
		t.Fatal("pod readiness enqueued topology")
	}

	changed.OwnerReferences = nil
	if pred.Create(event.CreateEvent{Object: changed}) || !pred.Update(event.UpdateEvent{ObjectOld: &pod, ObjectNew: changed}) {
		t.Fatal("ownership loss filtering")
	}

	changed = pod.DeepCopy()

	changed.Namespace = "unrelated"
	if pred.Create(event.CreateEvent{Object: changed}) {
		t.Fatal("foreign namespace pod admitted")
	}

	if keys := podNodeKeys(&pod); len(keys) != 1 || keys[0] != pod.Spec.NodeName {
		t.Fatalf("node index: %v", keys)
	}

	pod.Spec.NodeName = ""
	if len(podNodeKeys(&pod)) != 0 {
		t.Fatal("unassigned pod indexed")
	}

	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace, cm.ResourceVersion = cfg.VersionConfigMapName, cfg.Namespace, "1"
	newCM := cm.DeepCopy()

	newCM.ResourceVersion = "2"
	if versionChanges(cfg).Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: newCM}) {
		t.Fatal("CAS-only write caused reconcile loop")
	}

	newCM.Data = map[string]string{"sequence": "2"}
	if !versionChanges(cfg).Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: newCM}) {
		t.Fatal("version change ignored")
	}
}

// Opt-in, but never silently skip when assets were explicitly supplied. envtest
// runs real etcd/apiserver processes; it has no kubelet or workload controllers.
func TestEnvtestServer(t *testing.T) {
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

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../deploy/racer/crd"}, ErrorIfCRDPathMissing: true}

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

	t.Run("initialization-and-CAS", func(t *testing.T) { integrationInitialization(t, c) })
	t.Run("volume-name-admission", func(t *testing.T) { integrationVolumeNameAdmission(t, c) })
	t.Run("volume-type-admission", func(t *testing.T) { integrationVolumeTypeAdmission(t, c) })
	t.Run("volume-type-immutability", func(t *testing.T) { integrationVolumeTypeImmutability(t, c) })
	t.Run("rotation-crash-recovery", func(t *testing.T) { integrationRotation(t, c) })
	t.Run("manager-election-HTTPS-failover", func(t *testing.T) { integrationManagers(t, rc, scheme, c) })
}

func integrationInstallation(t *testing.T, c client.Client, namespace string) *Application {
	t.Helper()
	cfg := testConfig(t)
	cfg.Namespace = namespace

	cfg.MetricsAddress, cfg.ProbeAddress = "0", "0"
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: cfg.InstallationConfigMapName}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh", "initialization_protocol": "staged-v1"}},
	} {
		if err := c.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}

	return assembleFixture(cfg, c, c)
}

type interruptedClient struct {
	client.Client
	update func(context.Context, client.Object, ...client.UpdateOption) error
	create func(context.Context, client.Object, ...client.CreateOption) error
}

func (c interruptedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.update != nil {
		return c.update(ctx, obj, opts...)
	}

	return c.Client.Update(ctx, obj, opts...)
}

func (c interruptedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.create != nil {
		return c.create(ctx, obj, opts...)
	}

	return c.Client.Create(ctx, obj, opts...)
}

func integrationInitialization(t *testing.T, c client.Client) {
	ctx := t.Context()
	concurrent := integrationInstallation(t, c, "init-concurrent")
	arrived := make(chan struct{}, 2)
	proceed := make(chan struct{})
	results := make(chan error, 2)

	for range 2 {
		r := Assemble(concurrent.Topology.config, interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
			arrived <- struct{}{}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-proceed:
			}

			return c.Update(ctx, obj, opts...)
		}}, c).Topology

		go func() { results <- r.authority.Recover(ctx, r.Client) }()
	}

	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(proceed)
			t.Fatal("concurrent initializers did not reach marker CAS")
		}
	}

	close(proceed)

	winners := 0

	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("marker CAS loser: %v", err)
		}
	}

	if winners != 2 {
		t.Fatalf("successful concurrent startups: %d", winners)
	}

	integrationInitializationCAS(t, c)
	integrationInitializationCrash(t, c)
}

func integrationInitializationCAS(t *testing.T, c client.Client) {
	t.Helper()
	ctx := t.Context()
	a := integrationInstallation(t, c, "init-cas")

	r := a.Topology
	if err := a.Recover(ctx, r.Client); err != nil {
		t.Fatal(err)
	}

	marker, err := readInstallation(ctx, r.APIReader, r.config, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, mutation := range []func(*corev1.ConfigMap){
		func(cm *corev1.ConfigMap) { cm.Data["state"] = "fresh" },
		func(cm *corev1.ConfigMap) { cm.Immutable = ptr.To(false) },
	} {
		copy := marker.DeepCopy()
		mutation(copy)

		if err := c.Update(ctx, copy); !apierrors.IsInvalid(err) {
			t.Fatalf("API server allowed immutable marker rollback: %v", err)
		}
	}

	if err := a.Recover(ctx, r.Client); err != nil {
		t.Fatalf("installed startup rejected: %v", err)
	}

	cm, _, err := readVersion(ctx, r.APIReader, r.config)
	if err != nil {
		t.Fatal(err)
	}

	// Race after CommitVersion's authoritative read, so the API server, rather
	// than our preliminary resourceVersion comparison, must reject the write.
	writer := interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
		other := cm.DeepCopy()

		other.Labels = map[string]string{"concurrent": "writer"}
		if err := c.Update(ctx, other); err != nil {
			return err
		}

		return c.Update(ctx, obj, opts...)
	}}

	owner := authority.New(r.config.authorityConfig(), authority.Dependencies{Reader: c, Writer: writer})
	if _, err := owner.PublishTopology(ctx, r.observeTopology); !apierrors.IsConflict(err) {
		t.Fatalf("real CAS failed: err=%v", err)
	}

	r.Client = c

	cm, _, err = readVersion(ctx, r.APIReader, r.config)
	if err != nil {
		t.Fatal(err)
	}

	leader, cancel := context.WithCancel(ctx)

	owner = authority.New(r.config.authorityConfig(), authority.Dependencies{Reader: c, Writer: interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
		err := c.Update(ctx, obj, opts...)

		cancel()

		return err
	}}})
	if _, err := owner.PublishTopology(leader, r.observeTopology); !errors.Is(err, context.Canceled) {
		t.Fatalf("late install: %v", err)
	}
}

func integrationInitializationCrash(t *testing.T, c client.Client) {
	t.Helper()

	ctx := t.Context()
	for _, afterCreate := range []bool{false, true} {
		a := integrationInstallation(t, c, fmt.Sprintf("init-crash-%t", afterCreate))
		boom := errors.New("ambiguous initialization response")

		a.Topology.Client = interruptedClient{Client: c, create: func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
			if afterCreate {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
			}

			return boom
		}}
		if err := a.Recover(ctx, a.Topology.Client); !errors.Is(err, boom) {
			t.Fatal(err)
		}

		restarted := Assemble(a.Topology.config, c, c)
		if err := restarted.Recover(ctx, c); err != nil {
			t.Fatalf("ambiguous initialization recovery: %v", err)
		}

		if _, _, err := readVersion(ctx, restarted.Topology.APIReader, restarted.Topology.config); err != nil {
			t.Fatalf("crash recovery afterCreate=%t: %v", afterCreate, err)
		}
	}
}

func integrationRotation(t *testing.T, c client.Client) {
	a := integrationInstallation(t, c, "rotation")
	cfg := a.Keyring.config
	cfg.Rotation.Interval = 7 * 24 * time.Hour

	a = assembleFixture(cfg, c, c)
	require.NoError(t, a.Recover(t.Context(), a.Topology.Client))

	volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "rotation-cache"}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
	if err := c.Create(t.Context(), volume); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := c.Delete(context.Background(), volume); err != nil {
			t.Error(err)
		}
	})

	r := a.Keyring
	now := time.Now().UTC().Truncate(time.Second)
	fixtureDependencies[a.authority].now = func() time.Time { return now }

	runKeys(t, r)
	_, initial, state, _ := keyState(t, r)
	now = state.NextRotation
	oldIssuer := state.ActiveIssuer
	// Interrupt each actual write boundary, including a committed response lost
	// during activation. Every recovery uses a fresh application and real reads.
	for _, step := range []struct {
		name   string
		secret string
		after  bool
	}{{"stage-private", r.config.CredentialsSecretName, true}, {"activate-bundle", r.config.CredentialsSecretName, true}, {"prune-private", r.config.CredentialsSecretName, false}} {
		boom := errors.New(step.name)
		failed := false

		fixtureDependencies[r.authority].Client = interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() != step.secret {
				return c.Update(ctx, obj, opts...)
			}

			failed = true

			if step.after {
				if err := c.Update(ctx, obj, opts...); err != nil {
					return err
				}
			}

			return boom
		}}
		_, err := r.Reconcile(t.Context(), ctrl.Request{})
		require.True(t, failed)
		require.ErrorIs(t, err, boom)
		require.Error(t, r.authority.TrustReady())

		_, before, beforeState, private := keyState(t, r)
		recoveredApp := assembleFixture(r.config, c, c)
		recovered := recoveredApp.Keyring
		fixtureDependencies[recovered.authority].now = func() time.Time { return now }
		runKeys(t, recovered)
		_, after, next, material := keyState(t, recovered)

		switch step.name {
		case "stage-private":
			require.Equal(t, initial.Generation+1, before.Generation)
			require.Len(t, private.Keys, 2)
			require.Equal(t, beforeState.PreparedIssuer, next.PreparedIssuer)
			require.Len(t, after.CacheKeys, 4)

			now = next.ActivateAt
		case "activate-bundle":
			require.Equal(t, before.Generation, after.Generation)
			require.Equal(t, beforeState.ActiveIssuer, next.ActiveIssuer)
			require.Len(t, next.Retiring, 1)
			require.Len(t, after.CacheKeys, 2)

			now = next.Retiring[oldIssuer]
		case "prune-private":
			require.True(t, containsRoot(before, oldIssuer))
			require.Len(t, private.Keys, 2)
			require.Len(t, material.Keys, 1)
			require.Equal(t, before.Generation+1, after.Generation)
			require.Len(t, after.CacheKeys, 2)
			require.False(t, containsRoot(after, oldIssuer))
		}

		r = recovered
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func eventually(t *testing.T, description string, f func() bool) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("timed out: " + description)
}

func integrationManagers(t *testing.T, rc *rest.Config, scheme *runtime.Scheme, c client.Client) {
	a := integrationInstallation(t, c, "managers")
	if err := a.Recover(t.Context(), a.Topology.Client); err != nil {
		t.Fatal(err)
	}

	cfg := a.Topology.config
	cfg.ControllerServiceAccount = "racer-controller"
	cfg.ReplicationServerName = "racer-controller.managers.svc"
	roots := integrationTLS(t, &cfg)
	cfg.ReplicationTrustFile = cfg.TLSCertificateFile

	_, port, err := net.SplitHostPort(unusedAddress(t))
	if err != nil {
		t.Fatal(err)
	}

	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ReplicationPort = uint16(number)

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.ControllerServiceAccount}}
	if err := c.Create(t.Context(), sa); err != nil {
		t.Fatal(err)
	}

	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}

	var (
		apps              [2]*Application
		cancels           [2]context.CancelFunc
		done              [2]chan error
		denyRenewal       [2]atomic.Bool
		topologyCommitted [2]atomic.Bool
	)

	for i := range apps {
		apps[i], cancels[i], done[i] = startIntegrationManager(t, rc, scheme, c, cfg, kube, sa, i, port, &denyRenewal[i], &topologyCommitted[i])
	}

	leader := -1

	eventually(t, "elected manager becomes ready", func() bool {
		for i, app := range apps {
			if app.Replication.isLeader() && app.Server.Ready(nil) == nil {
				leader = i
				return true
			}
		}

		return false
	})

	follower := 1 - leader

	eventually(t, "follower installs replicated snapshot and serves", func() bool { return apps[follower].Server.Ready(nil) == nil })
	integrationManagerFailover(t, rc, c, cfg, roots, apps, cancels, done, &denyRenewal[leader], &topologyCommitted[follower], leader)
}

func startIntegrationManager(t *testing.T, rc *rest.Config, scheme *runtime.Scheme, c client.Client, cfg Config, kube kubernetes.Interface, sa *corev1.ServiceAccount, i int, port string, denyRenewal, topologyCommitted *atomic.Bool) (*Application, context.CancelFunc, chan error) {
	t.Helper()

	ip := fmt.Sprintf("127.0.0.%d", i+2)
	cfg.ControlAddress, cfg.ProbeAddress = net.JoinHostPort(ip, port), unusedAddress(t)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: fmt.Sprintf("controller-%d", i)}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "controller", Image: "example.invalid/controller:test"}}}}
	if err := c.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	pod.Status.PodIP = ip
	if err := c.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	cfg.PodName, cfg.PodUID = pod.Name, string(pod.UID)

	token := boundPodToken(t, kube, pod, sa.Name, ReplicationAudience)

	cfg.ReplicationTokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(cfg.ReplicationTokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	options := managerOptions(cfg, scheme)
	options.LeaseDuration, options.RenewDeadline, options.RetryPeriod = ptr.To(4*time.Second), ptr.To(2*time.Second), ptr.To(500*time.Millisecond)
	options.Controller.SkipNameValidation = ptr.To(true) // Two real managers in one test process.
	connection := rest.CopyConfig(rc)
	connection.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return transportFunc(func(req *http.Request) (*http.Response, error) {
			if denyRenewal.Load() && req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/leases/") {
				return nil, errors.New("injected Lease renewal partition")
			}

			return base.RoundTrip(req)
		})
	}

	lockClient, err := kubernetes.NewForConfig(connection)
	if err != nil {
		t.Fatal(err)
	}

	options.LeaderElectionResourceLockInterface = &resourcelock.LeaseLock{LeaseMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "racer-controller"}, Client: lockClient.CoordinationV1(), LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.PodName + "/" + cfg.PodUID}}

	mgr, err := ctrl.NewManager(connection, options)
	if err != nil {
		t.Fatal(err)
	}

	writer := interruptedClient{Client: mgr.GetClient(), update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
		if err := mgr.GetClient().Update(ctx, obj, opts...); err != nil {
			return err
		}

		if _, ok := obj.(*corev1.ConfigMap); ok && obj.GetName() == cfg.VersionConfigMapName {
			topologyCommitted.Store(true)
		}

		return nil
	}}

	app := Assemble(cfg, writer, mgr.GetAPIReader())
	if err := app.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("manager failed to stop")
		}
	})

	return app, cancel, done
}

func integrationManagerFailover(t *testing.T, rc *rest.Config, c client.Client, cfg Config, roots *x509.CertPool, apps [2]*Application, cancels [2]context.CancelFunc, done [2]chan error, denyRenewal, topologyCommitted *atomic.Bool, leader int) {
	t.Helper()

	follower := 1 - leader
	ds := integrationWorkloadDrift(t, c, cfg, apps)
	peer := integrationEnrollment(t, rc, c, apps[follower], ds, roots)
	integrationHTTPSFailover(t, rc, c, cfg, apps, cancels, done, denyRenewal, topologyCommitted, leader, peer)
}

func integrationWorkloadDrift(t *testing.T, c client.Client, cfg Config, apps [2]*Application) *appsv1.DaemonSet {
	t.Helper()

	for i, app := range apps {
		response, err := http.Get("http://" + app.Topology.config.ProbeAddress + "/readyz")

		want := 200

		if err != nil {
			t.Fatal(err)
		}

		response.Body.Close()

		if response.StatusCode != want {
			t.Fatalf("manager %d readiness: %d", i, response.StatusCode)
		}
	}
	// No Nodes/Pods/DaemonSets existed at startup. The ready Racer manager must
	// not provision workloads; simulate the operator's independent installation.
	ds := &appsv1.DaemonSet{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.DaemonSetName}, ds); !apierrors.IsNotFound(err) {
		t.Fatalf("Racer manager created a workload: %v", err)
	}

	workload, err := members.DesiredDaemonSet(members.Config{
		Cluster: cfg.Cluster, Namespace: cfg.Namespace,
		ControlURL: "https://127.0.0.1:8443", DataplaneImage: "example.invalid/racer:test",
		BootstrapTrustConfigMap: "racer-bootstrap-trust", PeerPort: cfg.PeerPort,
		DataplaneServiceAccount: cfg.DataplaneServiceAccount, DaemonSetName: cfg.DaemonSetName,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Create(t.Context(), workload); err != nil {
		t.Fatal(err)
	}

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(workload), ds); err != nil {
		t.Fatal(err)
	}

	if !ptr.Deref(ds.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem, false) {
		t.Fatal("operator-owned workload must initially have a read-only root filesystem")
	}

	ds.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = ptr.To(false)
	if err := c.Update(t.Context(), ds); err != nil {
		t.Fatal(err)
	}

	rv := ds.ResourceVersion

	time.Sleep(300 * time.Millisecond)

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds); err != nil || ds.ResourceVersion != rv {
		t.Fatalf("Racer manager mutated an operator-owned workload: %v", err)
	}

	if ptr.Deref(ds.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem, true) {
		t.Fatal("Racer manager reverted operator-owned security drift")
	}

	return ds
}

func integrationHTTPSFailover(t *testing.T, rc *rest.Config, c client.Client, cfg Config, apps [2]*Application, cancels [2]context.CancelFunc, done [2]chan error, denyRenewal, topologyCommitted *atomic.Bool, leader int, peer *http.Client) {
	t.Helper()

	follower := 1 - leader
	endpoint := "https://" + apps[leader].Topology.config.ControlAddress
	response, err := peer.Get(endpoint + wire.SnapshotPath)

	publication, err := wire.DecodePublication(bytes.NewReader(responseBody(t, response, err, 200)))
	if err != nil {
		t.Fatal(err)
	}
	// Establish a real authenticated pending HTTPS request before loss of Lease.
	eventually(t, "follower receives current image before failover", func() bool {
		p, err := apps[follower].authority.Current()
		return err == nil && p.Sequence() == publication.Sequence
	})

	followerResponse, followerErr := peer.Get("https://" + apps[follower].Topology.config.ControlAddress + wire.SnapshotPath)
	responseBody(t, followerResponse, followerErr, http.StatusOK)

	pollDone := integrationPendingPoll(peer, endpoint, publication.Sequence)

	// A duplicate authenticated poll observes admission through the public route.
	eventually(t, "leader parks authenticated poll", func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%s?after=%d", endpoint, wire.SnapshotPath, publication.Sequence), nil)
		if err != nil {
			t.Fatal(err)
		}

		duplicate, err := peer.Do(req)
		if err != nil {
			return false
		}
		defer duplicate.Body.Close()

		return duplicate.StatusCode == http.StatusTooManyRequests
	})

	lease := &coordv1.Lease{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: cfg.Namespace, Name: "racer-controller"}, lease); err != nil {
		t.Fatal(err)
	}

	oldHolder := *lease.Spec.HolderIdentity
	start := time.Now()

	denyRenewal.Store(true)
	eventually(t, "Lease renewal failure withdraws readiness", func() bool { return apps[leader].Server.Ready(nil) != nil })

	select {
	case err := <-pollDone:
		require.Error(t, err, "old leader poll completed instead of closing")
	case <-time.After(5 * time.Second):
		t.Fatal("old leader HTTPS poll survived cancellation")
	}

	select {
	case err := <-done[leader]:
		require.ErrorContains(t, err, "leader election lost")

		done[leader] <- err // Cleanup still joins this manager.
	case <-time.After(5 * time.Second):
		t.Fatal("lost leader did not stop")
	}

	if conn, err := net.DialTimeout("tcp", apps[leader].Topology.config.ControlAddress, time.Second); err == nil {
		conn.Close()
		t.Fatal("old leader listener still accepts after manager exit")
	}

	eventually(t, "follower takes expired Lease, commits topology, and serves", func() bool {
		return apps[follower].Replication.isLeader() && topologyCommitted.Load() && apps[follower].Server.Ready(nil) == nil
	})

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(lease), lease); err != nil || *lease.Spec.HolderIdentity == oldHolder {
		t.Fatalf("Lease did not change holder: %v", err)
	}

	_, committedVersion, err := readVersion(t.Context(), c, apps[follower].Topology.config)
	require.NoError(t, err)
	require.Equal(t, publication.Sequence, committedVersion.Sequence)
	require.Equal(t, publication.MembershipVersion, committedVersion.MembershipVersion)

	response, err = peer.Get("https://" + apps[follower].Topology.config.ControlAddress + wire.SnapshotPath)

	recovered, err := wire.DecodePublication(bytes.NewReader(responseBody(t, response, err, 200)))
	require.NoError(t, err)
	require.Equal(t, publication.Sequence, recovered.Sequence)
	require.Equal(t, publication.MembershipVersion, recovered.MembershipVersion)

	t.Logf("actual Lease failover and authenticated HTTPS recovery: %s; sequence=%d membership=%d", time.Since(start), recovered.Sequence, recovered.MembershipVersion)
	integrationAuthorizationLoad(t, rc, c, apps[follower], peer)

	integrationManagerStop(t, apps[follower], cancels[follower], done[follower])
}

func integrationPendingPoll(peer *http.Client, endpoint string, sequence wire.Sequence) <-chan error {
	done := make(chan error, 1)

	go func() {
		path := fmt.Sprintf("%s%s?after=%d", endpoint, wire.SnapshotPath, sequence)
		response, err := peer.Get(path)
		// A duplicate probe can win admission first. Wait for its short request.
		for attempts := 0; response != nil && response.StatusCode == http.StatusTooManyRequests && attempts < 20; attempts++ {
			response.Body.Close()
			time.Sleep(20 * time.Millisecond)

			response, err = peer.Get(path)
		}

		if response != nil {
			response.Body.Close()

			if response.StatusCode == http.StatusServiceUnavailable {
				err = wire.Unavailable
			}
		}

		done <- err
	}()

	return done
}

func integrationManagerStop(t *testing.T, app *Application, cancel context.CancelFunc, done chan error) {
	t.Helper()

	if err := app.Server.Ready(nil); err != nil {
		t.Fatalf("new leader not ready before normal cancellation: %v", err)
	}

	cancel()
	eventually(t, "manager cancellation withdraws readiness", func() bool { return app.Server.Ready(nil) != nil })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal manager cancellation: %v", err)
		}

		done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("canceled manager did not stop")
	}

	if _, err := app.authority.Current(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled manager still publishes: %v", err)
	}
}

type countedBody struct {
	io.ReadCloser
	bytes *atomic.Int64
}

func (b countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes.Add(int64(n))

	return n, err
}

func integrationAuthorizationLoad(t *testing.T, rc *rest.Config, c client.Client, a *Application, peer *http.Client) {
	t.Helper()
	// A separate owner uses the instrumented API dependency for every operation.
	// Validate replicated state through public operations before measuring serving.
	var requests, nodeLists, podLists, received atomic.Int64

	connection := rest.CopyConfig(rc)
	connection.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return transportFunc(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)

			if req.URL.Path == "/api/v1/nodes" {
				nodeLists.Add(1)
			}

			if strings.HasSuffix(req.URL.Path, "/pods") {
				podLists.Add(1)
			}

			response, err := base.RoundTrip(req)
			if err == nil {
				response.Body = countedBody{ReadCloser: response.Body, bytes: &received}
			}

			return response, err
		})
	}

	reader, err := client.New(connection, client.Options{Scheme: c.Scheme()})
	if err != nil {
		t.Fatal(err)
	}

	measuredApp := Assemble(a.Topology.config, reader, reader)

	measured := measuredApp.Server
	if err := measuredApp.authority.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}

	image, err := wire.DecodePublication(strings.NewReader(capturePublication(t, a.authority).encoded))
	if err != nil {
		t.Fatal(err)
	}

	if err := measuredApp.authority.AcceptReplica(t.Context(), t.Context(), image); err != nil {
		t.Fatal(err)
	}

	startFixtureLifecycle(t, measured.Lifecycle, t.Context())
	// Positive control on this same owner proves the measurement is connected.
	requests.Store(0)
	received.Store(0)

	if err := measuredApp.authority.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}

	require.NotZero(t, requests.Load())
	require.NotZero(t, received.Load())

	config, err := measured.TLSConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: measured.Handler(), TLSConfig: config}

	go func() { server.Serve(tls.NewListener(listener, config)) }()

	t.Cleanup(func() { server.Close() })

	endpoint := "https://" + listener.Addr().String() + wire.SnapshotPath
	// The first request includes a real TLS handshake. Neither path may read API state.
	if err := measuredApp.authority.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}

	requests.Store(0)
	received.Store(0)

	response, err := peer.Get(endpoint)
	responseBody(t, response, err, 200)

	if requests.Load() != 0 || received.Load() != 0 {
		t.Fatalf("TLS handshake/snapshot used API: requests=%d bytes=%d", requests.Load(), received.Load())
	}

	integrationAuthorizationScale(t, rc, c, a, measuredApp, peer, endpoint, &requests, &nodeLists, &podLists, &received)
}

func integrationAuthorizationScale(t *testing.T, rc *rest.Config, c client.Client, a, measuredApp *Application, peer *http.Client, endpoint string, requests, nodeLists, podLists, received *atomic.Int64) {
	t.Helper()

	for _, count := range []int{1, 1001} {
		if count > 1 {
			for i := range 1000 {
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("auth-scale-%04d", i), Labels: map[string]string{wire.ExclusionLabel: ""}}}
				if err := c.Create(t.Context(), node); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Include all added Nodes in the real informer. Controller watch traffic
		// is deliberately outside the request budget.
		eventually(t, "authorization discovery cache convergence", func() bool {
			var nodes corev1.NodeList
			return a.Topology.List(t.Context(), &nodes) == nil && len(nodes.Items) == count
		})

		// This standalone owner has no observer loop. Setup may outlast snapshot
		// freshness, so refresh outside the measured request window.
		if err := measuredApp.authority.Observe(t.Context()); err != nil {
			t.Fatal(err)
		}

		requests.Store(0)
		nodeLists.Store(0)
		podLists.Store(0)
		received.Store(0)

		start := time.Now()

		for range 10 {
			response, err := peer.Get(endpoint)
			responseBody(t, response, err, 200)
		}

		if requests.Load() != 0 || nodeLists.Load() != 0 || podLists.Load() != 0 {
			t.Fatalf("authorization API budget drift: requests=%d node_lists=%d pod_lists=%d", requests.Load(), nodeLists.Load(), podLists.Load())
		}

		if received.Load() != 0 {
			t.Fatalf("snapshot read API bytes: %d", received.Load())
		}

		t.Logf("real HTTPS authorization: live_nodes=%d snapshots=10 elapsed=%s API_requests=%d Node_lists=%d Pod_lists=%d API_response_bytes=%d (warm TLS; envtest QPS=%g burst=%d)", count, time.Since(start), requests.Load(), nodeLists.Load(), podLists.Load(), received.Load(), rc.QPS, rc.Burst)
	}
	// Exclusion changes routing membership, not authorization of issued identities.
	node := &corev1.Node{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "server-node"}, node); err != nil {
		t.Fatal(err)
	}

	node.Labels = map[string]string{wire.ExclusionLabel: ""}
	if err := c.Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}

	response, err := peer.Get(endpoint)
	responseBody(t, response, err, 200)
}

func integrationEnrollment(t *testing.T, rc *rest.Config, c client.Client, a *Application, ds *appsv1.DaemonSet, roots *x509.CertPool) *http.Client {
	t.Helper()

	cfg := a.Topology.config
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "server-node"}}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.DataplaneServiceAccount}}
	for _, obj := range []client.Object{node, sa} {
		require.NoError(t, c.Create(t.Context(), obj))
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-pod", Namespace: cfg.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}, Spec: *ds.Spec.Template.Spec.DeepCopy()}

	pod.Spec.NodeName = node.Name
	if err := c.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	pod.Status.PodIP = "192.0.2.1"
	if err := c.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}

	eventually(t, "managed Pod published through informer", func() bool {
		_, err := a.authority.Current()
		return err == nil && strings.Contains(capturePublication(t, a.authority).encoded, string(node.UID))
	})

	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}

	requestToken := func(audience string) string {
		return boundPodToken(t, kube, pod, sa.Name, audience)
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"untrusted"}}, key)
	if err != nil {
		t.Fatal(err)
	}

	body, err := wire.EncodeBootstrapRequest(wire.BootstrapRequest{SchemaVersion: 1, Cluster: cfg.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr, Shares: wire.DefaultShares})
	if err != nil {
		t.Fatal(err)
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	anonymous := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	var enrollment wire.BootstrapResponse

	for _, audience := range []string{"wrong-audience", wire.TokenAudience} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+cfg.ControlAddress+wire.BootstrapPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+requestToken(audience))
		response, err := anonymous.Do(req)

		want := 401
		if audience == wire.TokenAudience {
			want = 200
		}

		encoded := responseBody(t, response, err, want)
		if want == 200 {
			enrollment, err = wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
			require.NoError(t, err)
			require.Equal(t, wire.NodeID(node.UID), enrollment.Node)
		}
	}

	peerTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{{Certificate: enrollment.CertificateChain, PrivateKey: key}}}}
	t.Cleanup(peerTransport.CloseIdleConnections)

	return &http.Client{Transport: peerTransport, Timeout: 15 * time.Second}
}

func TestConcurrentStartupSingleCreate(t *testing.T) {
	for _, failure := range []string{"none", "create denied", "create response lost", "marker response lost"} {
		t.Run(failure, func(t *testing.T) { concurrentStartup(t, failure) })
	}
}

func concurrentStartup(t *testing.T, failure string) {
	t.Helper()
	r := testTopology(t)
	base := r.Client.(client.WithWatch)

	const replicas = 8

	arrived := make(chan struct{}, replicas)
	proceed := make(chan struct{})
	results := make(chan error, replicas)

	var creates, persisted, consumed atomic.Int32

	boom := errors.New(failure)
	writer := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, obj, opts...); err != nil {
				return err
			}

			consumed.Add(1)

			if failure == "marker response lost" {
				return boom
			}

			return nil
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates.Add(1)

			arrived <- struct{}{}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-proceed:
			}

			if failure == "create denied" {
				return boom
			}

			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}

			persisted.Add(1)

			if failure == "create response lost" {
				return boom
			}

			return nil
		},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	for range replicas {
		go func() { results <- Assemble(r.config, writer, base).Recover(ctx, writer) }()
	}

	for range replicas {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(proceed)
			t.Fatal("startups did not reach candidate Create")
		}
	}

	close(proceed)

	successes := successfulStartups(results, replicas)

	wantPersisted, wantConsumed, wantSuccess := int32(1), int32(1), replicas-1

	switch failure {
	case "none":
		wantSuccess = replicas
	case "create denied":
		wantPersisted, wantConsumed, wantSuccess = 0, 0, 0
	}

	require.Equal(t, wantConsumed, consumed.Load())
	require.EqualValues(t, replicas, creates.Load())
	require.Equal(t, wantPersisted, persisted.Load(), "concurrent Create attempts must persist only one candidate")
	require.Equal(t, wantSuccess, successes)
	require.NoError(t, Assemble(r.config, base, base).Recover(t.Context(), base), "fresh/candidate/committed recovery must converge")
}

func successfulStartups(results <-chan error, replicas int) int {
	successes := 0

	for range replicas {
		if <-results == nil {
			successes++
		}
	}

	return successes
}

func TestStartupRecoveryNeverWrites(t *testing.T) {
	for _, state := range []string{"valid", "missing", "corrupt", "wrong binding", "mutable marker", "read denied"} {
		t.Run(state, func(t *testing.T) {
			r := initializedTopology(t)

			cm, _, err := readVersion(t.Context(), r.APIReader, r.config)
			if err != nil {
				t.Fatal(err)
			}

			switch state {
			case "missing":
				err = r.Delete(t.Context(), cm)
			case "corrupt":
				cm.Data["sequence"] = "0"
				err = r.Update(t.Context(), cm)
			case "wrong binding":
				cm.Annotations[installationUIDAnnotation] = "foreign"
				err = r.Update(t.Context(), cm)
			case "mutable marker":
				marker, getErr := readInstallation(t.Context(), r.APIReader, r.config, false)
				if getErr != nil {
					t.Fatal(getErr)
				}

				marker.Immutable = nil
				err = r.Update(t.Context(), marker)
			}

			if err != nil {
				t.Fatal(err)
			}

			writes := 0
			c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return errors.New("unexpected Update")
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					writes++
					return errors.New("unexpected Create")
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if state == "read denied" {
						return apierrors.NewForbidden(corev1.Resource("configmaps"), key.Name, errors.New("denied"))
					}

					return c.Get(ctx, key, obj, opts...)
				},
			})

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			err = Assemble(r.config, c, c).Recover(ctx, c)
			if (err == nil) != (state == "valid") || writes != 0 {
				t.Fatalf("recovery err=%v writes=%d", err, writes)
			}
		})
	}
}

func TestStartupWaitsForWinnerGap(t *testing.T) {
	r := testTopology(t)
	base := r.Client.(client.WithWatch)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	creating := make(chan struct{})
	observedGap := make(chan struct{}, 1)
	results := make(chan error, 2)

	var creates atomic.Int32

	winner := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			close(creating)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-observedGap:
			}

			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}

			creates.Add(1)

			return nil
		},
	})

	go func() { results <- Assemble(r.config, winner, base).Recover(ctx, winner) }()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-creating:
	}

	follower := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if key.Name == r.config.VersionConfigMapName && apierrors.IsNotFound(err) {
				select {
				case observedGap <- struct{}{}:
				default:
				}
			}

			return err
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}

			creates.Add(1)

			return nil
		},
	})

	go func() { results <- Assemble(r.config, follower, follower).Recover(ctx, follower) }()

	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	if creates.Load() != 1 {
		t.Fatalf("Create attempts: %d", creates.Load())
	}
}

func TestHTTPSCertificateIndependentOfWorkloadChanges(t *testing.T) {
	for _, scenario := range []string{"node deleted", "pod recreated", "pod deleted", "pod owner revoked", "ds recreated"} {
		t.Run(scenario, func(t *testing.T) { certificateAfterWorkloadChange(t, scenario) })
	}
}

func certificateAfterWorkloadChange(t *testing.T, scenario string) {
	t.Helper()
	f := newServingFixture(t)
	endpoint := f.start(t)
	peer := f.client(t, &f.certificate)
	response, err := peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusOK)

	var obj client.Object = &corev1.Pod{}

	key := client.ObjectKey{Namespace: "racer", Name: "worker-pod"}

	switch scenario {
	case "node deleted":
		obj, key = &corev1.Node{}, client.ObjectKey{Name: "worker"}
	case "ds recreated":
		obj, key = &appsv1.DaemonSet{}, client.ObjectKey{Namespace: "racer", Name: "racer-dataplane"}
	}

	if err := f.a.Topology.Get(t.Context(), key, obj); err != nil {
		t.Fatal(err)
	}

	if scenario == "pod owner revoked" {
		obj.(*corev1.Pod).OwnerReferences[0].UID = "revoked-owner"
		if err := f.a.Topology.Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := f.a.Topology.Delete(t.Context(), obj); err != nil {
			t.Fatal(err)
		}

		if scenario == "pod recreated" || scenario == "ds recreated" {
			obj.SetUID("replacement")
			obj.SetResourceVersion("")

			if err := f.a.Topology.Create(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
		}
	}

	reused := false
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+wire.SnapshotPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err = peer.Do(req)
	responseBody(t, response, err, http.StatusOK)

	if !reused {
		t.Fatal("workload change check did not reuse TLS connection")
	}
}

func TestHTTPSSnapshotDoesNotReadKubernetes(t *testing.T) {
	f := newServingFixture(t)

	var reads atomic.Int64

	fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			reads.Add(1)
			return errors.New("unexpected Kubernetes GET")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			reads.Add(1)
			return errors.New("unexpected Kubernetes LIST")
		},
	})
	endpoint := f.start(t)
	peer := f.client(t, &f.certificate)

	for range 2 {
		response, err := peer.Get(endpoint + wire.SnapshotPath)
		responseBody(t, response, err, http.StatusOK)
	}

	if reads.Load() != 0 {
		t.Fatalf("snapshot read Kubernetes: %d", reads.Load())
	}
}

func catalogVolume(name string, uid types.UID) racerv1.ClusterVolume {
	return racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
}

// CanonicalSocketPaths keeps these tests on the production wire validation boundary.
func CanonicalSocketPaths(name string) (clientPath, originPath string, err error) {
	return wire.CanonicalSocketPaths(name)
}

func TestCanonicalSocketPaths(t *testing.T) {
	for _, name := range []string{"a", "cache-a", "cache.a", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
		client, origin, err := CanonicalSocketPaths(name)
		if err != nil || client != "/run/racer/"+name+"/client/socket" || origin != "/run/racer/"+name+"/origin/socket" || len(client) > 107 || len(origin) > 107 {
			t.Fatalf("name %q: %q, %q, %v", name, client, origin, err)
		}
	}

	for _, name := range []string{"", ".", "..", "../cache", "cache/child", "Cache", "cache_a", "a..b", "-a", "a-", "a.-b", "a.b-", "a\x00", "caché", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		client, origin, err := CanonicalSocketPaths(name)
		if !errors.Is(err, wire.InvalidRequest) || client != "" || origin != "" {
			t.Fatalf("invalid name %q: %q, %q, %v", name, client, origin, err)
		}
	}
}

func TestBuildCatalog(t *testing.T) {
	volumes := []racerv1.ClusterVolume{catalogVolume("cache-b", testOtherUID), catalogVolume("cache-a", testNodeUID), catalogVolume("cache-c", testDaemonSetUID)}

	original := make([]racerv1.ClusterVolume, len(volumes))
	for i := range volumes {
		original[i] = *volumes[i].DeepCopy()
	}

	got, err := BuildCatalog(volumes)

	want := []wire.CacheDefinition{
		{ID: testNodeUID, Name: "cache-a", ClientSocket: "/run/racer/cache-a/client/socket", OriginSocket: "/run/racer/cache-a/origin/socket"},
		{ID: testOtherUID, Name: "cache-b", ClientSocket: "/run/racer/cache-b/client/socket", OriginSocket: "/run/racer/cache-b/origin/socket"},
		{ID: wire.CacheID(testDaemonSetUID), Name: "cache-c", ClientSocket: "/run/racer/cache-c/client/socket", OriginSocket: "/run/racer/cache-c/origin/socket"},
	}
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(volumes, original) {
		t.Fatalf("catalog: %#v, %v; inputs: %#v", got, err, volumes)
	}

	slices.Reverse(volumes)

	again, err := BuildCatalog(volumes)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("order changed catalog: %#v, %v", again, err)
	}

	empty, err := BuildCatalog(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty catalog: %#v, %v", empty, err)
	}
	// A terminating object still exists; removal follows its absence from inputs.
	volume := catalogVolume("cache-a", testNodeUID)
	volume.DeletionTimestamp = &metav1.Time{}

	got, err = BuildCatalog([]racerv1.ClusterVolume{volume})
	if err != nil || len(got) != 1 {
		t.Fatalf("terminating cache: %#v, %v", got, err)
	}

	volume.UID = testOtherUID

	recreated, err := BuildCatalog([]racerv1.ClusterVolume{volume})
	if err != nil || recreated[0].ID == got[0].ID || recreated[0].ClientSocket != got[0].ClientSocket {
		t.Fatalf("recreation: %#v, %v", recreated, err)
	}
}

func TestBuildCatalogRejectsWholeInvalidCandidate(t *testing.T) {
	valid := catalogVolume("cache-a", testNodeUID)
	for name, invalid := range map[string]racerv1.ClusterVolume{
		"missing uid":    catalogVolume("cache-b", ""),
		"invalid uid":    catalogVolume("cache-b", "invalid"),
		"uppercase uid":  catalogVolume("cache-b", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"),
		"duplicate uid":  catalogVolume("cache-b", testNodeUID),
		"duplicate name": catalogVolume("cache-a", testOtherUID),
		"unsafe name":    catalogVolume("../cache", testOtherUID),
		"long path":      catalogVolume(strings.Repeat("a", 63)+"."+strings.Repeat("b", 19), testOtherUID),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BuildCatalog([]racerv1.ClusterVolume{valid, invalid})
			if !errors.Is(err, wire.InvalidRequest) || got != nil {
				t.Fatalf("partial catalog escaped: %#v, %v", got, err)
			}
		})
	}
}

// Run against the generated CRD in TestEnvtestServer, including Kubernetes'
// built-in metadata validation rather than a fake client or a CEL-only evaluator.
func integrationVolumeNameAdmission(t *testing.T, c client.Client) {
	t.Helper()

	for _, tt := range []struct {
		name        string
		volumeName  string
		wantMessage string
	}{
		{name: "single-character", volumeName: "a"},
		{name: "digits-and-hyphens", volumeName: "0.cache-1.2"},
		{name: "63-character-label", volumeName: strings.Repeat("a", 63)},
		{name: "63-character-hyphenated-label", volumeName: "0" + strings.Repeat("-", 61) + "9"},
		{name: "63-character-middle-label", volumeName: "a." + strings.Repeat("b", 63) + ".c"},
		{name: "64-total-multiple-labels", volumeName: strings.Repeat("a", 62) + ".b"},
		{name: "82-total-first-label-boundary", volumeName: strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)},
		{name: "82-total-last-label-boundary", volumeName: strings.Repeat("a", 18) + "." + strings.Repeat("b", 63)},
		{name: "82-total-many-labels", volumeName: strings.Repeat("a.", 40) + "bb"},
		{name: "64-character-label", volumeName: strings.Repeat("a", 64), wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-first-label", volumeName: strings.Repeat("a", 64) + ".b", wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-middle-label", volumeName: "a." + strings.Repeat("b", 64) + ".c", wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-last-label", volumeName: "a." + strings.Repeat("b", 64), wantMessage: "each name label must be at most 63 characters"},
		{name: "82-character-single-label", volumeName: strings.Repeat("a", 82), wantMessage: "each name label must be at most 63 characters"},
		{name: "83-total-valid-labels", volumeName: strings.Repeat("a", 63) + "." + strings.Repeat("b", 19), wantMessage: "name must fit the canonical Unix socket path"},
		{name: "83-total-many-labels", volumeName: strings.Repeat("a.", 41) + "b", wantMessage: "name must fit the canonical Unix socket path"},
		{name: "empty", volumeName: "", wantMessage: "metadata.name"},
		{name: "uppercase", volumeName: "Cache", wantMessage: "metadata.name"},
		{name: "underscore", volumeName: "cache_a", wantMessage: "metadata.name"},
		{name: "non-ASCII", volumeName: "caché", wantMessage: "metadata.name"},
		{name: "slash", volumeName: "cache/child", wantMessage: "metadata.name"},
		{name: "leading-dot", volumeName: ".cache", wantMessage: "metadata.name"},
		{name: "trailing-dot", volumeName: "cache.", wantMessage: "metadata.name"},
		{name: "empty-label", volumeName: "cache..a", wantMessage: "metadata.name"},
		{name: "leading-hyphen", volumeName: "-cache", wantMessage: "metadata.name"},
		{name: "trailing-hyphen", volumeName: "cache-", wantMessage: "metadata.name"},
		{name: "label-leading-hyphen", volumeName: "cache.-a", wantMessage: "metadata.name"},
		{name: "label-trailing-hyphen", volumeName: "cache.a-", wantMessage: "metadata.name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: tt.volumeName}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}

			err := c.Create(t.Context(), volume)
			if err == nil {
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					if err := c.Delete(ctx, volume); err != nil {
						t.Error(err)
					}
				})
			}

			if tt.wantMessage != "" {
				if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantMessage) {
					t.Fatalf("create %q: want Invalid containing %q, got %v", tt.volumeName, tt.wantMessage, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("create %q: %v", tt.volumeName, err)
			}

			catalog, err := BuildCatalog([]racerv1.ClusterVolume{*volume})
			if err != nil || len(catalog) != 1 {
				t.Fatalf("admitted cache cannot enter wire catalog: %v, %v", catalog, err)
			}
		})
	}
}

func TestStartupDeadlineCancelsAPI(t *testing.T) {
	for _, operation := range []string{"initial marker", "initial version", "update", "create", "final marker", "final version"} {
		for _, boundary := range []string{"application", "earlier caller", "caller cancellation"} {
			t.Run(operation+"/"+boundary, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) { startupDeadline(t, operation, boundary) })
			})
		}
	}
}

func startupDeadline(t *testing.T, operation, boundary string) {
	t.Helper()
	r := testTopology(t)

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	wantDuration := 30 * time.Second
	wantErr := context.DeadlineExceeded

	switch boundary {
	case "earlier caller":
		var stop context.CancelFunc

		parent, stop = context.WithTimeout(parent, time.Second)
		defer stop()

		wantDuration = time.Second
	case "caller cancellation":
		time.AfterFunc(time.Second, cancel)
		wantDuration = time.Second
		wantErr = context.Canceled
	}

	blocked := false
	block := func(ctx context.Context) error {
		blocked = true

		<-ctx.Done()

		return ctx.Err()
	}
	created := false
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			stage := "initial "
			if created {
				stage = "final "
			}

			if key.Name == r.config.InstallationConfigMapName {
				stage += "marker"
			} else {
				stage += "version"
			}

			if operation == stage {
				return block(ctx)
			}
			// Spending budget before later calls catches per-call resets.
			time.Sleep(100 * time.Millisecond)

			return c.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if operation == "update" {
				return block(ctx)
			}

			return c.Update(ctx, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if operation == "create" {
				return block(ctx)
			}

			created = true

			return c.Create(ctx, obj, opts...)
		},
	})

	start := time.Now()

	err := Assemble(r.config, c, c).Recover(parent, c)
	if !blocked || !errors.Is(err, wantErr) || time.Since(start) != wantDuration {
		t.Fatalf("blocked=%v error=%v elapsed=%v; want %v after %v", blocked, err, time.Since(start), wantErr, wantDuration)
	}

	if boundary == "application" && parent.Err() != nil {
		t.Fatalf("recovery canceled parent: %v", parent.Err())
	}
}

func TestStartupDeadlineSuccess(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "installed"}[installed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { startupDeadlineSuccess(t, installed) })
		})
	}
}

func startupDeadlineSuccess(t *testing.T, installed bool) {
	t.Helper()

	r := testTopology(t)
	if installed {
		if err := ensureInstalled(t.Context(), r.Client, r.APIReader, r.config); err != nil {
			t.Fatal(err)
		}
	}

	var recovery []context.Context

	reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if len(recovery) == 0 {
				recovery = append(recovery, ctx)

				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) != 30*time.Second {
					t.Fatalf("startup deadline=%v present=%v", deadline, ok)
				}
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})

	a := Assemble(r.config, r.Client, reader)
	if err := a.Recover(t.Context(), r.Client); err != nil {
		t.Fatal(err)
	}

	if len(recovery) == 0 || !errors.Is(recovery[0].Err(), context.Canceled) {
		t.Fatal("recovery context not released on success")
	}

	if t.Context().Err() != nil || a.Server.Ready(nil) == nil {
		t.Fatal("recovery canceled caller or granted serving authority")
	}
}

func TestStartupDeadlineAllowsCompetingInstallerWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := testTopology(t)

		c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("winner stopped before version creation")
			},
		})
		if err := ensureInstalled(t.Context(), c, c, r.config); err == nil {
			t.Fatal("expected incomplete installation")
		}

		first := true
		reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if first {
					first = false

					time.Sleep(2 * time.Second)
				}

				return c.Get(ctx, key, obj, opts...)
			},
		})
		start := time.Now()

		err := Assemble(r.config, r.Client, reader).Recover(t.Context(), r.Client)
		if err != nil || time.Since(start) != 2*time.Second {
			t.Fatalf("competing installer wait: error=%v elapsed=%v", err, time.Since(start))
		}
	})
}
