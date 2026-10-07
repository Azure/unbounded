// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/testutil"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func unusedAddress(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	address := l.Addr().String()
	require.NoError(t, l.Close())

	return address
}

func integrationTLS(t *testing.T, cfg *Config) *x509.CertPool {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{cfg.ReplicationServerName}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.3")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	require.NoError(t, err)
	private, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()

	cfg.TLSCertificateFile, cfg.TLSPrivateKeyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, block := range map[string]*pem.Block{cfg.TLSCertificateFile: {Type: "CERTIFICATE", Bytes: der}, cfg.TLSPrivateKeyFile: {Type: "PRIVATE KEY", Bytes: private}} {
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	return roots
}

func boundPodToken(t *testing.T, kube kubernetes.Interface, pod *corev1.Pod, serviceAccount, audience string) string {
	t.Helper()
	token, err := kube.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(t.Context(), serviceAccount, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: ptr.To(int64(3600)), BoundObjectRef: &authv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
	require.NoError(t, err)

	return token.Status.Token
}

func workloadConfig(t *testing.T) testutil.Config {
	t.Helper()

	return testutil.Config{
		Cluster: "11111111-1111-1111-1111-111111111111", Namespace: "racer",
		ControlURL: "https://racer-controller.racer.svc:8443", DataplaneImage: "racer:test",
		BootstrapTrustConfigMap: "racer-bootstrap-trust", PeerPort: 8082,
		DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane",
	}
}

func TestWorkloadPeerMembership(t *testing.T) {
	for _, port := range []uint16{8082, 7443, 9090, 9091, 65535} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.PeerPort = port
			ds, err := testutil.DesiredDaemonSet(cfg)
			require.NoError(t, err)
			assertWorkloadPeerMembership(t, ds, port)
		})
	}
}

// Exercise ordered downward-API expansion and membership together.
func assertWorkloadPeerMembership(t *testing.T, ds *appsv1.DaemonSet, peerPort uint16) {
	t.Helper()

	for _, ips := range [][]string{{"192.0.2.1"}, {"2001:db8::1"}, {"192.0.2.1", "2001:db8::1"}, {"2001:db8::1", "192.0.2.1"}} {
		pod := memberPod("peer", 1, ips[0])
		for _, ip := range ips {
			pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
		}

		podIP, listen := "", ""

		for _, env := range ds.Spec.Template.Spec.Containers[0].Env {
			switch env.Name {
			case "RACER_POD_IP":
				require.Empty(t, podIP)
				require.Empty(t, env.Value)
				require.Equal(t, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}, env.ValueFrom)

				podIP = pod.Status.PodIP
			case "RACER_PEER_LISTEN":
				if podIP == "" || listen != "" || env.ValueFrom != nil || env.Value != "[$(RACER_POD_IP)]:"+strconv.Itoa(int(peerPort)) {
					t.Fatal("peer listener must expand the preceding Pod IP and configured peer port")
				}

				listen = strings.ReplaceAll(env.Value, "$(RACER_POD_IP)", podIP)
			}
		}

		host, port, err := net.SplitHostPort(listen)
		require.NoError(t, err)
		ip, err := netip.ParseAddr(host)
		require.NoError(t, err)
		require.False(t, ip.IsUnspecified())
		require.Equal(t, strconv.Itoa(int(peerPort)), port)
		candidate, diagnostics, err := reconcileMembers([]corev1.Node{memberNode()}, map[string][]corev1.Pod{pod.Spec.NodeName: {pod}}, memberOwnership(t, testDaemonSetUID), nil, peerPort)
		require.NoError(t, err)
		require.Empty(t, diagnostics)
		require.Len(t, candidate, 1)
		require.Equal(t, netip.AddrPortFrom(ip, peerPort).String(), candidate[testNodeUID].PeerEndpoint)
	}
}

func TestVolumeChanges(t *testing.T) {
	volume := catalogVolume("cache", testNodeUID)
	p := volumeChanges()
	require.True(t, p.Create(event.CreateEvent{Object: &volume}))
	require.True(t, p.Delete(event.DeleteEvent{Object: &volume}))

	for _, tt := range []struct {
		name   string
		mutate func(*racerv1.ClusterVolume)
		want   bool
	}{
		{"unchanged", func(*racerv1.ClusterVolume) {}, false},
		{"resource version", func(v *racerv1.ClusterVolume) { v.ResourceVersion = "2" }, false},
		{"labels", func(v *racerv1.ClusterVolume) { v.Labels = map[string]string{"test": "value"} }, false},
		{"uid", func(v *racerv1.ClusterVolume) { v.UID = testOtherUID }, true},
		{"name", func(v *racerv1.ClusterVolume) { v.Name = "other" }, true},
		{"type", func(v *racerv1.ClusterVolume) { v.Spec.Type = "Future" }, true},
		{"zero type", func(v *racerv1.ClusterVolume) { v.Spec.Type = "" }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updated := volume.DeepCopy()
			tt.mutate(updated)
			require.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: &volume, ObjectNew: updated}))
		})
	}

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: &corev1.Node{}, ObjectNew: &volume}))
}

func admissionVolume(name string, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	v := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"type": "Cache"},
	}}
	v.SetGroupVersionKind(gvk)

	return v
}

func cleanupAdmissionObject(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		require.NoError(t, c.Delete(ctx, obj))
	})
}

func integrationVolumeTypeAdmission(t *testing.T, c client.Client) {
	t.Helper()

	for _, tt := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   string
	}{
		{"cache", func(*unstructured.Unstructured) {}, ""},
		{"missing-spec", func(v *unstructured.Unstructured) { delete(v.Object, "spec") }, "spec: Required value"},
		{"null-spec", func(v *unstructured.Unstructured) { v.Object["spec"] = nil }, "spec: Required value"},
		{"missing-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{} }, "spec.type: Required value"},
		{"null-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": nil} }, "spec.type: Required value"},
		{"empty-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": ""} }, "spec.type: Unsupported value"},
		{"invalid-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": "Future"} }, "spec.type: Unsupported value"},
		{"wrong-case", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": "cache"} }, "spec.type: Unsupported value"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := admissionVolume("type-"+tt.name, racerv1.GroupVersion.WithKind("ClusterVolume"))
			tt.mutate(v)

			err := c.Create(t.Context(), v)
			if err == nil {
				cleanupAdmissionObject(t, c, v)
			}

			if tt.want != "" {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				require.ErrorContains(t, err, tt.want)

				return
			}

			require.NoError(t, err)
			v.SetLabels(map[string]string{"test": "metadata-update"})
			require.NoError(t, c.Update(t.Context(), v))
			v.Object["spec"] = map[string]any{"type": "Future"}
			err = c.Update(t.Context(), v)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "spec.type: Unsupported value")
		})
	}
}

func integrationVolumeTypeImmutability(t *testing.T, c client.Client) {
	t.Helper()
	// Extend only the enum on an isolated test kind to test CEL immutability.
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "clustervolumes." + racerv1.GroupName}, crd))
	crd.Object["metadata"] = map[string]any{"name": "volumetypeprobes." + racerv1.GroupName}
	delete(crd.Object, "status")
	require.NoError(t, unstructured.SetNestedMap(crd.Object, map[string]any{
		"kind": "VolumeTypeProbe", "listKind": "VolumeTypeProbeList", "plural": "volumetypeprobes", "singular": "volumetypeprobes",
	}, "spec", "names"))
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, found)

	version := versions[0].(map[string]any)
	require.NoError(t, unstructured.SetNestedSlice(version, []any{"Cache", "Future"}, "schema", "openAPIV3Schema", "properties", "spec", "properties", "type", "enum"))
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
	require.NoError(t, c.Create(t.Context(), crd))
	cleanupAdmissionObject(t, c, crd)
	require.NoError(t, wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
			return false, err
		}

		raw, _, err := unstructured.NestedFieldNoCopy(crd.Object, "status", "conditions")
		if raw == nil || err != nil {
			return false, err
		}

		conditions := raw.([]any)
		for _, item := range conditions {
			condition := item.(map[string]any)
			if condition["type"] == "Established" && condition["status"] == string(metav1.ConditionTrue) {
				return true, nil
			}
		}

		return false, err
	}))

	v := admissionVolume("immutable", racerv1.GroupVersion.WithKind("VolumeTypeProbe"))
	require.NoError(t, c.Create(t.Context(), v))
	cleanupAdmissionObject(t, c, v)
	v.SetLabels(map[string]string{"test": "same-type"})
	require.NoError(t, c.Update(t.Context(), v))
	v.Object["spec"] = map[string]any{"type": "Future"}
	err = c.Update(t.Context(), v)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.ErrorContains(t, err, "type is immutable")
	require.NotContains(t, err.Error(), "Unsupported value")
}

func TestReplicationValidation(t *testing.T) {
	cfg := testConfig(t)
	cfg.PodName, cfg.PodUID = "controller", "controller-uid"
	require.NoError(t, cfg.validateReplication())

	for name, mutate := range map[string]func(*Config){
		"pod name":        func(c *Config) { c.PodName = "Invalid" },
		"pod UID":         func(c *Config) { c.PodUID = "" },
		"service account": func(c *Config) { c.ControllerServiceAccount = "" },
		"server name":     func(c *Config) { c.ReplicationServerName = "" },
		"port":            func(c *Config) { c.ReplicationPort = 0 },
		"token":           func(c *Config) { c.ReplicationTokenFile = "" },
		"trust":           func(c *Config) { c.ReplicationTrustFile = "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			mutate(&invalid)
			require.Error(t, invalid.validateReplication())
		})
	}
}

func TestReplicationLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &Replication{config: Config{SnapshotMaxAge: 3 * time.Second}.effective()}
		p := &publisherLifetime{replication: r}
		require.True(t, p.NeedLeaderElection())
		require.False(t, r.NeedLeaderElection())
		require.Equal(t, time.Second, r.PollInterval())
		require.Equal(t, 5*time.Second, Assemble(Config{}, nil, nil).Replication.PollInterval())

		ctx, leader := r.LeaderContext()
		require.Nil(t, ctx)
		require.False(t, leader)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- p.Start(ctx) }()

		synctest.Wait()

		observed, leader := r.LeaderContext()
		require.Same(t, ctx, observed)
		require.True(t, leader)
		cancel()
		require.NoError(t, <-done)
		require.False(t, r.isLeader())
		require.False(t, replicationSleep(ctx, time.Hour))
		require.True(t, replicationSleep(t.Context(), time.Second))
	})
}

func TestReplicationObserverLoop(t *testing.T) {
	for _, leader := range []bool{false, true} {
		t.Run(strconv.FormatBool(leader), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := initializedTopology(t)
				require.NoError(t, coordv1.AddToScheme(r.Scheme()))

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				observer := Assemble(r.config, r.Client, r.APIReader).Replication
				if leader {
					observer.leader = ctx
				}

				done := make(chan error, 1)

				go func() { done <- observer.Start(ctx) }()

				time.Sleep(6 * time.Second)
				cancel()
				require.NoError(t, <-done)
			})
		})
	}
}

func replicationPeer(t *testing.T, handler http.Handler) (*Replication, *httptest.Server) {
	t.Helper()
	r := initializedTopology(t)
	cfg := r.config
	integrationTLS(t, &cfg)
	cfg.ReplicationTrustFile = cfg.TLSCertificateFile
	cfg.ReplicationTokenFile = filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(cfg.ReplicationTokenFile, []byte(" token-value\n"), 0o600))
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertificateFile, cfg.TLSPrivateKeyFile)
	require.NoError(t, err)

	peer := httptest.NewUnstartedServer(handler)
	peer.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	peer.StartTLS()
	t.Cleanup(peer.Close)
	host, port, err := net.SplitHostPort(peer.Listener.Addr().String())
	require.NoError(t, err)
	number, err := strconv.ParseUint(port, 10, 16)
	require.NoError(t, err)

	cfg.ReplicationPort = uint16(number)

	require.NoError(t, coordv1.AddToScheme(r.Scheme()))

	for _, obj := range []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "controller", UID: "controller-uid"}, Spec: corev1.PodSpec{ServiceAccountName: cfg.ControllerServiceAccount}, Status: corev1.PodStatus{PodIP: host}},
		&coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "racer-controller"}, Spec: coordv1.LeaseSpec{HolderIdentity: ptr.To("controller/controller-uid"), RenewTime: ptr.To(metav1.NewMicroTime(time.Now())), LeaseDurationSeconds: ptr.To(int32(60))}},
	} {
		require.NoError(t, r.Create(t.Context(), obj))
	}

	return Assemble(cfg, r.Client, r.APIReader).Replication, peer
}

func TestReplicationPollResponses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusForbidden, http.StatusTemporaryRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var image string

			requests := make(chan *http.Request, 2)
			r, _ := replicationPeer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests <- req

				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)

				if status == http.StatusOK {
					_, _ = w.Write([]byte(image))
				}
			}))
			topology := Assemble(r.config, r.Client, r.APIReader).Topology
			image = reconcileTopology(t, topology, t.Context()).encoded

			err := r.poll(t.Context(), t.Context())
			if status == http.StatusOK || status == http.StatusNoContent {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, "replication HTTP status "+strconv.Itoa(status))
			}

			req := <-requests
			require.Equal(t, "Bearer token-value", req.Header.Get("Authorization"))
			require.Empty(t, req.URL.RawQuery)
			require.Empty(t, requests, "redirects must not forward credentials")

			if status == http.StatusOK {
				require.NoError(t, r.poll(t.Context(), t.Context()))
				require.Equal(t, "1", (<-requests).URL.Query().Get("after"))
			} else {
				require.Error(t, r.authority.PublicationReady(), "responses must not grant freshness")
			}
		})
	}
}

func TestReplicationPollFailures(t *testing.T) {
	for _, stage := range []string{"leader", "trust missing", "trust invalid", "token", "TLS", "decode", "unconfirmed"} {
		t.Run(stage, func(t *testing.T) {
			body := "invalid"
			r, _ := replicationPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			cfg := r.config

			switch stage {
			case "leader":
				r.APIReader = fake.NewClientBuilder().WithScheme(r.Client.Scheme()).Build()
			case "trust missing":
				cfg.ReplicationTrustFile += ".missing"
			case "trust invalid":
				require.NoError(t, os.WriteFile(cfg.ReplicationTrustFile, []byte("invalid"), 0o600))
			case "token":
				cfg.ReplicationTokenFile += ".missing"
			case "TLS":
				cfg.ReplicationServerName = "wrong.invalid"
			case "unconfirmed":
				p := reconcileTopology(t, Assemble(cfg, r.Client, r.APIReader).Topology, t.Context())
				image, err := wire.DecodePublication(strings.NewReader(p.encoded))
				require.NoError(t, err)

				image.Sequence++
				encoded, err := wire.EncodePublication(image)
				require.NoError(t, err)

				body = string(encoded)
			}

			r = Assemble(cfg, r.Client, r.APIReader).Replication
			require.Error(t, r.poll(t.Context(), t.Context()))
			require.Error(t, r.authority.PublicationReady())
		})
	}
}

func TestLeaderDiscoveryRejectsInvalidLease(t *testing.T) {
	for _, holder := range []*string{nil, ptr.To(""), ptr.To("controller"), ptr.To("/uid"), ptr.To("controller/"), ptr.To("missing/uid")} {
		r, _ := replicationPeer(t, http.NotFoundHandler())

		var lease coordv1.Lease

		key := client.ObjectKey{Namespace: r.config.Namespace, Name: "racer-controller"}
		require.NoError(t, r.Client.Get(t.Context(), key, &lease))
		lease.Spec.HolderIdentity = holder
		require.NoError(t, r.Client.Update(t.Context(), &lease))
		_, err := r.leaderAddress(t.Context())
		require.Error(t, err)
	}
}

func TestReplicaAuthenticationDelegates(t *testing.T) {
	r := initializedTopology(t)
	boom := errors.New("review failed")
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return boom }})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer token")
	uid, expires, err := Assemble(r.config, c, c).Replication.AuthenticateReplica(t.Context(), request)
	require.Error(t, err)
	require.Empty(t, uid)
	require.Zero(t, expires)
}
