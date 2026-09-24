//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/e2e/racer/fixture"
	"github.com/Azure/unbounded/internal/gantry/config"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

// These tests intentionally fail on missing prerequisites. Run each top-level
// test separately with timeout 60s and go test -timeout 50s. Builds belong outside
// that budget. Only the subprocess uses root, a private /dev, and loopback-only
// networking; all backing files live under the caller's workspace TMPDIR.
func gantryNamespace(t *testing.T) bool {
	t.Helper()

	if os.Getenv("GANTRY_RACER_CHILD") == "1" {
		return false
	}

	for _, key := range []string{"RACER_DATAPLANE_BINARY", "GANTRY_BINARY", "TMPDIR"} {
		value := os.Getenv(key)
		if !filepath.IsAbs(value) {
			t.Fatalf("%s must be an absolute workspace path", key)
		}

		if _, err := os.Stat(value); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sudo", "-n", "unshare", "--mount", "--net", "--pid", "--fork", "--kill-child", "env",
		"GANTRY_RACER_CHILD=1", "GANTRY_RACER_DIR="+dir, "GANTRY_RACER_UID="+strconv.Itoa(os.Getuid()), "GANTRY_RACER_GID="+strconv.Itoa(os.Getgid()), "TMPDIR="+os.Getenv("TMPDIR"),
		"RACER_DATAPLANE_BINARY="+os.Getenv("RACER_DATAPLANE_BINARY"), "GANTRY_BINARY="+os.Getenv("GANTRY_BINARY"), "RACER_REQUIRE_KTLS="+os.Getenv("RACER_REQUIRE_KTLS"),
		"RACER_LOADGEN_BINARY="+os.Getenv("RACER_LOADGEN_BINARY"),
		os.Args[0], "-test.v", "-test.timeout=40s", "-test.run=^"+t.Name()+"$")
	out, err := cmd.CombinedOutput()
	t.Logf("isolated processes:\n%s", out)

	if err != nil {
		t.Fatalf("isolated integration: %v", err)
	}

	return true
}

type gantryProcess struct {
	cmd     *exec.Cmd
	done    chan error
	log     string
	stopped bool
}

func gantryStart(t *testing.T, dir, name string, env []string, binary string, args ...string) *gantryProcess {
	t.Helper()

	f, err := os.Create(filepath.Join(dir, name+".log"))
	if err != nil {
		t.Fatal(err)
	}

	p := &gantryProcess{cmd: exec.Command(binary, args...), done: make(chan error, 1), log: f.Name()}

	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "RACER_") && !strings.HasPrefix(value, "GANTRY_") {
			p.cmd.Env = append(p.cmd.Env, value)
		}
	}

	p.cmd.Env = append(p.cmd.Env, env...)

	p.cmd.Stdout, p.cmd.Stderr = f, f
	if err := p.cmd.Start(); err != nil {
		f.Close()
		t.Fatal(err)
	}

	f.Close()

	go func() { p.done <- p.cmd.Wait() }()

	t.Cleanup(func() {
		p.stop()

		if t.Failed() {
			data, _ := os.ReadFile(p.log)
			t.Logf("%s:\n%s", name, data)
		}
	})

	return p
}

func (p *gantryProcess) stop() {
	if p.stopped {
		return
	}

	p.stopped = true

	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func gantryAwait(t *testing.T, what string, check func() bool) {
	t.Helper()

	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if check() {
			return
		}

		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("timed out: " + what)
}

func gantryWrite(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type (
	gantryHit struct {
		node                           int
		method, path, auth, rangeValue string
	}
	gantryObject struct {
		data             []byte
		mediaType        string
		corrupt, noRange bool
	}
	gantryFixture struct {
		t                              *testing.T
		dir                            string
		objects                        map[string]gantryObject
		mu                             sync.Mutex
		hits                           []gantryHit
		offline                        atomic.Bool
		privateRegistry                atomic.Bool
		mirrors, metrics, racerMetrics []string
		agents, racers                 []*gantryProcess
		agentConfigs                   []string
		racerEnvs                      [][]string
		client                         *http.Client
		containerd                     *containerd.Client
		controlState                   *gantryControl
	}
)

func newGantryFixture(t *testing.T, nodes int, objects map[string]gantryObject, registryEndpoint ...func(*gantryFixture) string) *gantryFixture {
	t.Helper()

	dir := os.Getenv("GANTRY_RACER_DIR")

	t.Cleanup(func() {
		uid, _ := strconv.Atoi(os.Getenv("GANTRY_RACER_UID"))
		gid, _ := strconv.Atoi(os.Getenv("GANTRY_RACER_GID"))

		if err := filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			return os.Lchown(path, uid, gid)
		}); err != nil {
			t.Error(err)
		}
	})

	dev := filepath.Join(dir, "dev")
	if err := os.MkdirAll(filepath.Join(dev, "racer"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}

	for name, minor := range map[string]uint32{"null": 3, "zero": 5, "random": 8, "urandom": 9} {
		if err := unix.Mknod(filepath.Join(dev, name), unix.S_IFCHR|0o666, int(unix.Mkdev(1, minor))); err != nil {
			t.Fatal(err)
		}
	}

	if err := unix.Mount(dev, "/dev", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("loopback: %v %s", err, out)
	}

	f := &gantryFixture{t: t, dir: dir, objects: cloneGantryObjects(objects), client: &http.Client{Timeout: 15 * time.Second}}
	t.Cleanup(f.client.CloseIdleConnections)

	cdConfig := filepath.Join(dir, "containerd.toml")
	gantryWrite(t, cdConfig, fmt.Appendf(nil, "version = 2\nroot = %q\nstate = %q\ndisabled_plugins = [\"io.containerd.grpc.v1.cri\", \"io.containerd.cri.v1.images\", \"io.containerd.cri.v1.runtime\", \"io.containerd.nri.v1.nri\"]\n[grpc]\naddress = \"/dev/racer/containerd\"\n[plugins.\"io.containerd.internal.v1.opt\"]\npath = %q\n", filepath.Join(dir, "content"), filepath.Join(dir, "state"), filepath.Join(dir, "opt")))
	gantryStart(t, dir, "containerd", nil, "containerd", "--config", cdConfig)

	var err error

	f.containerd, err = containerd.New("/dev/racer/containerd")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { f.containerd.Close() })
	gantryAwait(t, "containerd", func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		ok, _ := f.containerd.IsServing(ctx)

		return ok
	})
	// Fixed ports are isolated by the private network namespace and stay outside
	// the ephemeral range used by fixture servers and libp2p.
	for i := 0; i < nodes; i++ {
		f.mirrors = append(f.mirrors, fmt.Sprintf("127.0.0.1:%d", 15000+i))
		f.metrics = append(f.metrics, fmt.Sprintf("127.0.0.1:%d", 16000+i))
		f.racerMetrics = append(f.racerMetrics, fmt.Sprintf("127.0.0.1:%d", 17000+i))
	}

	enroll := f.control(nodes)

	var externalRegistry string
	if len(registryEndpoint) != 0 {
		externalRegistry = registryEndpoint[0](f)
	}

	for i := 0; i < nodes; i++ {
		origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.registry(i, w, r) }))
		t.Cleanup(origin.Close)

		ca := filepath.Join(dir, fmt.Sprintf("registry-%d.pem", i))
		gantryWrite(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw}))

		cfg := config.NewDefault()
		cfg.ContentBackend = "racer"
		cfg.RacerCacheName = fmt.Sprintf("node%d", i)
		cfg.ContainerdSocket = "/dev/racer/containerd"
		cfg.ContainerdNamespace = fmt.Sprintf("node%d", i)
		cfg.MirrorListen = f.mirrors[i]
		cfg.MetricsListen = f.metrics[i]
		cfg.PprofListen = ""
		cfg.Libp2pListen = []string{"/ip4/127.0.0.1/tcp/0"}
		cfg.Libp2pIdentityPath = filepath.Join(dir, fmt.Sprintf("identity%d", i))

		cfg.UpstreamRegistries = []config.UpstreamRegistry{{Name: "fixture.test", Endpoint: origin.URL}}
		if externalRegistry != "" {
			cfg.UpstreamRegistries[0].Endpoint = externalRegistry
		}

		cfg.PeerFetchTimeout = 15 * time.Second

		wire, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(dir, fmt.Sprintf("gantry%d.yaml", i))
		gantryWrite(t, path, wire)
		f.agentConfigs = append(f.agentConfigs, path)
		f.agents = append(f.agents, gantryStart(t, dir, fmt.Sprintf("gantry%d", i), []string{"SSL_CERT_FILE=" + ca}, os.Getenv("GANTRY_BINARY"), "agent", "--config", path))
		env := append([]string{}, enroll[i]...)
		env = append(env, "RACER_UNIVERSE="+strings.Repeat("01", 32), "RACER_NODE="+gantryNode(i),
			"RACER_SLAB_PATH="+filepath.Join(dir, fmt.Sprintf("cache%d.slab", i)), "RACER_SLAB_SIZE=536870912", "RACER_SHARDS=1",
			"RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=4", "RACER_RDMA_MODE=disabled", "RACER_METRICS_ADDR="+f.racerMetrics[i], fmt.Sprintf("RACER_POD_IP=127.0.0.%d", i+2))
		f.racerEnvs = append(f.racerEnvs, env)
		f.racers = append(f.racers, gantryStart(t, dir, fmt.Sprintf("racer%d", i), env, os.Getenv("RACER_DATAPLANE_BINARY")))
	}

	t.Cleanup(func() {
		if t.Failed() {
			for _, address := range f.racerMetrics {
				for _, path := range []string{"/readyz", "/metrics"} {
					r, e := f.client.Get("http://" + address + path)
					if e == nil {
						b, _ := io.ReadAll(r.Body)
						r.Body.Close()
						t.Logf("%s%s: %s", address, path, b)
					}
				}
			}
		}
	})

	for _, address := range f.metrics {
		gantryAwait(t, "Gantry readiness "+address, func() bool {
			r, e := f.client.Get("http://" + address + "/readyz")
			if e != nil {
				return false
			}

			r.Body.Close()

			return r.StatusCode == 200
		})
	}

	f.awaitCacheRevision(1)

	return f
}

func gantryNode(i int) string { return strings.Repeat(fmt.Sprintf("%02x", i+2), 32) }

func (f *gantryFixture) control(nodes int) [][]string {
	t := f.t

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Gantry fixture root"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}

	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}

	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}

	rootDigest := sha256.Sum256(rootDER)
	bundle := racermeta.TrustBundle{Version: 1, Generation: 1, Active: hex.EncodeToString(rootDigest[:]), Certificates: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))}
	// Test-only issuance signs the daemon's own CSR with the Rust CA wire identity.
	issue := func(public any, uri string, client bool) ([]byte, error) {
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
		if err != nil {
			return nil, err
		}

		identity, err := url.Parse(uri)
		if err != nil {
			return nil, err
		}

		leaf := &x509.Certificate{SerialNumber: serial, NotBefore: root.NotBefore, NotAfter: root.NotAfter, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{identity}}
		if client {
			leaf.ExtKeyUsage = append(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		} else {
			leaf.DNSNames = []string{"racer-controlplane.gantry-e2e.svc"}
		}

		return x509.CreateCertificate(rand.Reader, leaf, root, public, rootKey)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	serverClaims := fixture.Claims{Version: 1, Namespace: "gantry-e2e", Identity: fixture.CertificateIdentity{Kind: "controlplane", PodUID: "controller", BootID: strings.Repeat("04", 32), PodName: "controller"}}

	serverDER, err := issue(key.Public(), serverClaims.URI(), false)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(root)

	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		t.Fatal(err)
	}

	tokenValue := hex.EncodeToString(token)
	configs := map[string]*pb.Configuration{}
	nodeClaims := func(i int, boot string) fixture.Claims {
		return fixture.Claims{Version: 1, Namespace: "gantry-e2e", Identity: fixture.CertificateIdentity{Kind: "node", Universe: strings.Repeat("01", 32), Node: gantryNode(i), PodUID: fmt.Sprintf("pod%d", i), BootID: boot, PodName: fmt.Sprintf("node%d", i)}}
	}

	for i := 0; i < nodes; i++ {
		node, _ := hex.DecodeString(gantryNode(i))
		algorithm, catalog := uint32(1), uint32(0)
		v := &pb.Volume{Id: "gantry", CacheGeneration: 1, CacheSocket: fmt.Sprintf("/dev/racer/node%d/cache", i), OriginSocket: fmt.Sprintf("/dev/racer/node%d/origin", i), MemberCatalog: &catalog, PeerEndpoints: &pb.VolumePeerEndpoints{}, Topology: &pb.Topology{RoutingAlgorithm: &algorithm, Epoch: 1, SlotCount: uint32(nodes), LocalSlots: []uint32{uint32(i)}}}
		s := &pb.Snapshot{Universe: bytes.Repeat([]byte{1}, 32), Node: node, Revision: 1, Epoch: 1, Volumes: []*pb.Volume{v}, MemberCatalogs: []*pb.MemberCatalog{{}}}

		attempts := uint32(min(nodes, 3))
		v.MaxCandidateAttempts = &attempts
		v.Topology.Product = &pb.ProductTopology{LeftFactor: 1, RightFactor: uint32(nodes), LocalMember: uint32(i), CandidateWidth: attempts}

		for j := 0; j < nodes; j++ {
			id := gantryNode(j)
			memberNode, _ := hex.DecodeString(id)
			v.Topology.Product.Members = append(v.Topology.Product.Members, id)

			v.Topology.Product.Roles = append(v.Topology.Product.Roles, uint32(j))
			for k := range attempts {
				v.Topology.Product.Candidates = append(v.Topology.Product.Candidates, (uint32(j)+k)%uint32(nodes))
			}

			s.MemberCatalogs[0].Members = append(s.MemberCatalogs[0].Members, &pb.Member{Node: memberNode, PodUid: fmt.Sprintf("pod%d", j)})
			if i == j {
				continue
			}

			s.Peers = append(s.Peers, &pb.Peer{Id: id, HttpAddress: fmt.Sprintf("127.0.0.%d:9443", j+2), PodUid: fmt.Sprintf("pod%d", j)})
			v.PeerEndpoints.Peers = append(v.PeerEndpoints.Peers, &pb.VolumePeerEndpoint{Peer: id, HttpAddress: fmt.Sprintf("127.0.0.%d:9443", j+2)})
			v.Peers = append(v.Peers, id)
		}

		configs[gantryNode(i)] = &pb.Configuration{Contents: &pb.Configuration_Snapshot{Snapshot: s}}
	}

	f.controlState = &gantryControl{configs: configs, changed: make(chan struct{})}

	authenticatedNode := func(r *http.Request) string {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates[0].URIs) != 1 {
			return ""
		}

		claims, err := fixture.ParseClaims(r.TLS.PeerCertificates[0].URIs[0].String())
		if err != nil {
			return ""
		}

		for i := range nodes {
			if claims == nodeClaims(i, claims.Identity.BootID) {
				return gantryNode(i)
			}
		}

		return ""
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		node := authenticatedNode(r)
		if node == "" {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}

		boot, err := hex.DecodeString(r.Header.Get("X-Racer-Boot"))
		if err != nil || len(boot) != 32 {
			http.Error(w, "invalid boot", http.StatusBadRequest)
			return
		}

		claims, _ := fixture.ParseClaims(r.TLS.PeerCertificates[0].URIs[0].String())
		if claims.Identity.BootID != r.Header.Get("X-Racer-Boot") {
			http.Error(w, "signed boot mismatch", http.StatusForbidden)
			return
		}

		deadline := time.NewTimer(28 * time.Second)
		defer deadline.Stop()

		for {
			// Published configurations are immutable. Capture the wakeup channel
			// with the snapshot so a concurrent bump cannot be lost.
			f.controlState.mu.Lock()
			c, changed := f.controlState.configs[node], f.controlState.changed
			f.controlState.mu.Unlock()

			s := c.GetSnapshot()
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(s)
			sum := sha256.Sum256(raw)
			cursor := hex.EncodeToString(sum[:])

			if r.Header.Get("X-Racer-Cursor") != cursor {
				body, _ := proto.Marshal(&pb.DesiredState{Universe: s.Universe, Node: s.Node, Incarnation: boot, SnapshotDigest: sum[:], Revision: s.Revision, Configuration: c, Profile: 1, PodUid: fmt.Sprintf("pod%d", int(s.Node[0])-2), Cursor: cursor})

				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body)

				return
			}

			select {
			case <-r.Context().Done():
				return
			case <-changed:
			case <-deadline.C:
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusNoContent)

				return
			}
		}
	})
	mux.HandleFunc("POST /v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			CSR       string `json:"csr"`
			Namespace string `json:"pod_namespace"`
			Pod       string `json:"pod_name"`
			Universe  string `json:"expected_universe"`
			Node      string `json:"expected_node"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Namespace != "gantry-e2e" || r.Header.Get("Authorization") != "Bearer "+tokenValue {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}

		i, err := strconv.Atoi(strings.TrimPrefix(request.Pod, "node"))
		if err != nil || i < 0 || i >= nodes || request.Pod != fmt.Sprintf("node%d", i) || request.Universe != strings.Repeat("01", 32) || request.Node != gantryNode(i) {
			http.Error(w, "invalid node", http.StatusForbidden)
			return
		}

		block, rest := pem.Decode([]byte(request.CSR))
		if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
			http.Error(w, "invalid CSR", http.StatusBadRequest)
			return
		}

		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			http.Error(w, "invalid CSR signature", http.StatusBadRequest)
			return
		}

		boot, err := hex.DecodeString(r.Header.Get("X-Racer-Boot"))
		if err != nil || len(boot) != 32 || hex.EncodeToString(boot) != r.Header.Get("X-Racer-Boot") {
			http.Error(w, "invalid boot", http.StatusBadRequest)
			return
		}

		leaf, err := issue(csr.PublicKey, nodeClaims(i, hex.EncodeToString(boot)).URI(), true)
		if err != nil {
			t.Error(err)
			http.Error(w, "issuance failed", http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf})), "generation": bundle.Generation, "issuer": bundle.Active})
	})
	mux.HandleFunc("POST /v1/proof", func(w http.ResponseWriter, r *http.Request) {
		if authenticatedNode(r) == "" {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: key}}, ClientCAs: roots, ClientAuth: tls.VerifyClientCertIfGiven}
	server.StartTLS()
	t.Cleanup(server.Close)
	gantryWrite(t, filepath.Join(f.dir, "bundle.json"), bundle.JSON())
	gantryWrite(t, filepath.Join(f.dir, "token"), []byte(tokenValue))

	result := make([][]string, nodes)
	for i := range result {
		result[i] = []string{"RACER_CONTROL_PLANE_URL=" + server.URL + "/v1/config", "RACER_TLS_TRUST_DIR=" + f.dir, "RACER_ENROLL_URL=" + server.URL + "/v1/enroll", "RACER_CONTROL_SERVER_NAME=racer-controlplane.gantry-e2e.svc", "RACER_CONTROL_TOKEN_FILE=" + filepath.Join(f.dir, "token"), "RACER_POD_NAMESPACE=gantry-e2e", fmt.Sprintf("RACER_POD_NAME=node%d", i), fmt.Sprintf("RACER_POD_UID=pod%d", i)}
	}

	return result
}

func (f *gantryFixture) registry(node int, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, gantryHit{node: node, method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), rangeValue: r.Header.Get("Range")})
	obj, ok := f.objects[r.URL.Path]
	f.mu.Unlock()

	if f.offline.Load() {
		http.Error(w, "offline", http.StatusServiceUnavailable)
		return
	}

	if r.URL.Path == "/v2/" {
		if f.privateRegistry.Load() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://fixture.test/token",service="fixture.test"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.WriteHeader(200)

		return
	}

	if !ok {
		http.NotFound(w, r)
		return
	}

	auth := r.Header.Get("Authorization")
	if strings.Contains(r.URL.Path, "/private/") && (auth == "" || auth == "Bearer unauthorized" || auth == "Bearer denied" || auth == "Bearer page-denied" && r.Method == "GET") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://fixture.test/token",service="fixture.test",scope="repository:private:pull"`)

		code := 401
		if auth != "" && auth != "Bearer unauthorized" {
			code = 403
		}

		w.WriteHeader(code)

		return
	}

	data := obj.data
	if obj.corrupt && len(data) != 0 {
		data = bytes.Clone(data)
		data[len(data)/2] ^= 1
	}

	w.Header().Set("Content-Type", obj.mediaType)
	w.Header().Set("Docker-Content-Digest", r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])

	if r.Method == "HEAD" {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		return
	}

	if r.Header.Get("Range") != "" && obj.noRange {
		w.WriteHeader(416)
		return
	}

	if r.Header.Get("Range") != "" && !obj.noRange {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= int64(len(data)) || end < start {
			w.WriteHeader(416)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(206)
		_, _ = w.Write(data[start : end+1])

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (f *gantryFixture) request(node int, method, path, auth, rangeValue string) (*http.Response, []byte, error) {
	r, err := http.NewRequestWithContext(f.t.Context(), method, "http://"+f.mirrors[node]+path+"?ns=fixture.test", nil)
	if err != nil {
		return nil, nil, err
	}

	r.Header.Set("Authorization", auth)
	r.Header.Set("Range", rangeValue)

	resp, err := f.client.Do(r)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	return resp, body, err
}

func (f *gantryFixture) metric(node int, name string) float64 {
	f.t.Helper()
	return f.metricAt(f.metrics[node], name)
}

func (f *gantryFixture) metricAt(address, name string) float64 {
	f.t.Helper()

	resp, err := f.client.Get("http://" + address + "/metrics")
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+" ") {
			value, err := strconv.ParseFloat(strings.TrimPrefix(line, name+" "), 64)
			if err != nil {
				f.t.Fatal(err)
			}

			return value
		}
	}

	return 0
}

func gantryPath(repo, kind string, data []byte) string {
	return fmt.Sprintf("/v2/%s/%s/sha256:%x", repo, kind, sha256.Sum256(data))
}
