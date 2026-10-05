// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
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
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/server"
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
		require.True(t, ids.Owns(pod))

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
	require.Empty(t, ids.workloads[1].uid)
	require.Equal(t, host.UID, ids.workloads[0].uid)
}

func TestAssemble(t *testing.T) {
	// Nil Kubernetes dependencies make unintended constructor API calls fail.
	a := Assemble(Config{}, nil, nil)
	if a.Topology.authority != a.authority {
		t.Fatal("topology and HTTP must share the single publication owner")
	}

	if a.authority == nil {
		t.Fatal("bootstrap must have an issuer")
	}

	if a.Keyring.authority != a.authority || a.Topology.authority != a.authority {
		t.Fatal("controllers, issuance, and serving must share trust")
	}

	if a.Keyring.authority == nil || a.Topology.authority != a.Keyring.authority {
		t.Fatal("controllers and issuance must share the catalog gate")
	}

	if a.Server.Lifecycle != a.Lifecycle {
		t.Fatal("serving must share the process readiness gate")
	}

	if a.Server.Leader != a.Replication || a.Server.Config != a.Topology.Config.serverConfig() {
		t.Fatal("serving must use the composed replication owner and transport inputs")
	}

	for _, cfg := range []Config{a.Topology.Config, a.Keyring.Config, a.Replication.Config} {
		if cfg.CertificateLifetime != wire.CertificateLifetime || cfg.SnapshotMaxAge != 30*time.Second {
			t.Fatal("composition did not resolve default lifetimes")
		}
	}

	if a.Replication.Config.SnapshotMaxAge != a.Topology.Config.SnapshotMaxAge || a.Replication.Config.SnapshotMaxAge != a.Keyring.Config.SnapshotMaxAge {
		t.Fatal("freshness owners differ from effective configuration")
	}

	if a.authority.PublicationReady() == nil || a.Server.NeedLeaderElection() || a.Replication.NeedLeaderElection() {
		t.Fatal("missing local accepted state or process-scoped server")
	}

	if err := a.Server.Ready(nil); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("uninitialized server became ready: %v", err)
	}

	// Composition freezes observer inputs before exposing any server entry point.
	want := a.Replication.Config

	a.Replication.Config = Config{SnapshotMaxAge: time.Hour}
	if a.Replication.runtimeConfig() != want {
		t.Fatal("replication settings were not frozen before serving composition")
	}
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
			if err := operation(); !errors.Is(err, wire.InvalidRequest) {
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

type servingFixture struct {
	a                 *Application
	token             string
	key               ed25519.PrivateKey
	request           wire.BootstrapRequest
	certificate       tls.Certificate
	serverCertificate tls.Certificate
	roots             *x509.CertPool
	ctx               context.Context
	cancel            context.CancelFunc
}

func newServingFixture(t *testing.T) *servingFixture {
	t.Helper()
	a, status, token := authFixture(t)
	installReview(t, a, status, token)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runKeys(t, a.Keyring)
	reconcileTopology(t, a.Topology, ctx)
	startFixtureLifecycle(t, a.Lifecycle, ctx)

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{"attacker"}, Subject: pkix.Name{CommonName: "attacker"}}, key)
	if err != nil {
		t.Fatal(err)
	}

	request := wire.BootstrapRequest{SchemaVersion: 1, Cluster: a.Topology.Config.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr, Shares: wire.DefaultShares}
	authRequest := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	authRequest.Header.Set("Authorization", "Bearer "+token)

	identity, err := a.authority.Authenticate(ctx, authRequest)
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := a.authority.Issue(ctx, identity, request)
	if err != nil {
		t.Fatal(err)
	}

	response := decodeIssuedResponse(t, encoded)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{a.Server.Config.ReplicationServerName}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(cert)
	fixtureTLS(t, a.Server, ctx, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})

	return &servingFixture{a: a, token: token, key: key, request: request, certificate: tls.Certificate{Certificate: response.CertificateChain, PrivateKey: key}, serverCertificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots: roots, ctx: ctx, cancel: cancel}
}

func (f *servingFixture) client(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()

	config := &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13, ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	if cert != nil {
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	}

	transport := &http.Transport{TLSClientConfig: config}
	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func (f *servingFixture) start(t *testing.T) string {
	t.Helper()

	s := httptest.NewUnstartedServer(f.a.Server.Handler())
	s.TLS = fixtureTLS(t, f.a.Server, f.ctx, f.serverCertificate)
	s.StartTLS()
	t.Cleanup(s.Close)

	return s.URL
}

func responseBody(t *testing.T, response *http.Response, err error, status int) []byte {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()

	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != status {
		t.Fatalf("status %d, want %d: %s", response.StatusCode, status, b)
	}

	if status >= 400 {
		if _, err := wire.DecodeError(bytes.NewReader(b)); err != nil {
			t.Fatalf("non-protocol error %q", b)
		}

		if status == 429 || status == 503 {
			if response.Header.Get("Retry-After") != "1" {
				t.Fatal("missing retry bound")
			}
		}
	}

	return b
}

func startFixtureLifecycle(t *testing.T, l *server.Lifecycle, ctx context.Context) {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("fixture lifecycle did not stop")
		}
	})

	synced := make(chan struct{})

	l.SetCacheSync(func(context.Context) bool { close(synced); return true })

	go func() { done <- l.Start(ctx) }()

	<-synced
	l.SetServingReady(true)

	deadline := time.Now().Add(5 * time.Second)
	for l.Ready(nil) != nil {
		if ctx.Err() != nil || time.Now().After(deadline) {
			t.Fatal("fixture lifecycle did not become ready")
		}

		time.Sleep(time.Millisecond)
	}
}

func fixtureTLS(t *testing.T, s *server.Server, ctx context.Context, certificate tls.Certificate) *tls.Config {
	t.Helper()

	if s.Config.TLSCertificateFile == "/etc/racer/tls/tls.crt" {
		dir := t.TempDir()
		s.Config.TLSCertificateFile = filepath.Join(dir, "tls.crt")
		s.Config.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")

		var chain []byte
		for _, der := range certificate.Certificate {
			chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		}

		key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(s.Config.TLSCertificateFile, chain, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(s.Config.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	config, err := s.TLSConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}

	return config
}

type fixtureRequestWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w fixtureRequestWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := w.writer.Write(b)
	if err == nil {
		err = w.ctx.Err()
	}

	return n, err
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
	t.Run("cache-name-admission", func(t *testing.T) { integrationCacheNameAdmission(t, c) })
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
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: cfg.InstallationConfigMapName}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh"}},
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
		r := Assemble(concurrent.Topology.Config, interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
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

	a := integrationInstallation(t, c, "init-cas")

	r := a.Topology
	if err := a.Recover(ctx, r.Client); err != nil {
		t.Fatal(err)
	}

	marker, err := readInstallation(ctx, r.APIReader, r.Config, false)
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

	cm, _, err := readVersion(ctx, r.APIReader, r.Config)
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

	owner := authority.New(r.Config.authorityConfig(), authority.Dependencies{Reader: c, Writer: writer})
	if _, err := owner.PublishTopology(ctx, r.observeTopology); !apierrors.IsConflict(err) {
		t.Fatalf("real CAS failed: err=%v", err)
	}

	r.Client = c

	cm, _, err = readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	leader, cancel := context.WithCancel(ctx)

	owner = authority.New(r.Config.authorityConfig(), authority.Dependencies{Reader: c, Writer: interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
		err := c.Update(ctx, obj, opts...)

		cancel()

		return err
	}}})
	if _, err := owner.PublishTopology(leader, r.observeTopology); !errors.Is(err, context.Canceled) {
		t.Fatalf("late install: %v", err)
	}

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

		restarted := Assemble(a.Topology.Config, c, c)
		if err := restarted.Recover(ctx, c); (err == nil) != afterCreate {
			t.Fatalf("ambiguous initialization recovery: %v", err)
		}

		if _, _, err := readVersion(ctx, restarted.Topology.APIReader, restarted.Topology.Config); (err == nil) != afterCreate {
			t.Fatalf("crash recovery afterCreate=%t: %v", afterCreate, err)
		}
	}
}

func integrationRotation(t *testing.T, c client.Client) {
	a := integrationInstallation(t, c, "rotation")
	cfg := a.Keyring.Config
	cfg.Rotation.Interval = 7 * 24 * time.Hour

	a = assembleFixture(cfg, c, c)
	if err := a.Recover(t.Context(), a.Topology.Client); err != nil {
		t.Fatal(err)
	}

	cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "rotation-cache"}}
	if err := c.Create(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := c.Delete(context.Background(), cache); err != nil {
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
	}{{"stage-private", r.Config.CredentialsSecretName, true}, {"activate-bundle", r.Config.CredentialsSecretName, true}, {"prune-private", r.Config.CredentialsSecretName, false}} {
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
		if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !failed || !errors.Is(err, boom) || r.authority.TrustReady() == nil {
			t.Fatalf("%s interruption: %v", step.name, err)
		}

		_, before, beforeState, private := keyState(t, r)
		recoveredApp := assembleFixture(r.Config, c, c)
		recovered := recoveredApp.Keyring
		fixtureDependencies[recovered.authority].now = func() time.Time { return now }
		runKeys(t, recovered)
		_, after, next, material := keyState(t, recovered)

		switch step.name {
		case "stage-private":
			if before.Generation != initial.Generation+1 || len(private.Keys) != 2 || next.PreparedIssuer != beforeState.PreparedIssuer || len(after.CacheKeys) != 4 {
				t.Fatal("staging recovery replaced committed credentials or lost keys")
			}

			now = next.ActivateAt
		case "activate-bundle":
			if after.Generation != before.Generation || next.ActiveIssuer != beforeState.ActiveIssuer || len(next.Retiring) != 1 || len(after.CacheKeys) != 2 {
				t.Fatal("activation recovery reset committed generation/deadlines")
			}

			now = next.Retiring[oldIssuer]
		case "prune-private":
			if !containsRoot(before, oldIssuer) || len(private.Keys) != 2 || len(material.Keys) != 1 || after.Generation != before.Generation+1 || len(after.CacheKeys) != 2 || containsRoot(after, oldIssuer) {
				t.Fatal("atomic root/private pruning recovery")
			}
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

func unusedAddress(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return address
}

func integrationTLS(t *testing.T, cfg *Config) *x509.CertPool {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{cfg.ReplicationServerName}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.3")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}

	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}

	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	cfg.TLSCertificateFile, cfg.TLSPrivateKeyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, block := range map[string]*pem.Block{cfg.TLSCertificateFile: {Type: "CERTIFICATE", Bytes: der}, cfg.TLSPrivateKeyFile: {Type: "PRIVATE KEY", Bytes: private}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	return roots
}

func integrationManagers(t *testing.T, rc *rest.Config, scheme *runtime.Scheme, c client.Client) {
	a := integrationInstallation(t, c, "managers")
	if err := a.Recover(t.Context(), a.Topology.Client); err != nil {
		t.Fatal(err)
	}

	cfg := a.Topology.Config
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

		token, err := kube.CoreV1().ServiceAccounts(cfg.Namespace).CreateToken(t.Context(), sa.Name, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{ReplicationAudience}, ExpirationSeconds: ptr.To(int64(3600)), BoundObjectRef: &authv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		cfg.ReplicationTokenFile = filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(cfg.ReplicationTokenFile, []byte(token.Status.Token), 0o600); err != nil {
			t.Fatal(err)
		}

		options := managerOptions(cfg, scheme)
		options.LeaseDuration, options.RenewDeadline, options.RetryPeriod = ptr.To(4*time.Second), ptr.To(2*time.Second), ptr.To(500*time.Millisecond)
		options.Controller.SkipNameValidation = ptr.To(true) // Two real managers in one test process.
		connection := rest.CopyConfig(rc)
		connection.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
			return transportFunc(func(req *http.Request) (*http.Response, error) {
				if denyRenewal[i].Load() && req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/leases/") {
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

			if _, ok := obj.(*corev1.ConfigMap); ok && obj.GetName() == apps[i].Topology.Config.VersionConfigMapName {
				topologyCommitted[i].Store(true)
			}

			return nil
		}}

		apps[i] = Assemble(cfg, writer, mgr.GetAPIReader())
		if err := apps[i].SetupWithManager(mgr); err != nil {
			t.Fatal(err)
		}

		var ctx context.Context

		ctx, cancels[i] = context.WithCancel(t.Context())

		done[i] = make(chan error, 1)

		go func() { done[i] <- mgr.Start(ctx) }()

		t.Cleanup(func() {
			cancels[i]()

			select {
			case <-done[i]:
			case <-time.After(15 * time.Second):
				t.Error("manager failed to stop")
			}
		})
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

	for i, app := range apps {
		response, err := http.Get("http://" + app.Topology.Config.ProbeAddress + "/readyz")

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

	peer := integrationEnrollment(t, rc, c, apps[follower], ds, roots)
	endpoint := "https://" + apps[leader].Server.Config.ControlAddress
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

	followerResponse, followerErr := peer.Get("https://" + apps[follower].Server.Config.ControlAddress + wire.SnapshotPath)
	responseBody(t, followerResponse, followerErr, http.StatusOK)

	pollDone := make(chan error, 1)

	go func() {
		pollResponse, err := peer.Get(fmt.Sprintf("%s%s?after=%d", endpoint, wire.SnapshotPath, publication.Sequence))
		if pollResponse != nil {
			pollResponse.Body.Close()

			if pollResponse.StatusCode == http.StatusServiceUnavailable {
				err = wire.Unavailable // Cancellation may send a bounded error before TCP closes.
			}
		}

		pollDone <- err
	}()

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

	denyRenewal[leader].Store(true)
	eventually(t, "Lease renewal failure withdraws readiness", func() bool { return apps[leader].Server.Ready(nil) != nil })

	select {
	case err := <-pollDone:
		if err == nil {
			t.Fatal("old leader poll completed instead of closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old leader HTTPS poll survived cancellation")
	}

	select {
	case err := <-done[leader]:
		if err == nil || !strings.Contains(err.Error(), "leader election lost") {
			t.Fatalf("manager loss result: %v", err)
		}

		done[leader] <- err // Cleanup still joins this manager.
	case <-time.After(5 * time.Second):
		t.Fatal("lost leader did not stop")
	}

	if conn, err := net.DialTimeout("tcp", apps[leader].Server.Config.ControlAddress, time.Second); err == nil {
		conn.Close()
		t.Fatal("old leader listener still accepts after manager exit")
	}

	eventually(t, "follower takes expired Lease, commits topology, and serves", func() bool {
		return apps[follower].Replication.isLeader() && topologyCommitted[follower].Load() && apps[follower].Server.Ready(nil) == nil
	})

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(lease), lease); err != nil || *lease.Spec.HolderIdentity == oldHolder {
		t.Fatalf("Lease did not change holder: %v", err)
	}

	_, committedVersion, err := readVersion(t.Context(), c, apps[follower].Topology.Config)
	if err != nil || committedVersion.Sequence != publication.Sequence || committedVersion.MembershipVersion != publication.MembershipVersion {
		t.Fatalf("new leader durable commit changed counters: %+v, %v", committedVersion, err)
	}

	response, err = peer.Get("https://" + apps[follower].Server.Config.ControlAddress + wire.SnapshotPath)

	recovered, err := wire.DecodePublication(bytes.NewReader(responseBody(t, response, err, 200)))
	if err != nil || recovered.Sequence != publication.Sequence || recovered.MembershipVersion != publication.MembershipVersion {
		t.Fatalf("failover changed unchanged counters: before=%+v after=%+v error=%v", publication, recovered, err)
	}

	t.Logf("actual Lease failover and authenticated HTTPS recovery: %s; sequence=%d membership=%d", time.Since(start), recovered.Sequence, recovered.MembershipVersion)
	integrationAuthorizationLoad(t, rc, c, apps[follower], peer)

	if err := apps[follower].Server.Ready(nil); err != nil {
		t.Fatalf("new leader not ready before normal cancellation: %v", err)
	}

	cancels[follower]()
	eventually(t, "manager cancellation withdraws readiness", func() bool { return apps[follower].Server.Ready(nil) != nil })

	select {
	case err := <-done[follower]:
		if err != nil {
			t.Fatalf("normal manager cancellation: %v", err)
		}

		done[follower] <- err
	case <-time.After(5 * time.Second):
		t.Fatal("canceled manager did not stop")
	}

	if _, err := apps[follower].authority.Current(); !errors.Is(err, context.Canceled) {
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

	measuredApp := Assemble(a.Topology.Config, reader, reader)

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

	if requests.Load() == 0 || received.Load() == 0 {
		t.Fatal("instrumented authority positive control did not read API")
	}

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
			response, err = peer.Get(endpoint)
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

	response, err = peer.Get(endpoint)
	responseBody(t, response, err, 200)
}

func integrationEnrollment(t *testing.T, rc *rest.Config, c client.Client, a *Application, ds *appsv1.DaemonSet, roots *x509.CertPool) *http.Client {
	t.Helper()

	cfg := a.Topology.Config
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "server-node"}}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.DataplaneServiceAccount}}
	for _, obj := range []client.Object{node, sa} {
		if err := c.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
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
		token, err := kube.CoreV1().ServiceAccounts(cfg.Namespace).CreateToken(t.Context(), sa.Name, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: ptr.To(int64(3600)), BoundObjectRef: &authv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}

		return token.Status.Token
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
			if err != nil || enrollment.Node != wire.NodeID(node.UID) {
				t.Fatalf("live TokenReview enrollment: %v", err)
			}
		}
	}

	peerTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{{Certificate: enrollment.CertificateChain, PrivateKey: key}}}}
	t.Cleanup(peerTransport.CloseIdleConnections)

	return &http.Client{Transport: peerTransport, Timeout: 15 * time.Second}
}

func TestConcurrentStartupSingleCreate(t *testing.T) {
	for _, failure := range []string{"none", "create denied", "create response lost", "marker response lost"} {
		t.Run(failure, func(t *testing.T) {
			r := testTopology(t)
			base := r.Client.(client.WithWatch)

			const replicas = 8

			arrived := make(chan struct{}, replicas)
			proceed := make(chan struct{})
			results := make(chan error, replicas)

			var creates, consumed atomic.Int32

			boom := errors.New(failure)
			writer := interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					arrived <- struct{}{}

					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-proceed:
					}

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

					if failure == "create denied" {
						return boom
					}

					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}

					if failure == "create response lost" {
						return boom
					}

					return nil
				},
			})

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			for range replicas {
				go func() { results <- Assemble(r.Config, writer, base).Recover(ctx, writer) }()
			}

			for range replicas {
				select {
				case <-arrived:
				case <-ctx.Done():
					close(proceed)
					t.Fatal("startups did not reach CAS")
				}
			}

			close(proceed)

			successes := 0

			for range replicas {
				if err := <-results; err == nil {
					successes++
				}
			}

			wantCreates, wantSuccess := int32(1), 0

			switch failure {
			case "none":
				wantSuccess = replicas
			case "create response lost":
				wantSuccess = replicas - 1
			case "marker response lost":
				wantCreates = 0
			}

			if consumed.Load() != 1 || creates.Load() != wantCreates || successes != wantSuccess {
				t.Fatalf("consumed=%d creates=%d successes=%d", consumed.Load(), creates.Load(), successes)
			}
		})
	}
}

func TestStartupRecoveryNeverWrites(t *testing.T) {
	for _, state := range []string{"valid", "missing", "corrupt", "wrong binding", "mutable marker", "read denied"} {
		t.Run(state, func(t *testing.T) {
			r := initializedTopology(t)

			cm, _, err := readVersion(t.Context(), r.APIReader, r.Config)
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
				marker, getErr := readInstallation(t.Context(), r.APIReader, r.Config, false)
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

			err = Assemble(r.Config, c, c).Recover(ctx, c)
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
			creates.Add(1)
			close(creating)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-observedGap:
			}

			return c.Create(ctx, obj, opts...)
		},
	})

	go func() { results <- Assemble(r.Config, winner, base).Recover(ctx, winner) }()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-creating:
	}

	follower := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if key.Name == r.Config.VersionConfigMapName && apierrors.IsNotFound(err) {
				select {
				case observedGap <- struct{}{}:
				default:
				}
			}

			return err
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			creates.Add(1)
			return errors.New("follower must not create")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return errors.New("follower must not update")
		},
	})

	go func() { results <- Assemble(r.Config, follower, follower).Recover(ctx, follower) }()

	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	if creates.Load() != 1 {
		t.Fatalf("Create attempts: %d", creates.Load())
	}
}

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
	a.Lifecycle = server.NewLifecycle(owner)
	a.Server = server.New(cfg.serverConfig(), d, owner, a.Lifecycle, a.Replication)
	a.Replication.authority = owner
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
		n, err := fixtureRequestWriter{ctx: ctx, writer: w}.Write([]byte(rest[:min(len(rest), 32768)]))

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

func TestHTTPSCertificateIndependentOfWorkloadChanges(t *testing.T) {
	for _, scenario := range []string{"node deleted", "pod recreated", "pod deleted", "pod owner revoked", "ds recreated"} {
		t.Run(scenario, func(t *testing.T) {
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
		})
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

func catalogCache(name string, uid types.UID) racerv1.ClusterCache {
	return racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
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
	caches := []racerv1.ClusterCache{catalogCache("cache-b", testOtherUID), catalogCache("cache-a", testNodeUID), catalogCache("cache-c", testDaemonSetUID)}

	original := make([]racerv1.ClusterCache, len(caches))
	for i := range caches {
		original[i] = *caches[i].DeepCopy()
	}

	got, err := BuildCatalog(caches)

	want := []wire.CacheDefinition{
		{ID: testNodeUID, Name: "cache-a", ClientSocket: "/run/racer/cache-a/client/socket", OriginSocket: "/run/racer/cache-a/origin/socket"},
		{ID: testOtherUID, Name: "cache-b", ClientSocket: "/run/racer/cache-b/client/socket", OriginSocket: "/run/racer/cache-b/origin/socket"},
		{ID: wire.CacheID(testDaemonSetUID), Name: "cache-c", ClientSocket: "/run/racer/cache-c/client/socket", OriginSocket: "/run/racer/cache-c/origin/socket"},
	}
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(caches, original) {
		t.Fatalf("catalog: %#v, %v; inputs: %#v", got, err, caches)
	}

	slices.Reverse(caches)

	again, err := BuildCatalog(caches)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("order changed catalog: %#v, %v", again, err)
	}

	empty, err := BuildCatalog(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty catalog: %#v, %v", empty, err)
	}
	// A terminating object still exists; removal follows its absence from inputs.
	cache := catalogCache("cache-a", testNodeUID)
	cache.DeletionTimestamp = &metav1.Time{}

	got, err = BuildCatalog([]racerv1.ClusterCache{cache})
	if err != nil || len(got) != 1 {
		t.Fatalf("terminating cache: %#v, %v", got, err)
	}

	cache.UID = testOtherUID

	recreated, err := BuildCatalog([]racerv1.ClusterCache{cache})
	if err != nil || recreated[0].ID == got[0].ID || recreated[0].ClientSocket != got[0].ClientSocket {
		t.Fatalf("recreation: %#v, %v", recreated, err)
	}
}

func TestBuildCatalogRejectsWholeInvalidCandidate(t *testing.T) {
	valid := catalogCache("cache-a", testNodeUID)
	for name, invalid := range map[string]racerv1.ClusterCache{
		"missing uid":    catalogCache("cache-b", ""),
		"invalid uid":    catalogCache("cache-b", "invalid"),
		"uppercase uid":  catalogCache("cache-b", "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"),
		"duplicate uid":  catalogCache("cache-b", testNodeUID),
		"duplicate name": catalogCache("cache-a", testOtherUID),
		"unsafe name":    catalogCache("../cache", testOtherUID),
		"long path":      catalogCache(strings.Repeat("a", 63)+"."+strings.Repeat("b", 19), testOtherUID),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BuildCatalog([]racerv1.ClusterCache{valid, invalid})
			if !errors.Is(err, wire.InvalidRequest) || got != nil {
				t.Fatalf("partial catalog escaped: %#v, %v", got, err)
			}
		})
	}
}

// Run against the generated CRD in TestEnvtestServer, including Kubernetes'
// built-in metadata validation rather than a fake client or a CEL-only evaluator.
func integrationCacheNameAdmission(t *testing.T, c client.Client) {
	t.Helper()

	for _, tt := range []struct {
		name        string
		cacheName   string
		wantMessage string
	}{
		{name: "single-character", cacheName: "a"},
		{name: "digits-and-hyphens", cacheName: "0.cache-1.2"},
		{name: "63-character-label", cacheName: strings.Repeat("a", 63)},
		{name: "63-character-hyphenated-label", cacheName: "0" + strings.Repeat("-", 61) + "9"},
		{name: "63-character-middle-label", cacheName: "a." + strings.Repeat("b", 63) + ".c"},
		{name: "64-total-multiple-labels", cacheName: strings.Repeat("a", 62) + ".b"},
		{name: "82-total-first-label-boundary", cacheName: strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)},
		{name: "82-total-last-label-boundary", cacheName: strings.Repeat("a", 18) + "." + strings.Repeat("b", 63)},
		{name: "82-total-many-labels", cacheName: strings.Repeat("a.", 40) + "bb"},
		{name: "64-character-label", cacheName: strings.Repeat("a", 64), wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-first-label", cacheName: strings.Repeat("a", 64) + ".b", wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-middle-label", cacheName: "a." + strings.Repeat("b", 64) + ".c", wantMessage: "each name label must be at most 63 characters"},
		{name: "64-character-last-label", cacheName: "a." + strings.Repeat("b", 64), wantMessage: "each name label must be at most 63 characters"},
		{name: "82-character-single-label", cacheName: strings.Repeat("a", 82), wantMessage: "each name label must be at most 63 characters"},
		{name: "83-total-valid-labels", cacheName: strings.Repeat("a", 63) + "." + strings.Repeat("b", 19), wantMessage: "name must fit the canonical Unix socket path"},
		{name: "83-total-many-labels", cacheName: strings.Repeat("a.", 41) + "b", wantMessage: "name must fit the canonical Unix socket path"},
		{name: "empty", cacheName: "", wantMessage: "metadata.name"},
		{name: "uppercase", cacheName: "Cache", wantMessage: "metadata.name"},
		{name: "underscore", cacheName: "cache_a", wantMessage: "metadata.name"},
		{name: "non-ASCII", cacheName: "caché", wantMessage: "metadata.name"},
		{name: "slash", cacheName: "cache/child", wantMessage: "metadata.name"},
		{name: "leading-dot", cacheName: ".cache", wantMessage: "metadata.name"},
		{name: "trailing-dot", cacheName: "cache.", wantMessage: "metadata.name"},
		{name: "empty-label", cacheName: "cache..a", wantMessage: "metadata.name"},
		{name: "leading-hyphen", cacheName: "-cache", wantMessage: "metadata.name"},
		{name: "trailing-hyphen", cacheName: "cache-", wantMessage: "metadata.name"},
		{name: "label-leading-hyphen", cacheName: "cache.-a", wantMessage: "metadata.name"},
		{name: "label-trailing-hyphen", cacheName: "cache.a-", wantMessage: "metadata.name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: tt.cacheName}}

			err := c.Create(t.Context(), cache)
			if err == nil {
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()

					if err := c.Delete(ctx, cache); err != nil {
						t.Error(err)
					}
				})
			}

			if tt.wantMessage != "" {
				if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantMessage) {
					t.Fatalf("create %q: want Invalid containing %q, got %v", tt.cacheName, tt.wantMessage, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("create %q: %v", tt.cacheName, err)
			}

			catalog, err := BuildCatalog([]racerv1.ClusterCache{*cache})
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
				synctest.Test(t, func(t *testing.T) {
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

							if key.Name == r.Config.InstallationConfigMapName {
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

					err := Assemble(r.Config, c, c).Recover(parent, c)
					if !blocked || !errors.Is(err, wantErr) || time.Since(start) != wantDuration {
						t.Fatalf("blocked=%v error=%v elapsed=%v; want %v after %v", blocked, err, time.Since(start), wantErr, wantDuration)
					}

					if boundary == "application" && parent.Err() != nil {
						t.Fatalf("recovery canceled parent: %v", parent.Err())
					}
				})
			})
		}
	}
}

func TestStartupDeadlineSuccess(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "installed"}[installed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := testTopology(t)
				if installed {
					if err := ensureInstalled(t.Context(), r.Client, r.APIReader, r.Config); err != nil {
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

				a := Assemble(r.Config, r.Client, reader)
				if err := a.Recover(t.Context(), r.Client); err != nil {
					t.Fatal(err)
				}

				if len(recovery) == 0 || !errors.Is(recovery[0].Err(), context.Canceled) {
					t.Fatal("recovery context not released on success")
				}

				if t.Context().Err() != nil || a.Server.Ready(nil) == nil {
					t.Fatal("recovery canceled caller or granted serving authority")
				}
			})
		})
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
		if err := ensureInstalled(t.Context(), c, c, r.Config); err == nil {
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

		err := Assemble(r.Config, r.Client, reader).Recover(t.Context(), r.Client)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 7*time.Second {
			t.Fatalf("competing installer wait: error=%v elapsed=%v", err, time.Since(start))
		}
	})
}

func TestFrozenConfigUsedByRuntimeOperations(t *testing.T) {
	f := newServingFixture(t)
	a := f.a
	request := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	request.Header.Set("Authorization", "Bearer "+f.token)

	identity, err := a.authority.Authenticate(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	a.Replication.observe(f.ctx)
	handler := a.Server.Handler()
	// All components have crossed real operational boundaries, not just getters.
	for _, input := range []*Config{&a.Topology.Config, &a.Keyring.Config, &a.Replication.Config} {
		*input = Config{}
	}

	a.Server.Config = server.Config{}

	runKeys(t, a.Keyring)
	reconcileTopology(t, a.Topology, f.ctx)
	a.Replication.observe(f.ctx)

	if _, err := a.authority.Authenticate(f.ctx, request); err != nil {
		t.Fatal("bootstrap reread config", err)
	}

	encoded, err := a.authority.Issue(f.ctx, identity, f.request)
	if err != nil {
		t.Fatal("issuer reread config", err)
	}

	if response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded)); err != nil || response.Cluster != f.request.Cluster {
		t.Fatal("issued identity changed", err)
	}

	request.TLS = f.requestState(t)
	request.Header.Del("Authorization")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)

	if w.Code != http.StatusOK {
		t.Fatal("serving/replication reread config", w.Code)
	}
}

func TestComponentConfigFreezesDefaultsAtFirstUse(t *testing.T) {
	for _, name := range []string{"topology", "keyring", "replication", "bootstrap", "issuer", "server"} {
		t.Run(name, func(t *testing.T) {
			// Authority policy now belongs to root construction; the server only
			// freezes transport settings, exercised through its public handler.
			if name == "bootstrap" || name == "issuer" || name == "server" {
				cfg := testConfig(t)
				cfg.CertificateLifetime, cfg.SnapshotMaxAge = 0, 0

				cfg.PeerPort = 9443
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}

				a := Assemble(cfg, nil, nil)
				if a.Topology.Config != cfg.effective() || a.Server.Config != cfg.serverConfig() {
					t.Fatal("construction ignored pre-use inputs/defaults")
				}

				handler := a.Server.Handler()
				a.Server.Config = server.Config{}

				var readers sync.WaitGroup
				for range 8 {
					readers.Go(func() {
						r := httptest.NewRequest(http.MethodPost, wire.BootstrapPath, nil)
						r.Header.Set("X-Large", strings.Repeat("x", cfg.Limits.HeaderBytes))

						w := httptest.NewRecorder()
						handler.ServeHTTP(w, r)

						if w.Code != http.StatusRequestEntityTooLarge {
							t.Error("runtime reread mutated construction inputs")
						}
					})
				}

				readers.Wait()

				return
			}

			a := Assemble(Config{}, nil, nil)
			a.Replication = &Replication{}
			component := map[string]struct {
				input *Config
				get   func() Config
			}{
				"topology":    {&a.Topology.Config, a.Topology.runtimeConfig},
				"keyring":     {&a.Keyring.Config, a.Keyring.runtimeConfig},
				"replication": {&a.Replication.Config, a.Replication.runtimeConfig},
			}[name]
			*component.input = testConfig(t)
			component.input.CertificateLifetime = 0
			component.input.SnapshotMaxAge = 0
			component.input.PeerPort = 9443

			want := component.input.effective()
			if err := component.input.Validate(); err != nil {
				t.Fatal("zero optional lifetimes rejected", err)
			}

			if got := component.get(); got != want || got.CertificateLifetime != wire.CertificateLifetime || got.SnapshotMaxAge != 30*time.Second {
				t.Fatal("first use ignored pre-use inputs/defaults")
			}

			*component.input = Config{}

			var readers sync.WaitGroup
			for range 8 {
				readers.Go(func() {
					if component.get() != want {
						t.Error("runtime reread mutated construction inputs")
					}
				})
			}

			readers.Wait()
		})
	}
}

func TestDirectComponentConfigDefaults(t *testing.T) {
	for name, get := range map[string]func() Config{
		"topology":    (&TopologyReconciler{}).runtimeConfig,
		"keyring":     (&KeyringReconciler{}).runtimeConfig,
		"bootstrap":   func() Config { return Assemble(Config{}, nil, nil).Topology.Config },
		"issuer":      func() Config { return Assemble(Config{}, nil, nil).Keyring.Config },
		"replication": (&Replication{}).runtimeConfig,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := get()
			if cfg.CertificateLifetime != wire.CertificateLifetime || cfg.SnapshotMaxAge != 30*time.Second {
				t.Fatal("direct zero-default semantics lost")
			}
		})
	}
}

func TestConfigDeploymentIdentityAndBounds(t *testing.T) {
	cfg := testConfig(t)
	for name, mutate := range map[string]func(*Config){
		"cluster":                    func(c *Config) { c.Cluster = "" },
		"namespace":                  func(c *Config) { c.Namespace = "../namespace" },
		"missing marker name":        func(c *Config) { c.InstallationConfigMapName = "" },
		"aliased durable objects":    func(c *Config) { c.InstallationConfigMapName = c.VersionConfigMapName },
		"aliased credential secrets": func(c *Config) { c.CredentialsSecretName = "" },
		"no preparation":             func(c *Config) { c.Rotation.PrepareFor = 0 },
		"short overlap":              func(c *Config) { c.Rotation.RetainFor = wire.CertificateLifetime - 1 },
		"short interval":             func(c *Config) { c.Rotation.Interval = c.Rotation.PrepareFor - 1 },
		"zero port":                  func(c *Config) { c.PeerPort = 0 },
		"unbounded polls":            func(c *Config) { c.Limits.MaxPolls = 0 },
		"unbounded writes":           func(c *Config) { c.Limits.MaxConcurrentWrites = 0 },
		"unbounded bootstrap":        func(c *Config) { c.Limits.MaxConcurrentBootstrap = 0 },
		"unbounded headers":          func(c *Config) { c.Limits.HeaderBytes = 0 },
		"unbounded write duration":   func(c *Config) { c.Limits.WriteTimeout = 0 },
		"unbounded shutdown":         func(c *Config) { c.Limits.ShutdownTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			mutate(&invalid)

			if err := invalid.Validate(); !errors.Is(err, wire.InvalidRequest) {
				t.Fatalf("invalid config accepted: %v", err)
			}
		})
	}

	for _, port := range []string{"0", "65536", "-1", "invalid"} {
		t.Setenv("RACER_PEER_PORT", port)

		if _, err := LoadConfig(); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("port %q: %v", port, err)
		}
	}

	t.Setenv("RACER_PEER_PORT", "65535")
	t.Setenv("RACER_INSTALLATION_CONFIGMAP_NAME", "permanent-installation")
	t.Setenv("RACER_CREDENTIALS_SECRET_NAME", "custom-credentials")

	loaded, err := LoadConfig()
	if err != nil || loaded.PeerPort != 65535 || loaded.InstallationConfigMapName != "permanent-installation" || loaded.CredentialsSecretName != "custom-credentials" {
		t.Fatalf("deployment overrides: %+v, %v", loaded, err)
	}
}

func TestRuntimeConfigDoesNotReadWorkloadOnlySettings(t *testing.T) {
	_, err := ConfigFromLookup(func(key string) (string, bool) {
		switch key {
		case "RACER_CLUSTER_ID":
			return "11111111-1111-1111-1111-111111111111", true
		case "RACER_CONTROL_URL", "RACER_DATAPLANE_IMAGE", "RACER_BOOTSTRAP_TRUST_CONFIGMAP":
			t.Errorf("runtime requested workload-only setting %s", key)
			return "invalid", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigShortRotationDurations(t *testing.T) {
	testConfig(t)

	cfg, err := LoadConfig()
	if err != nil || cfg.CertificateLifetime != wire.CertificateLifetime {
		t.Fatalf("default lifetime: %v", err)
	}

	for name, value := range map[string]string{
		"RACER_CERTIFICATE_LIFETIME": "2m",
		"RACER_ROTATION_INTERVAL":    "5m",
		"RACER_ROTATION_PREPARE_FOR": "20s",
		"RACER_ROTATION_RETAIN_FOR":  "2m",
	} {
		t.Setenv(name, value)
	}

	cfg, err = LoadConfig()
	if err != nil || cfg.CertificateLifetime != 2*time.Minute || cfg.Rotation != (RotationPolicy{Interval: 5 * time.Minute, PrepareFor: 20 * time.Second, RetainFor: 2 * time.Minute}) {
		t.Fatalf("short rotation config: %v", err)
	}

	for name, value := range map[string]string{
		"RACER_CERTIFICATE_LIFETIME": "119s",
		"RACER_ROTATION_INTERVAL":    "19s",
		"RACER_ROTATION_PREPARE_FOR": "500ms",
		"RACER_ROTATION_RETAIN_FOR":  "119s",
	} {
		t.Run(name, func(t *testing.T) {
			for _, invalid := range []string{value, "", "nonsense", "0", "-1s", "8761h", "120.5s"} {
				t.Setenv(name, invalid)

				if _, err := LoadConfig(); !errors.Is(err, wire.InvalidRequest) {
					t.Fatalf("%s=%q accepted: %v", name, invalid, err)
				}
			}
		})
	}
}

func TestConfigDurationsUseProvidedLookup(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":           string(testConfig(t).Cluster),
		"RACER_CERTIFICATE_LIFETIME": "2m",
		"RACER_ROTATION_INTERVAL":    "5m",
		"RACER_ROTATION_PREPARE_FOR": "1m",
		"RACER_ROTATION_RETAIN_FOR":  "2m",
	}
	for name := range values {
		t.Setenv(name, "invalid-process-value")
	}

	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := ConfigFromLookup(lookup)
	if err != nil || cfg.CertificateLifetime != 2*time.Minute || cfg.Rotation != (RotationPolicy{Interval: 5 * time.Minute, PrepareFor: time.Minute, RetainFor: 2 * time.Minute}) {
		t.Fatalf("custom lookup ignored: %v", err)
	}

	for _, name := range []string{"RACER_CERTIFICATE_LIFETIME", "RACER_ROTATION_INTERVAL", "RACER_ROTATION_PREPARE_FOR", "RACER_ROTATION_RETAIN_FOR"} {
		previous := values[name]

		values[name] = "invalid-lookup-value"
		if _, err := ConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("invalid custom %s accepted: %v", name, err)
		}

		values[name] = previous
	}

	delete(values, "RACER_CERTIFICATE_LIFETIME")
	delete(values, "RACER_ROTATION_RETAIN_FOR")

	cfg, err = ConfigFromLookup(lookup)
	if err != nil || cfg.CertificateLifetime != wire.CertificateLifetime || cfg.Rotation.RetainFor != 48*time.Hour {
		t.Fatalf("absent custom values did not use defaults: %v", err)
	}
}

func TestReplicationConfigDefaultsAndOverrides(t *testing.T) {
	values := map[string]string{"RACER_CLUSTER_ID": string(testConfig(t).Cluster), "POD_NAMESPACE": "controllers"}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SnapshotMaxAge != 30*time.Second || cfg.ReplicationPort != 8443 || cfg.ReplicationServerName != "racer-controller.controllers.svc" || cfg.ReplicationTokenFile != "/var/run/secrets/racer-controller/token" || cfg.ReplicationTrustFile != "/etc/racer/tls/ca.crt" || cfg.ControllerServiceAccount != "racer-controller" {
		t.Fatalf("replication defaults: %+v", cfg)
	}

	values["RACER_SNAPSHOT_MAX_AGE"] = "45s"
	values["RACER_REPLICATION_PORT"] = "9443"
	values["POD_NAME"] = "controller-0"
	values["POD_UID"] = "pod-uid"

	cfg, err = ConfigFromLookup(lookup)
	if err != nil || cfg.SnapshotMaxAge != 45*time.Second || cfg.ReplicationPort != 9443 || cfg.PodName != "controller-0" || cfg.PodUID != "pod-uid" {
		t.Fatal("replication overrides", err)
	}

	for name, invalid := range map[string][]string{"RACER_REPLICATION_PORT": {"0", "65536", "-1", "bad"}, "RACER_SNAPSHOT_MAX_AGE": {"0s", "-1s", "500ms", "bad"}} {
		previous := values[name]
		for _, value := range invalid {
			values[name] = value
			if _, err := ConfigFromLookup(lookup); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		}

		values[name] = previous
	}
}

func TestCredentialsCacheSelector(t *testing.T) {
	for _, name := range []string{"racer-credentials", "custom-credentials"} {
		t.Run(name, func(t *testing.T) {
			options := managerOptions(Config{Namespace: "custom-system", CredentialsSecretName: name}, runtime.NewScheme())
			for obj, config := range options.Cache.ByObject {
				if _, ok := obj.(*corev1.Secret); !ok {
					continue
				}

				require.Len(t, config.Namespaces, 1)
				require.Contains(t, config.Namespaces, "custom-system")
				require.NotNil(t, config.Field)
				require.Equal(t, "metadata.name="+name, config.Field.String())
				require.True(t, config.Field.Matches(fields.Set{"metadata.name": name}))

				for _, excluded := range []string{"racer-controller-tls", "unrelated", ""} {
					require.False(t, config.Field.Matches(fields.Set{"metadata.name": excluded}))
				}

				return
			}

			t.Fatal("Secret cache configuration missing")
		})
	}
}

func (f *servingFixture) requestState(t *testing.T) *tls.ConnectionState {
	t.Helper()

	certs := make([]*x509.Certificate, len(f.certificate.Certificate))
	for i, der := range f.certificate.Certificate {
		var err error

		certs[i], err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
	}

	return &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: certs, VerifiedChains: [][]*x509.Certificate{certs}}
}

func authFixture(t *testing.T) (*Application, authv1.TokenReviewStatus, string) {
	t.Helper()

	controller := true
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "racer-dataplane", UID: "ds-uid"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "racer-dataplane", UID: "sa-uid"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID(testNodeUID)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "worker-pod", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name, ServiceAccountName: sa.Name}, Status: corev1.PodStatus{PodIP: "192.0.2.1"}}
	r := initializedTopology(t, ds, sa, node, pod)
	a := assembleFixture(r.Config, r.Client, r.APIReader)
	status := authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{wire.TokenAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:racer:racer-dataplane", UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}, "authentication.kubernetes.io/node-name": {node.Name}, "authentication.kubernetes.io/node-uid": {string(node.UID)}}}}
	token := "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".signature"

	return a, status, token
}

func installReview(t *testing.T, a *Application, status authv1.TokenReviewStatus, token string) {
	t.Helper()

	fixtureDependencies[a.authority].Client = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		review, ok := obj.(*authv1.TokenReview)
		if !ok {
			return c.Create(ctx, obj, opts...)
		}

		if review.Spec.Token != token || len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != wire.TokenAudience {
			t.Error("TokenReview did not bind token/audience")
		}

		review.Status = *status.DeepCopy()

		return ctx.Err()
	}})
}

func TestWorkloadNameLabelBounds(t *testing.T) {
	for _, name := range []string{"racer", "racer.custom", strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 63) + ".b", "", "Invalid"} {
		t.Run(name, func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.DaemonSetName = name
			valid := len(validation.IsDNS1123Subdomain(name)) == 0 && len(validation.IsValidLabelValue(name)) == 0

			ds, err := members.DesiredDaemonSet(cfg)
			if !valid {
				if !errors.Is(err, wire.InvalidRequest) || ds != nil {
					t.Fatalf("invalid name accepted: %v", err)
				}

				return
			}

			if err != nil || ds.Name != name || ds.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != name || ds.Spec.Template.Labels["app.kubernetes.io/instance"] != name {
				t.Fatalf("valid name not preserved: %v", err)
			}
		})
	}

	// Other resource names are not instance labels and retain DNS subdomain bounds.
	cfg := workloadConfig(t)
	cfg.BootstrapTrustConfigMap = strings.Repeat("a", 63) + ".trust"

	cfg.DataplaneServiceAccount = strings.Repeat("a", 63) + ".account"
	if _, err := members.DesiredDaemonSet(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRDMANICAnnotationWatches(t *testing.T) {
	for _, field := range []string{wire.RDMANICsAnnotation, enrolledRDMANICsAnnotation, wire.RailsAnnotation, wire.AlignmentAnnotation} {
		t.Run(field, func(t *testing.T) {
			before := memberNode()
			after := before.DeepCopy()
			after.Annotations = map[string]string{field: "[]"}
			require.True(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: &before, ObjectNew: after}))
			require.True(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: after, ObjectNew: &before}))
			require.False(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: after, ObjectNew: after.DeepCopy()}))
		})
	}
}

func TestMixedControllerBootstrapBindings(t *testing.T) {
	a, status, token := authFixture(t)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "podnet"}}
	require.NoError(t, a.Topology.Create(t.Context(), ds))

	pod := &corev1.Pod{}
	require.NoError(t, a.Topology.Get(t.Context(), client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod))
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}
	require.NoError(t, a.Topology.Update(t.Context(), pod))

	for _, scenario := range []string{"success", "sa", "pod", "node"} {
		bound := *status.DeepCopy()

		switch scenario {
		case "sa":
			bound.User.UID = "old-sa"
		case "pod":
			bound.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"old-pod"}
		case "node":
			bound.User.Extra["authentication.kubernetes.io/node-uid"] = authv1.ExtraValue{"old-node"}
		}

		installReview(t, a, bound, token)

		req := httptest.NewRequest("POST", "https://racer/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		identity, err := a.authority.Authenticate(t.Context(), req)
		if scenario == "success" {
			require.NoError(t, err)
			require.Equal(t, wire.NodeID(testNodeUID), identity.Node())
		} else {
			require.ErrorIs(t, err, wire.Forbidden, scenario)
		}
	}
}

func TestMixedControllerEvents(t *testing.T) {
	cfg := Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName}
	p := memberPod("pod", 1, "192.0.2.1")
	p.OwnerReferences[0].Name = PodNetworkDaemonSetName
	pred := managedPodChanges(cfg)
	require.True(t, pred.Create(event.CreateEvent{Object: &p}))
	require.True(t, pred.Delete(event.DeleteEvent{Object: &p}))
	changed := p.DeepCopy()
	changed.Status.PodIP = "192.0.2.2"
	require.True(t, pred.Update(event.UpdateEvent{ObjectOld: &p, ObjectNew: changed}))
	changed = p.DeepCopy()
	changed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: &p, ObjectNew: changed}))
	p.OwnerReferences[0].Name = "arbitrary"
	require.False(t, pred.Create(event.CreateEvent{Object: &p}))

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName}}
	require.True(t, namedChanges(cfg.Namespace, managedWorkloadNames(cfg)...).Create(event.CreateEvent{Object: ds}))
}

func TestLocalSnapshotsDuringAPIOutage(t *testing.T) {
	f := newServingFixture(t)

	var calls atomic.Int64

	unavailable := interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			calls.Add(1)
			return errors.New("API offline")
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			calls.Add(1)
			return errors.New("API offline")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			calls.Add(1)
			return errors.New("API offline")
		},
	})
	f.a.Topology.APIReader = unavailable
	fixtureDependencies[f.a.authority].reader = unavailable

	fixtureDependencies[f.a.authority].Client = unavailable
	if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil {
		t.Fatal("reconciliation hid API failure")
	}

	if _, err := f.a.Topology.Reconcile(f.ctx, ctrl.Request{}); err == nil {
		t.Fatal("topology hid API failure")
	}

	calls.Store(0)

	endpoint := f.start(t)
	peer := f.client(t, &f.certificate)

	peer.Timeout = wire.PollWait + 5*time.Second
	for range 2 {
		response, err := peer.Get(endpoint + wire.SnapshotPath)
		responseBody(t, response, err, http.StatusOK)
	}

	current, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	response, err := peer.Get(fmt.Sprintf("%s%s?after=%d", endpoint, wire.SnapshotPath, current.Sequence()))
	responseBody(t, response, err, http.StatusServiceUnavailable)

	if calls.Load() != 0 {
		t.Fatalf("handshake/warm/204 used API: %d", calls.Load())
	}
	// Issuance still needs live authorization during the same outage.
	body, err := wire.EncodeBootstrapRequest(f.request)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, endpoint+wire.BootstrapPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	response, err = peer.Do(req)
	responseBody(t, response, err, http.StatusServiceUnavailable)

	if calls.Load() != 0 {
		t.Fatal("stale replica attempted enrollment authorization")
	}

	f.cancel()

	response, err = peer.Get(endpoint + wire.SnapshotPath)
	responseBody(t, response, err, http.StatusServiceUnavailable)
}

func TestObservedInvalidTrustCannotRecoverFromReadFailure(t *testing.T) {
	for _, observer := range []string{"keyring", "topology", "issuance"} {
		t.Run(observer, func(t *testing.T) {
			f := newServingFixture(t)
			endpoint := f.start(t)
			peer := f.client(t, &f.certificate)
			response, err := peer.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, http.StatusOK)

			shared := &corev1.Secret{}

			key := client.ObjectKey{Namespace: f.a.Keyring.Config.Namespace, Name: f.a.Keyring.Config.CredentialsSecretName}
			if err := f.a.Topology.Get(f.ctx, key, shared); err != nil {
				t.Fatal(err)
			}

			valid := bytes.Clone(shared.Data["bundle.json"])

			shared.Data["bundle.json"] = []byte(`{}`)
			if err := f.a.Topology.Update(f.ctx, shared); err != nil {
				t.Fatal(err)
			}

			switch observer {
			case "keyring":
				_, err = f.a.Keyring.Reconcile(f.ctx, ctrl.Request{})
			case "issuance":
				_, err = f.a.authority.Issue(f.ctx, fixtureIdentity(t, f), f.request)
			default:
				_, err = f.a.Topology.Reconcile(f.ctx, ctrl.Request{})
			}

			if err == nil {
				t.Fatal("invalid trust accepted")
			}

			live := fixtureDependencies[f.a.authority].reader

			fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("API offline after invalid observation")
			}})
			if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil {
				t.Fatal("API failure hidden")
			}

			response, err = peer.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, http.StatusServiceUnavailable)

			if err := f.a.authority.TrustReady(); err == nil {
				t.Fatal("read failure restored invalidated roots")
			}

			fresh := f.client(t, &f.certificate)

			response, err = fresh.Get(endpoint + wire.SnapshotPath)
			if err == nil {
				response.Body.Close()
				t.Fatal("handshake accepted missing local trust")
			}

			fixtureDependencies[f.a.authority].reader = live

			shared.Data["bundle.json"] = valid
			if err := f.a.Topology.Update(f.ctx, shared); err != nil {
				t.Fatal(err)
			}

			runKeys(t, f.a.Keyring)
			reconcileTopology(t, f.a.Topology, f.ctx)

			response, err = peer.Get(endpoint + wire.SnapshotPath)
			responseBody(t, response, err, http.StatusOK)
		})
	}
}

func TestTrustReadOutageAtEachAuthorityRead(t *testing.T) {
	for _, resource := range []string{"racer-installation", "racer-version", "issuer.json", "bundle.json"} {
		t.Run(resource, func(t *testing.T) {
			if resource == "issuer.json" || resource == "bundle.json" {
				resource = "racer-credentials"
			}

			f := newServingFixture(t)
			reads := 0

			fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == resource {
					reads++
					return errors.New("API offline")
				}

				return c.Get(ctx, key, obj, opts...)
			}})
			if _, err := f.a.Keyring.Reconcile(f.ctx, ctrl.Request{}); err == nil || reads != 1 {
				t.Fatalf("expected read outage at %s: %v, reads=%d", resource, err, reads)
			}

			if err := f.a.Server.Ready(nil); err != nil {
				t.Fatalf("read outage withdrew local state: %v", err)
			}
		})
	}
}

func TestIssuanceTrustObservationLockHonorsDeadline(t *testing.T) {
	f := newServingFixture(t)

	release := holdFixtureGate(t, f)
	defer release()

	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := f.a.authority.Issue(ctx, fixtureIdentity(t, f), f.request)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gate wait ignored deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("gate wait held enrollment admission past deadline")
	}

	if err := f.a.authority.TrustReady(); err != nil {
		t.Fatalf("canceled gate wait invalidated accepted trust: %v", err)
	}
}

func TestCatalogGateCancellationPreservesAcceptedState(t *testing.T) {
	for _, operation := range []string{"topology", "keyring", "issuance"} {
		for _, held := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/held=%t", operation, held), func(t *testing.T) {
				f := newServingFixture(t)

				identity := fixtureIdentity(t, f)

				roots, err := f.a.authority.TrustPool()
				if err != nil {
					t.Fatal(err)
				}

				publication, err := f.a.authority.Current()
				if err != nil {
					t.Fatal(err)
				}

				var reads atomic.Int64

				reader := interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						reads.Add(1)
						return wire.Unavailable
					},
				})
				if held {
					release := holdFixtureGate(t, f)
					defer release()
				}

				fixtureDependencies[f.a.authority].reader = reader

				ctx, cancel := context.WithTimeout(f.ctx, 20*time.Millisecond)
				defer cancel()

				want := context.DeadlineExceeded

				if !held {
					cancel()

					want = context.Canceled
				}

				done := make(chan error, 1)

				go func() {
					var err error

					switch operation {
					case "topology":
						_, err = f.a.Topology.Reconcile(ctx, ctrl.Request{})
					case "keyring":
						_, err = f.a.Keyring.Reconcile(ctx, ctrl.Request{})
					case "issuance":
						_, err = f.a.authority.Issue(ctx, identity, f.request)
					}

					done <- err
				}()

				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("gate wait cancellation: %v", err)
					}

					if operation != "issuance" && !errors.Is(err, reconcile.TerminalError(nil)) {
						t.Fatalf("canceled reconcile can retry: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("gate wait ignored cancellation")
				}

				if reads.Load() != 0 {
					t.Fatalf("canceled admission read authority: %d", reads.Load())
				}

				if current, err := f.a.authority.TrustPool(); err != nil || !current.Equal(roots) {
					t.Fatalf("canceled admission changed accepted trust: %v", err)
				}

				if current, err := f.a.authority.Current(); err != nil || current.Sequence() != publication.Sequence() {
					t.Fatalf("canceled admission changed publication: %v", err)
				}

				if err := f.a.Server.Ready(nil); err != nil {
					t.Fatalf("canceled admission withdrew readiness: %v", err)
				}
			})
		}
	}
}

func TestNodeChangesSiteLabels(t *testing.T) {
	for _, key := range []string{machinav1.MachineSiteLabelKey, "net.unbounded-cloud.io/site"} {
		for _, values := range [][2]string{{"", "site-a"}, {"site-a", "site-b"}, {"site-a", ""}} {
			old, next := memberNode(), memberNode()
			if values[0] != "" {
				old.Labels = map[string]string{key: values[0]}
			}

			if values[1] != "" {
				next.Labels = map[string]string{key: values[1]}
			}

			require.Equal(t, key == machinav1.MachineSiteLabelKey, nodeChanges().Update(event.UpdateEvent{ObjectOld: &old, ObjectNew: &next}), "%s %v", key, values)
		}
	}

	old, next := memberNode(), memberNode()
	next.Labels = map[string]string{"unrelated": "site-a"}
	require.False(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: &old, ObjectNew: &next}))
}

func TestWorkloadDefaultsAgreeWithController(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":  "11111111-1111-1111-1111-111111111111",
		"RACER_CONTROL_URL": "https://controller:8443", "RACER_DATAPLANE_IMAGE": "racer:test",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := members.ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	runtime, err := ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Namespace != runtime.Namespace || cfg.PeerPort != runtime.PeerPort || cfg.DaemonSetName != runtime.DaemonSetName || cfg.DataplaneServiceAccount != runtime.DataplaneServiceAccount || cfg.BootstrapTrustConfigMap != "racer-bootstrap-trust" {
		t.Fatalf("workload defaults disagree with runtime: %+v", cfg)
	}
}
