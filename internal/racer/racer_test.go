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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/server"
	"github.com/Azure/unbounded/internal/racer/testutil"
	"github.com/Azure/unbounded/internal/racer/wire"
)

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
	// Runtime components retain only private constructor inputs.
	input := a.Topology.config
	require.NotZero(t, input)
	input = Config{}
	require.Zero(t, input)

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
		t.Run(name, func(t *testing.T) { componentConfigFreezes(t, name) })
	}
}

func componentConfigFreezes(t *testing.T, name string) {
	t.Helper()
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
		if a.Topology.config != cfg.effective() {
			t.Fatal("construction ignored pre-use inputs/defaults")
		}

		handler := a.Server.Handler()
		cfg.Limits.HeaderBytes = 0

		var readers sync.WaitGroup
		for range 8 {
			readers.Go(func() {
				r := httptest.NewRequest(http.MethodPost, wire.BootstrapPath, nil)
				r.Header.Set("X-Large", strings.Repeat("x", 16*1024))

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

	input := testConfig(t)
	input.CertificateLifetime = 0
	input.SnapshotMaxAge = 0
	input.PeerPort = 9443

	want := input.effective()
	if err := input.Validate(); err != nil {
		t.Fatal("zero optional lifetimes rejected", err)
	}

	a := Assemble(input, nil, nil)

	get := map[string]func() Config{
		"topology":    func() Config { return a.Topology.config },
		"keyring":     func() Config { return a.Keyring.config },
		"replication": func() Config { return a.Replication.config },
	}[name]
	if got := get(); got != want || got.CertificateLifetime != wire.CertificateLifetime || got.SnapshotMaxAge != 30*time.Second {
		t.Fatal("construction ignored inputs/defaults")
	}

	input = Config{}
	require.Zero(t, input)

	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			if get() != want {
				t.Error("runtime reread mutated construction inputs")
			}
		})
	}

	readers.Wait()
}

func TestDirectComponentConfigDefaults(t *testing.T) {
	for name, get := range map[string]func() Config{
		"topology":    func() Config { return Assemble(Config{}, nil, nil).Topology.config },
		"keyring":     func() Config { return Assemble(Config{}, nil, nil).Keyring.config },
		"bootstrap":   func() Config { return Assemble(Config{}, nil, nil).Topology.config },
		"issuer":      func() Config { return Assemble(Config{}, nil, nil).Keyring.config },
		"replication": func() Config { return Assemble(Config{}, nil, nil).Replication.config },
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

			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid config accepted: %v", err)
			}
		})
	}

	for _, port := range []string{"0", "65536", "-1", "invalid"} {
		t.Setenv("RACER_PEER_PORT", port)

		if _, err := LoadConfig(); err == nil {
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

				if _, err := LoadConfig(); err == nil {
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
		if _, err := ConfigFromLookup(lookup); err == nil {
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

	require.Equal(t, 30*time.Second, cfg.SnapshotMaxAge)
	require.EqualValues(t, 8443, cfg.ReplicationPort)
	require.Equal(t, "racer-controller.controllers.svc", cfg.ReplicationServerName)
	require.Equal(t, "/var/run/secrets/racer-controller/token", cfg.ReplicationTokenFile)
	require.Equal(t, "/etc/racer/tls/ca.crt", cfg.ReplicationTrustFile)
	require.Equal(t, "racer-controller", cfg.ControllerServiceAccount)

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
	a := assembleFixture(r.config, r.Client, r.APIReader)
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

			ds, err := testutil.DesiredDaemonSet(cfg)
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
	if _, err := testutil.DesiredDaemonSet(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRDMANICAnnotationWatches(t *testing.T) {
	for _, field := range []string{wire.RDMANICsAnnotation, enrolledRDMANICsAnnotation} {
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
		require.ErrorIs(t, err, wire.Forbidden, scenario)
		require.Empty(t, identity.Node(), "unconfigured workload must not authenticate")
	}
}

func TestMixedControllerEvents(t *testing.T) {
	cfg := Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName}
	p := memberPod("pod", 1, "192.0.2.1")
	p.OwnerReferences[0].Name = PodNetworkDaemonSetName
	pred := managedPodChanges(cfg)
	require.False(t, pred.Create(event.CreateEvent{Object: &p}))
	require.False(t, pred.Delete(event.DeleteEvent{Object: &p}))
	changed := p.DeepCopy()
	changed.Status.PodIP = "192.0.2.2"
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: &p, ObjectNew: changed}))
	changed = p.DeepCopy()
	changed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: &p, ObjectNew: changed}))
	p.OwnerReferences[0].Name = "arbitrary"
	require.False(t, pred.Create(event.CreateEvent{Object: &p}))

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName}}
	require.False(t, namedChanges(cfg.Namespace, cfg.DaemonSetName).Create(event.CreateEvent{Object: ds}))
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

			key := client.ObjectKey{Namespace: f.a.Keyring.config.Namespace, Name: f.a.Keyring.config.CredentialsSecretName}
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
			t.Run(fmt.Sprintf("%s/held=%t", operation, held), func(t *testing.T) { catalogGateCancellation(t, operation, held) })
		}
	}
}

func catalogGateCancellation(t *testing.T, operation string, held bool) {
	t.Helper()
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

	currentRoots, err := f.a.authority.TrustPool()
	require.NoError(t, err)
	require.True(t, currentRoots.Equal(roots), "canceled admission changed accepted trust")

	current, err := f.a.authority.Current()
	require.NoError(t, err)
	require.Equal(t, publication.Sequence(), current.Sequence(), "canceled admission changed publication")

	if err := f.a.Server.Ready(nil); err != nil {
		t.Fatalf("canceled admission withdrew readiness: %v", err)
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

	cfg, err := testutil.ConfigFromLookup(lookup)
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

	request := wire.BootstrapRequest{SchemaVersion: 1, Cluster: a.Topology.config.Cluster, Enrollment: wire.EnrollmentID(testOtherUID), CSRDER: csr, Shares: wire.DefaultShares}
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

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{a.Topology.config.ReplicationServerName}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}

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
	fixtureTLS(t, a, ctx, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})

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
	s.TLS = fixtureTLS(t, f.a, f.ctx, f.serverCertificate)
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

func fixtureTLS(t *testing.T, a *Application, ctx context.Context, certificate tls.Certificate) *tls.Config {
	t.Helper()

	var chain []byte
	for _, der := range certificate.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}

	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(a.Topology.config.TLSCertificateFile, chain, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(a.Topology.config.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := a.Server.TLSConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}

	return config
}

func testConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("RACER_CLUSTER_ID", testOtherUID)
	t.Setenv("POD_NAMESPACE", "racer")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg.TLSCertificateFile = filepath.Join(dir, "tls.crt")
	cfg.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")

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

	objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation-uid"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh", "initialization_protocol": "staged-v1"}})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithIndex(&corev1.Pod{}, podNodeIndex, podNodeKeys).Build()
	c = interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID(fmt.Sprintf("fake-%s-%d", obj.GetName(), time.Now().UnixNano())))
		}

		return c.Create(ctx, obj, opts...)
	}})

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

	guard, cancel, err := h.Admit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	ctx := guard.Context()
	if _, err := h.ForBase(0, "").WriteTo(ctx, guard, &b); err != nil {
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

func (p *CommittedPublication) admit(ctx context.Context) (*authority.Admission, context.CancelFunc, error) {
	return p.handle.Admit(ctx)
}

func reconcileTopology(t *testing.T, r *TopologyReconciler, ctx context.Context) *CommittedPublication {
	t.Helper()

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	for name := range r.hints {
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "hints", Name: name}})
		if err != nil || result != (ctrl.Result{}) {
			t.Fatalf("hint reconcile: %v, %v", result, err)
		}
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
	if err := deps.reader.Get(t.Context(), client.ObjectKey{Namespace: r.config.Namespace, Name: r.config.CredentialsSecretName}, &secret); err != nil {
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
	if err := authority.New(cfg.authorityConfig(), authority.Dependencies{Reader: reader}).Recover(ctx, nil); err != nil {
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

	cm, _, err := readVersion(t.Context(), r.APIReader, r.config)
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

	volume := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: testNodeUID}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
	r := initializedTopology(t, volume)
	a := assembleFixture(r.config, r.Client, r.APIReader)
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
