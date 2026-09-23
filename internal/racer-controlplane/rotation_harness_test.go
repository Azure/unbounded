// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	pb "github.com/Azure/unbounded/api/racer"
	originfixture "github.com/Azure/unbounded/e2e/racer/fixture"
	"github.com/Azure/unbounded/internal/racer/pki"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

// Two real daemons enroll their own private keys, forward peer reads over TLS,
// and prove each installed trust generation to the production Go proof server.
// Slab I/O remains throttled during UDS traffic and every CA transition.
// Fixture snapshots delivered over mTLS give each process distinct local sockets
// on this host. The coordination harness covers durable rollout decisions.
func TestProductionCARotationTraffic(t *testing.T) {
	binary := os.Getenv("RACER_DATAPLANE_BINARY")
	if binary == "" {
		t.Skip("set RACER_DATAPLANE_BINARY to the Rust production daemon")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Second)
	defer cancel()

	pod, daemon, node, site := enrollmentObjects()
	pod.UID = "pod-survivor"
	node2, pod2 := node.DeepCopy(), pod.DeepCopy()
	node2.Name, node2.UID = "worker-two", "node-two"
	pod2.Name, pod2.UID, pod2.Spec.NodeName = "racer-worker-two", "pod-failed", node2.Name

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, machina.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	kube := multiTokenClient{fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, pod2, daemon, node, node2, site).Build()}

	// Renewal must leave room for the production 30-second connection drain.
	manager, err := pki.New(kube, "system", pki.Options{LeafLifetime: 120 * time.Second, ClockSkew: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.AcquireLeadership(ctx, "rotation-test"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Publish(ctx); err != nil {
		t.Fatal(err)
	}

	cpID := pki.Identity{Kind: pki.ControlPlane, PodUID: "controller", BootID: "rotation-test"}
	hot, proofHot := pki.NewHotTLS(), pki.NewHotTLS()
	connections := new(tlsConnections)

	var (
		current    pki.TrustBundle
		leafExpiry time.Time
	)

	refresh := func() {
		t.Helper()

		bundle, err := manager.Bundle(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if bundle.Generation == current.Generation && time.Now().Add(12*time.Second).Before(leafExpiry) {
			return
		}

		leaf, key := issueTLSFixture(t, manager, cpID, false)

		leafExpiry = leaf.NotAfter
		if err := hot.Update(bundle.JSON(), leaf.CertificatePEM, key); err != nil {
			t.Fatal(err)
		}

		leaf, key = issueTLSFixture(t, manager, cpID, true)
		if err := proofHot.Update(bundle.JSON(), leaf.CertificatePEM, key); err != nil {
			t.Fatal(err)
		}

		connections.rotate()

		current = bundle
	}
	refresh()

	enrollment := &enrollmentServer{kube: kube, review: kube, namespace: "system", issue: func(ctx context.Context, csr string, id enrollmentIdentity) (enrollmentResponse, error) {
		leaf, err := manager.Issue(ctx, []byte(csr), pki.Identity{Kind: pki.Node, Universe: id.universe, Node: id.node, PodUID: id.podUID, BootID: id.boot})
		return enrollmentResponse{Certificate: string(leaf.CertificatePEM), Generation: leaf.Bundle.Generation, Issuer: leaf.RootDigest}, err
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/enroll", enrollment.enroll)

	configs := make(map[string]string)
	for _, n := range []*corev1.Node{node, node2} {
		configs[identity("node", string(n.UID))] = t.TempDir()
	}

	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, r *http.Request) {
		uid, err := authenticateControl(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		raw, err := os.ReadFile(filepath.Join(configs[r.PathValue("node")], "config.json"))

		var configuration pb.Configuration
		if err != nil || protojson.Unmarshal(raw, &configuration) != nil {
			http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
			return
		}

		snapshot := configuration.GetSnapshot()
		wire, _ := marshalSnapshot(snapshot)
		digest := sha256.Sum256(wire)
		boot, _ := hex.DecodeString(r.Header.Get("X-Racer-Boot"))
		phase, _ := strconv.Atoi(r.Header.Get("X-Racer-Phase"))
		body, _ := proto.Marshal(&pb.ControlCommand{Universe: snapshot.Universe, Node: snapshot.Node, Incarnation: boot, SnapshotDigest: digest[:], Revision: 1, Phase: uint32(min(phase+1, 4)), Configuration: &configuration, Profile: 1, PodUid: uid})

		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(body)
	})
	server := httptest.NewUnstartedServer(mux)
	server.TLS = hot.ServerConfig(tls.VerifyClientCertIfGiven)
	server.Config.ConnState = connections.state
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.IdleTimeout = 30 * time.Second

	server.StartTLS()
	defer server.Close()

	proofListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	proofDone := make(chan error, 1)

	go func() { proofDone <- serveTrustProof(ctx, proofListener, proofHot, manager) }()

	defer func() {
		cancel()

		if err := <-proofDone; err != nil {
			t.Error(err)
		}
	}()
	// The CP's proof uses its real server-authenticated handshake too, rather
	// than injecting exported acknowledgment fields as proof of trust.
	cpProof := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	cpProof.TLS = proofHot.ServerConfig(tls.NoClientCert)

	cpProof.StartTLS()
	defer cpProof.Close()

	proveCP := func() {
		t.Helper()

		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", cpProof.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()

		conn, finish, err := hot.HandshakeProof(ctx, raw, false, "racer-controlplane.system.svc")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		proof, err := finish(pki.Acknowledgment{Generation: current.Generation, Digest: current.Digest(), OldConnectionsDrained: connections.drained()})
		if err != nil {
			t.Fatal(err)
		}

		if err := manager.RecordTLSProof(ctx, cpID.Key(), proof); err != nil {
			t.Fatal(err)
		}
	}

	universe := identity("universe", "edge")
	u, _ := hex.DecodeString(universe)
	ids := []string{identity("node", string(node.UID)), identity("node", string(node2.UID))}
	pods := []*corev1.Pod{pod, pod2}
	dirs, metrics := make([]string, 2), make([]string, 2)
	clients := make([]*sdk.Client, 2)

	socketDir, err := os.MkdirTemp("", "rotation-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })

	for i := range 2 {
		dirs[i] = configs[ids[i]]
		metrics[i] = rotationAddress(t, fmt.Sprintf("127.0.0.%d", i+2))
	}

	for i := range 2 {
		cacheSocket := filepath.Join(socketDir, fmt.Sprintf("cache-%d", i))
		originSocket := filepath.Join(socketDir, fmt.Sprintf("origin-%d", i))

		originListener, err := net.Listen("unix", originSocket)
		if err != nil {
			t.Fatal(err)
		}

		backend := originfixture.NewOrigin()
		backend.Source = pods[i].Spec.NodeName
		origin := httptest.NewUnstartedServer(backend)
		origin.Listener.Close()
		origin.Listener = originListener
		origin.Start()
		t.Cleanup(origin.Close)

		n, _ := hex.DecodeString(ids[i])
		peer := 1 - i
		snapshot := &pb.Snapshot{
			Universe: u, Node: n, Revision: 1, Epoch: 1,
			Peers: []*pb.Peer{{Id: ids[peer], HttpAddress: fmt.Sprintf("127.0.0.%d:9443", peer+2), PodUid: string(pods[peer].UID)}},
			Volumes: []*pb.Volume{{
				Id: "cache-uid", CacheSocket: cacheSocket, OriginSocket: originSocket, CacheGeneration: 1,
				Peers: []string{ids[peer]}, PeerEndpoints: &pb.VolumePeerEndpoints{Peers: []*pb.VolumePeerEndpoint{{Peer: ids[peer]}}},
				Topology: &pb.Topology{Epoch: 1, SlotCount: 2, LocalSlots: []uint32{uint32(i)}, Neighbors: []*pb.SlotPeer{{Slot: uint32(peer), Peer: ids[peer]}}},
			}},
		}

		wire, err := protojson.Marshal(&pb.Configuration{Contents: &pb.Configuration_Snapshot{Snapshot: snapshot}})
		if err != nil {
			t.Fatal(err)
		}

		for name, data := range map[string][]byte{"config.json": wire, pki.BundleKey: current.JSON(), "token": []byte(pods[i].UID)} {
			if err := os.WriteFile(filepath.Join(dirs[i], name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		cmd := exec.CommandContext(ctx, binary)

		for _, env := range os.Environ() {
			if !strings.HasPrefix(env, "RACER_") {
				cmd.Env = append(cmd.Env, env)
			}
		}

		cmd.Env = append(cmd.Env,
			// One operation per burst forces file/checkpoint traffic to wait for
			// refill while leaving the production request and rotation deadlines intact.
			"RACER_SLAB_IOPS=100", "RACER_SLAB_IO_BURST=1",
			"RACER_CONTROL_PLANE_URL="+server.URL+"/v3/"+universe+"/"+ids[i], "RACER_UNIVERSE="+universe, "RACER_NODE="+ids[i],
			"RACER_TLS_TRUST_DIR="+dirs[i], "RACER_CONTROL_TOKEN_FILE="+filepath.Join(dirs[i], "token"),
			"RACER_ENROLL_URL="+server.URL+"/v3/enroll", "RACER_TRUST_PROOF_URL=https://"+proofListener.Addr().String()+"/v3/proof",
			"RACER_CONTROL_SERVER_NAME=racer-controlplane.system.svc", "RACER_POD_NAMESPACE=system", "RACER_POD_NAME="+pods[i].Name, "RACER_POD_UID="+string(pods[i].UID),
			"RACER_SLAB_PATH="+filepath.Join(dirs[i], "cache.slab"), "RACER_SLAB_SIZE=134217728", "RACER_SHARDS=1", "RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=8", "RACER_RDMA_MODE=disabled", "RACER_METRICS_ADDR="+metrics[i], "RACER_POD_IP="+fmt.Sprintf("127.0.0.%d", i+2))

		log, err := os.Create(filepath.Join(dirs[i], "daemon.log"))
		if err != nil {
			t.Fatal(err)
		}

		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}

		done := make(chan error, 1)

		go func() { done <- cmd.Wait() }()

		t.Cleanup(func() {
			_ = cmd.Process.Signal(os.Interrupt)

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()

				<-done
			}

			log.Close()

			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("daemon %d: %s", i, data)
			}
		})

		clients[i], err = sdk.NewClient(cacheSocket, sdk.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer clients[i].CloseIdleConnections()
	}

	httpClient := &http.Client{Timeout: time.Second}
	defer httpClient.CloseIdleConnections()
	defer func() {
		if t.Failed() {
			for _, address := range metrics {
				for _, path := range []string{"status", "metrics"} {
					response, err := httpClient.Get("http://" + address + "/" + path)
					if err == nil {
						data, _ := io.ReadAll(response.Body)
						response.Body.Close()
						t.Logf("%s %s: %s", address, path, data)
					}
				}
			}
		}
	}()

	for _, address := range metrics {
		for {
			response, err := httpClient.Get("http://" + address + "/readyz")
			if err == nil {
				response.Body.Close()

				if response.StatusCode == 200 {
					break
				}
			}

			if ctx.Err() != nil {
				t.Fatal("dataplane did not become Ready")
			}

			time.Sleep(50 * time.Millisecond)
		}
	}

	trafficCtx, stopTraffic := context.WithCancel(ctx)
	trafficDone := make(chan error, 1)

	var reads atomic.Uint64

	go func() {
		for count := 0; ; count++ {
			for _, c := range clients {
				// Fresh metadata forces peer exchanges throughout every rotation
				// phase even after both nodes have cached the bounded payload set.
				_, err := c.Open(trafficCtx, fmt.Sprintf("/rotation-metadata-%d", count))

				var object *sdk.Object
				if err == nil {
					object, err = c.Open(trafficCtx, fmt.Sprintf("/rotation-%d", count%8))
				}

				if err == nil {
					data := make([]byte, originfixture.ObjectSize)

					var n int

					n, err = object.ReadAt(trafficCtx, data, 0)
					if err == nil && (n != len(data) || !bytes.Equal(data, originfixture.Body(1))) {
						err = fmt.Errorf("rotation corrupted object bytes")
					}
				}

				if err != nil {
					if trafficCtx.Err() != nil && errors.Is(err, context.Canceled) {
						err = nil
					}

					trafficDone <- err

					return
				}

				reads.Add(1)
			}

			select {
			case <-trafficCtx.Done():
				trafficDone <- nil
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	joined := false

	defer func() {
		stopTraffic()

		if !joined {
			<-trafficDone
		}
	}()

	time.Sleep(time.Second)

	select {
	case err := <-trafficDone:
		joined = true

		t.Fatalf("baseline traffic failed: %v", err)
	default:
	}

	// Exclude initialization and baseline traffic from the limiter evidence.
	// Each daemon must do real, token-delayed slab work after rotation starts.
	slabBefore := make([]map[string]float64, len(metrics))
	for i, address := range metrics {
		slabBefore[i] = rotationSlabMetrics(t, rotationMetrics(t, httpClient, address))
	}

	if err := manager.TriggerRotation(ctx); err != nil {
		t.Fatal(err)
	}

	initial := current
	seen := map[uint64]bool{}
	logAt := time.Now().Add(30 * time.Second)

	for {
		refresh()

		seen[current.Generation] = true
		for _, dir := range dirs {
			if err := os.WriteFile(filepath.Join(dir, "bundle.next"), current.JSON(), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := os.Rename(filepath.Join(dir, "bundle.next"), filepath.Join(dir, pki.BundleKey)); err != nil {
				t.Fatal(err)
			}
		}

		proveCP()

		if err := manager.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}

		if time.Now().After(logAt) {
			logAt = time.Now().Add(30 * time.Second)

			var state corev1.Secret

			_ = kube.Get(ctx, client.ObjectKey{Namespace: "system", Name: pki.SecretName}, &state)

			var safe struct {
				Phase       string
				Authorities []struct {
					Digest           string
					LastIssuedExpiry time.Time
				}
			}

			_ = json.Unmarshal(state.Data[pki.StateKey], &safe)
			t.Logf("rotation progress: generation=%d drained=%v state=%+v", current.Generation, connections.drained(), safe)
		}

		if current.Generation == initial.Generation+3 {
			break
		}

		select {
		case err := <-trafficDone:
			joined = true

			t.Fatalf("continuous traffic stopped during rotation: %v", err)
		case <-ctx.Done():
			var state corev1.Secret

			_ = kube.Get(t.Context(), client.ObjectKey{Namespace: "system", Name: pki.SecretName}, &state)

			var safe struct {
				Phase       string
				Members     json.RawMessage
				Authorities []struct {
					Digest           string
					LastIssuedExpiry time.Time
				}
			}

			_ = json.Unmarshal(state.Data[pki.StateKey], &safe)

			connections.mu.Lock()
			t.Logf("tracked control connections: %+v", connections.connections)
			connections.mu.Unlock()

			var participants corev1.ConfigMapList

			_ = kube.List(t.Context(), &participants)

			var refs struct {
				Shards map[string]struct{ Name string }
			}

			_ = json.Unmarshal(state.Data[pki.StateKey], &refs)
			for _, ref := range refs.Shards {
				for _, cm := range participants.Items {
					if cm.Name == ref.Name {
						t.Logf("participant %s: %v", cm.Name, cm.Data)
					}
				}
			}

			t.Fatalf("rotation stalled at generation %d: %+v", current.Generation, safe)
		case <-time.After(100 * time.Millisecond):
		}
	}

	if current.Active == initial.Active || strings.Count(current.Certificates, "BEGIN CERTIFICATE") != 1 || len(seen) != 3 {
		t.Fatal("rotation did not overlap, switch, and retire the old CA")
	}
	// Wait for retirement projection and exercise new peer sessions past the old
	// leaf expiry, including ordinary leaf renewal under the new CA.
	time.Sleep(3 * time.Second)
	stopTraffic()

	err = <-trafficDone
	joined = true

	if err != nil {
		t.Fatal(err)
	}

	for i, address := range metrics {
		response, err := httpClient.Get("http://" + address + "/status")
		if err != nil {
			t.Fatal(err)
		}

		var status struct {
			Ready bool
			TLS   struct {
				Generation                uint64
				Issuer                    string
				InstalledWorkers, Workers int
				Error                     *string
			}
		}

		err = json.NewDecoder(response.Body).Decode(&status)
		response.Body.Close()

		if err != nil || !status.Ready || status.TLS.Generation != current.Generation || status.TLS.Issuer != current.Active || status.TLS.InstalledWorkers != status.TLS.Workers || status.TLS.Error != nil {
			t.Fatalf("retirement not installed: %+v %v", status, err)
		}

		data := rotationMetrics(t, httpClient, address)

		slabAfter := rotationSlabMetrics(t, data)
		for name, before := range slabBefore[i] {
			if !(slabAfter[name] > before) {
				t.Fatalf("daemon %d limiter was not exercised during rotation: %s before=%g after=%g", i, name, before, slabAfter[name])
			}
		}

		t.Logf("daemon %d slab rotation deltas: operations=%g bytes=%g waits=%g wait_seconds=%g", i,
			slabAfter["operations_total"]-slabBefore[i]["operations_total"],
			slabAfter["bytes_total"]-slabBefore[i]["bytes_total"],
			slabAfter["waits_total"]-slabBefore[i]["waits_total"],
			slabAfter["wait_seconds_total"]-slabBefore[i]["wait_seconds_total"])

		var peerRequests, handshakes float64

		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}

			value, _ := strconv.ParseFloat(fields[1], 64)
			if strings.HasPrefix(fields[0], "racer_dataplane_upstream_requests_total{destination=\"peer\",transport=\"http\"") {
				peerRequests += value
			}

			if fields[0] == "racer_dataplane_tls_handshakes_total" {
				handshakes = value
			}
		}

		if peerRequests == 0 || handshakes < 3 {
			t.Fatalf("missing peer traffic or TLS reconnects: requests=%g handshakes=%g", peerRequests, handshakes)
		}
	}

	if reads.Load() < 100 {
		t.Fatalf("insufficient continuous reads: %d", reads.Load())
	}

	t.Logf("%d continuous SDK reads survived CA generations %d through %d, old-root retirement, and live leaf renewal with active slab throttling", reads.Load(), initial.Generation, current.Generation)
}

func rotationMetrics(t *testing.T, httpClient *http.Client, address string) []byte {
	t.Helper()

	response, err := httpClient.Get("http://" + address + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("daemon metrics: status=%d error=%v", response.StatusCode, err)
	}

	return data
}

func rotationSlabMetrics(t *testing.T, data []byte) map[string]float64 {
	t.Helper()

	values := make(map[string]float64)

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		name, ok := strings.CutPrefix(fields[0], "racer_dataplane_slab_io_")
		if !ok {
			continue
		}

		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || !(value >= 0) {
			t.Fatalf("invalid slab metric %s: %q", name, fields[1])
		}

		values[name] = value
	}

	for _, name := range []string{"operations_total", "bytes_total", "waits_total", "wait_seconds_total"} {
		if _, ok := values[name]; !ok {
			t.Fatalf("missing slab metric %s", name)
		}
	}

	return values
}

func rotationAddress(t *testing.T, host string) string {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	return address
}
