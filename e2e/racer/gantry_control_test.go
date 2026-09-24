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
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/e2e/racer/fixture"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func TestGantryControlFixture(t *testing.T) {
	f := &gantryFixture{t: t, dir: t.TempDir()}
	env := map[string]string{}

	for _, entry := range f.control(2)[0] {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}

	wire, err := os.ReadFile(filepath.Join(f.dir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := racermeta.ParseTrustBundle(wire)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundle.Certificates)) {
		t.Fatal("missing fixture CA")
	}

	token, err := os.ReadFile(env["RACER_CONTROL_TOKEN_FILE"])
	if err != nil {
		t.Fatal(err)
	}

	newClient := func(config *tls.Config) *http.Client {
		transport := &http.Transport{TLSClientConfig: config}
		t.Cleanup(transport.CloseIdleConnections)

		return &http.Client{Transport: transport, Timeout: 2 * time.Second}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: env["RACER_CONTROL_SERVER_NAME"]}
	client := newClient(tlsConfig)
	request := func(client *http.Client, method, endpoint, auth, boot string, body []byte, status int) []byte {
		t.Helper()

		r, err := http.NewRequestWithContext(t.Context(), method, endpoint, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", auth)
		r.Header.Set("X-Racer-Boot", boot)

		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("%s %s: status=%d body=%s err=%v", method, endpoint, resp.StatusCode, data, err)
		}

		if status == http.StatusOK && method == http.MethodGet && (resp.ContentLength != int64(len(data)) || len(resp.TransferEncoding) != 0) {
			t.Fatal("desired state must use explicit Content-Length")
		}

		return data
	}
	boot := strings.Repeat("03", 32)
	controlURL := env["RACER_CONTROL_PLANE_URL"]
	proofURL := strings.TrimSuffix(env["RACER_ENROLL_URL"], "enroll") + "proof"

	request(client, "GET", controlURL, "", boot, nil, http.StatusForbidden)
	request(client, "POST", proofURL, "", boot, nil, http.StatusForbidden)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{{Scheme: "spiffe", Host: "racer", Path: "/controlplane"}}}, key)
	if err != nil {
		t.Fatal(err)
	}

	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
	badSignature := bytes.Clone(csr)

	badSignature[len(badSignature)-1] ^= 1
	for _, tc := range []struct {
		name, namespace, pod, token, csr string
		status                           int
	}{
		{"token", "gantry-e2e", "node0", "wrong", csrPEM, 403},
		{"namespace", "wrong", "node0", string(token), csrPEM, 403},
		{"pod", "gantry-e2e", "node2", string(token), csrPEM, 403},
		{"pod-prefix", "gantry-e2e", "0", string(token), csrPEM, 403},
		{"csr-framing", "gantry-e2e", "node0", string(token), csrPEM + "junk", 400},
		{"csr-signature", "gantry-e2e", "node0", string(token), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: badSignature})), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"csr": tc.csr, "pod_namespace": tc.namespace, "pod_name": tc.pod, "expected_universe": strings.Repeat("01", 32), "expected_node": gantryNode(0)})
			request(client, "POST", env["RACER_ENROLL_URL"], "Bearer "+tc.token, boot, body, tc.status)
		})
	}

	for i := range 2 {
		body, _ := json.Marshal(map[string]string{"csr": csrPEM, "pod_namespace": "gantry-e2e", "pod_name": fmt.Sprintf("node%d", i), "expected_universe": strings.Repeat("01", 32), "expected_node": gantryNode(i)})
		data := request(client, "POST", env["RACER_ENROLL_URL"], "Bearer "+string(token), boot, body, 200)

		var issued struct {
			Certificate string `json:"certificate"`
			Generation  uint64 `json:"generation"`
			Issuer      string `json:"issuer"`
		}
		if err := json.Unmarshal(data, &issued); err != nil || issued.Generation != bundle.Generation || issued.Issuer != bundle.Active {
			t.Fatalf("invalid enrollment metadata: %s err=%v", data, err)
		}

		block, _ := pem.Decode([]byte(issued.Certificate))
		if block == nil {
			t.Fatal("missing node certificate")
		}

		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}

		for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
				t.Fatal(err)
			}
		}

		uri := (fixture.Claims{Version: 1, Namespace: "gantry-e2e", Identity: fixture.CertificateIdentity{Kind: "node", Universe: strings.Repeat("01", 32), Node: gantryNode(i), PodUID: fmt.Sprintf("pod%d", i), BootID: boot, PodName: fmt.Sprintf("node%d", i)}}).URI()
		if len(leaf.URIs) != 1 || leaf.URIs[0].String() != uri || !key.PublicKey.Equal(leaf.PublicKey) {
			t.Fatal("issued identity must bind the enrolled pod and CSR key, ignoring requested SANs")
		}

		nodeTLS := tlsConfig.Clone()
		nodeTLS.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}
		nodeClient := newClient(nodeTLS)
		request(nodeClient, "GET", controlURL, "", "bad", nil, 400)
		request(nodeClient, "GET", controlURL, "", strings.Repeat("05", 32), nil, 403)
		data = request(nodeClient, "GET", controlURL, "", boot, nil, 200)

		var desired pb.DesiredState
		if err := proto.Unmarshal(data, &desired); err != nil {
			t.Fatal(err)
		}

		if desired.Configuration.GetSnapshot() == nil {
			t.Fatal("missing desired snapshot")
		}

		raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(desired.Configuration.GetSnapshot())

		digest := sha256.Sum256(raw)
		if hex.EncodeToString(desired.Node) != gantryNode(i) || !bytes.Equal(desired.Universe, bytes.Repeat([]byte{1}, 32)) || hex.EncodeToString(desired.Incarnation) != boot || desired.PodUid != fmt.Sprintf("pod%d", i) || desired.Profile != 1 || desired.Revision != 1 || !bytes.Equal(desired.SnapshotDigest, digest[:]) || desired.Cursor != hex.EncodeToString(digest[:]) {
			t.Fatal("desired state does not bind the authenticated process and snapshot")
		}

		request(nodeClient, "POST", proofURL, "", boot, nil, 204)

		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)

		r, err := http.NewRequestWithContext(ctx, "GET", controlURL, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		r.Header.Set("X-Racer-Boot", boot)
		r.Header.Set("X-Racer-Cursor", desired.Cursor)
		resp, err := nodeClient.Do(r)

		cancel()

		if resp != nil {
			resp.Body.Close()
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unchanged cursor must hold until cancellation: %v", err)
		}

		if i == 1 {
			// Keep a real authenticated long poll outstanding across the bump.
			// Whether publication wins the race with handler entry or not, the
			// captured snapshot/channel pair must deliver the new revision.
			r, err := http.NewRequestWithContext(t.Context(), "GET", controlURL, nil)
			if err != nil {
				t.Fatal(err)
			}

			r.Header.Set("X-Racer-Boot", boot)
			r.Header.Set("X-Racer-Cursor", desired.Cursor)

			var wg sync.WaitGroup
			wg.Go(func() {
				resp, err := nodeClient.Do(r)
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()

				data, err := io.ReadAll(resp.Body)

				var next pb.DesiredState
				if err != nil || resp.StatusCode != 200 || proto.Unmarshal(data, &next) != nil {
					t.Errorf("updated desired state: status=%d err=%v", resp.StatusCode, err)
					return
				}

				raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(next.Configuration.GetSnapshot())

				digest := sha256.Sum256(raw)
				if next.Revision != 2 || next.Configuration.GetSnapshot().Volumes[0].CacheGeneration != 2 || next.Cursor == desired.Cursor || next.Cursor != hex.EncodeToString(digest[:]) || !bytes.Equal(next.SnapshotDigest, digest[:]) {
					t.Error("generation bump did not bind a new revision and snapshot digest")
				}
			})
			time.Sleep(25 * time.Millisecond)

			if revision, err := f.advanceCacheGeneration("gantry"); err != nil || revision != 2 {
				t.Fatalf("advance generation: revision=%d err=%v", revision, err)
			}

			wg.Wait()
		}
	}

	for _, name := range []string{"untrusted-root", "wrong-server-name"} {
		t.Run(name, func(t *testing.T) {
			config := tlsConfig.Clone()
			if name == "untrusted-root" {
				config.RootCAs = x509.NewCertPool()
			} else {
				config.ServerName = "wrong.invalid"
			}

			resp, err := newClient(config).Get(controlURL)
			if resp != nil {
				resp.Body.Close()
			}

			if err == nil {
				t.Fatal("control TLS accepted invalid server trust")
			}
		})
	}
}

func TestGantryRecoveryFixture(t *testing.T) {
	t.Run("atomic-generation-publication", func(t *testing.T) {
		configs := map[string]*pb.Configuration{}
		for i := range 2 {
			configs[gantryNode(i)] = &pb.Configuration{Contents: &pb.Configuration_Snapshot{Snapshot: &pb.Snapshot{Revision: 7, Volumes: []*pb.Volume{{Id: "gantry", CacheGeneration: 3}, {Id: "other", CacheGeneration: 5}, {Id: "untouched", CacheGeneration: 9}}}}}
		}

		f := &gantryFixture{controlState: &gantryControl{configs: configs, changed: make(chan struct{})}}

		old := f.controlState.changed
		for _, ids := range [][]string{nil, {"missing"}, {"gantry", "missing"}, {"gantry", "gantry"}} {
			if _, err := f.advanceCacheGeneration(ids...); err == nil {
				t.Fatalf("accepted invalid selection %v", ids)
			}
		}

		select {
		case <-old:
			t.Fatal("invalid update woke watchers")
		default:
		}

		if revision, err := f.advanceCacheGeneration("gantry", "other"); err != nil || revision != 8 {
			t.Fatalf("advance: revision=%d err=%v", revision, err)
		}

		select {
		case <-old:
		default:
			t.Fatal("publication did not wake watchers")
		}

		for node, original := range configs {
			s := f.controlState.configs[node].GetSnapshot()
			if original.GetSnapshot().Revision != 7 || original.GetSnapshot().Volumes[0].CacheGeneration != 3 || s.Revision != 8 || s.Volumes[0].CacheGeneration != 4 || s.Volumes[1].CacheGeneration != 6 || s.Volumes[2].CacheGeneration != 9 {
				t.Fatal("publication mutated old snapshots or wrong volumes")
			}
		}

		if revision, err := f.advanceCacheGeneration("gantry"); err != nil || revision != 9 {
			t.Fatalf("second advance: revision=%d err=%v", revision, err)
		}
	})

	t.Run("reject-partial-and-exhausted-updates", func(t *testing.T) {
		for _, tc := range []struct {
			name                 string
			revision, generation uint64
			volume               string
		}{
			{"missing-on-one-node", 1, 1, "other"},
			{"generation-overflow", 1, math.MaxInt64, "gantry"},
			{"revision-overflow", math.MaxUint64, 1, "gantry"},
			{"inconsistent-revision", 2, 1, "gantry"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				configs := map[string]*pb.Configuration{}

				for i := range 2 {
					s := &pb.Snapshot{Revision: 1, Volumes: []*pb.Volume{{Id: "gantry", CacheGeneration: 1}}}
					if i == 1 {
						s.Revision, s.Volumes[0].Id, s.Volumes[0].CacheGeneration = tc.revision, tc.volume, tc.generation
					}

					configs[gantryNode(i)] = &pb.Configuration{Contents: &pb.Configuration_Snapshot{Snapshot: s}}
				}

				f := &gantryFixture{controlState: &gantryControl{configs: configs, changed: make(chan struct{})}}
				if _, err := f.advanceCacheGeneration("gantry"); err == nil {
					t.Fatal("accepted invalid update")
				}

				for node, original := range configs {
					if f.controlState.configs[node] != original || configs[gantryNode(0)].GetSnapshot().Revision != 1 {
						t.Fatal("failed update partially published")
					}
				}
			})
		}
	})

	t.Run("activation-not-offer", func(t *testing.T) {
		var secondActive atomic.Bool

		f := &gantryFixture{client: &http.Client{}, controlState: &gantryControl{configs: map[string]*pb.Configuration{"a": nil, "b": nil}}}

		for i := range 2 {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status" {
					t.Errorf("activation queried %s", r.URL.Path)
				}

				active, state, workers := 1, "preparing", 1
				if i == 0 || secondActive.Load() {
					active, state, workers = 2, "applied", 2
				}

				_ = json.NewEncoder(w).Encode(map[string]any{"activeRevision": active, "candidateRevision": 2, "ready": true, "localState": state, "workers": 2, "activatedWorkers": workers})
			}))
			t.Cleanup(server.Close)
			f.racerMetrics = append(f.racerMetrics, strings.TrimPrefix(server.URL, "http://"))
		}

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		err := f.waitCacheRevision(ctx, 2)

		cancel()

		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), f.racerMetrics[1]) {
			t.Fatalf("must wait for every active node with diagnostics: %v", err)
		}

		secondActive.Store(true)

		if err := f.waitCacheRevision(t.Context(), 2); err != nil {
			t.Fatal(err)
		}

		f.racerMetrics = f.racerMetrics[:1]
		if err := f.waitCacheRevision(t.Context(), 2); err == nil {
			t.Fatal("accepted missing dataplane status endpoint")
		}
	})

	t.Run("origin-repair-and-counts", func(t *testing.T) {
		data := []byte("good bytes")
		path := gantryPath("public", "blobs", data)
		objects := map[string]gantryObject{path: {data: data, corrupt: true}}
		f := &gantryFixture{objects: cloneGantryObjects(objects)}
		get := func() []byte {
			w := httptest.NewRecorder()
			f.registry(0, w, httptest.NewRequest("GET", path, nil))

			return w.Body.Bytes()
		}

		corrupt := get()
		if bytes.Equal(corrupt, data) || len(corrupt) != len(data) {
			t.Fatal("fixture did not corrupt same-length bytes")
		}

		if err := f.setOriginObject("missing", gantryObject{}); err == nil {
			t.Fatal("repair accepted unknown path")
		}

		if err := f.setOriginObject(path, gantryObject{data: data}); err != nil {
			t.Fatal(err)
		}

		data[0] ^= 1 // Caller mutation cannot change published bytes.

		if !bytes.Equal(get(), []byte("good bytes")) {
			t.Fatal("repair did not publish owned good bytes")
		}

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if err := f.setOriginObject(path, gantryObject{data: []byte("good bytes")}); err != nil {
					t.Error(err)
				}

				if !bytes.Equal(get(), []byte("good bytes")) {
					t.Error("concurrent repair returned inconsistent bytes")
				}

				_ = f.originRequestCount("GET", path)
			})
		}

		wg.Wait()
		f.registry(1, httptest.NewRecorder(), httptest.NewRequest("HEAD", path, nil))
		f.offline.Store(true)
		get()

		if f.originRequestCount("GET", path) != 23 || f.originRequestCount("HEAD", path) != 1 || f.originRequestCount("", "") != 24 || f.originRequestCount("GET", "missing") != 0 {
			t.Fatal("incorrect origin request counts")
		}
	})

	t.Run("activation-status-validation", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			field string
			value any
		}{
			{"old-active", "activeRevision", 1},
			{"superseded", "candidateRevision", 3},
			{"not-ready", "ready", false},
			{"rejected", "rejected", true},
			{"committing", "localState", "committing"},
			{"partial-workers", "activatedWorkers", 1},
			{"no-workers", "workers", 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				status := map[string]any{"activeRevision": 2, "candidateRevision": 2, "ready": true, "rejected": false, "localState": "applied", "workers": 2, "activatedWorkers": 2}
				status[tc.field] = tc.value

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(status)
				}))
				defer server.Close()

				f := &gantryFixture{client: server.Client()}
				if err := f.cacheRevisionStatus(t.Context(), strings.TrimPrefix(server.URL, "http://"), 2); err == nil {
					t.Fatal("accepted incomplete activation")
				}
			})
		}
	})
}
