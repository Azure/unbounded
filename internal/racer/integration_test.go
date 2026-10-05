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
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
)

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
	t.Run("staged-initialization", func(t *testing.T) { integrationStagedInitialization(t, c) })
	t.Run("cache-name-admission", func(t *testing.T) { integrationCacheNameAdmission(t, c) })
	t.Run("catalog-capacity", func(t *testing.T) { integrationCatalogCapacity(t, c) })
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

	return Assemble(cfg, c, c)
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

		go func() { results <- ensureInstalled(ctx, r.Client, r.APIReader, r.Config) }()
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

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Race after CommitVersion's authoritative read, so the API server, rather
	// than our preliminary resourceVersion comparison, must reject the write.
	r.Client = interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
		other := cm.DeepCopy()

		other.Labels = map[string]string{"concurrent": "writer"}
		if err := c.Update(ctx, other); err != nil {
			return err
		}

		return c.Update(ctx, obj, opts...)
	}}
	if committed, err := r.CommitVersion(ctx, prepared); !apierrors.IsConflict(err) || committed != nil {
		t.Fatalf("real CAS failed: committed=%v err=%v", committed != nil, err)
	}

	r.Client = c

	cm, previous, err = readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err = r.Publications.Prepare(previous, cm.ResourceVersion, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	leader, cancel := context.WithCancel(ctx)

	committed, err := r.CommitVersion(leader, prepared)
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	if err := r.Publications.Install(committed); !errors.Is(err, context.Canceled) {
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
	r.Config.Rotation.Interval = 7 * 24 * time.Hour
	now := time.Now().UTC().Truncate(time.Second)
	r.Now = func() time.Time { return now }
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

		r.Client = interruptedClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
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
		if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !failed || !errors.Is(err, boom) || trustReady(r.Trust) {
			t.Fatalf("%s interruption: %v", step.name, err)
		}

		_, before, beforeState, private := keyState(t, r)
		recovered := Assemble(r.Config, c, c).Keyring
		recovered.Now = r.Now
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

		apps[i] = Assemble(cfg, mgr.GetClient(), mgr.GetAPIReader())

		apps[i].Topology.Client = interruptedClient{Client: mgr.GetClient(), update: func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
			if err := mgr.GetClient().Update(ctx, obj, opts...); err != nil {
				return err
			}

			if _, ok := obj.(*corev1.ConfigMap); ok && obj.GetName() == apps[i].Topology.Config.VersionConfigMapName {
				topologyCommitted[i].Store(true)
			}

			return nil
		}}
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
		response, err := http.Get("http://" + app.Server.Config.ProbeAddress + "/readyz")

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

	workload, err := workload.DesiredDaemonSet(workload.Config{
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
		p, err := apps[follower].Server.Publications.Current()
		return err == nil && p.record.Sequence == publication.Sequence
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

	awaitServerPolls(t, apps[leader].Server, 1)

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

	if _, err := apps[follower].Server.Publications.Current(); !errors.Is(err, context.Canceled) {
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
	// A separate handler shares the elected application's lifecycle/publication.
	// Its reader records actual API responses without mutating running dependencies.
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

	measured := Assemble(a.Server.Config, reader, reader).Server
	measured.Lifecycle, measured.Publications = a.Lifecycle, a.Server.Publications
	measured.Trust = a.Server.Trust

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

	cfg := a.Server.Config
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
		p, err := a.Server.Publications.Current()
		return err == nil && strings.Contains(p.encoded, string(node.UID))
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
