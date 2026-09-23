// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

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
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Coordination barriers, command validation and mutual-TLS heartbeats.
type coordinationPKI struct {
	root *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func (p *coordinationPKI) leaf(t *testing.T, uri, dns string) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}

	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{u}}
	if dns != "" {
		leaf.DNSNames = []string{dns}
	}

	der, err := x509.CreateCertificate(rand.Reader, leaf, p.root, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}

	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}

func (p *coordinationPKI) directory(t *testing.T, node, pod string) string {
	t.Helper()
	dir := t.TempDir()
	cert, key := p.leaf(t, "spiffe://racer/universe/"+identity("universe", "default")+"/node/"+node+"/pod/"+pod, "")
	digest := sha256.Sum256(p.root.Raw)

	bundle, err := json.Marshal(map[string]any{"version": 1, "generation": 1, "active": hex.EncodeToString(digest[:]), "certificates": string(p.pem)})
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{"ca.pem": p.pem, "tls.crt": cert, "tls.key": key, "bundle.json": bundle} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func coordinationServer(t *testing.T, handler http.Handler) (*httptest.Server, *coordinationPKI) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "coordination root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	p := &coordinationPKI{root: root, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	cert, private := p.leaf(t, "spiffe://racer/controlplane", "localhost")

	pair, err := tls.X509KeyPair(cert, private)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(root)

	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	s.StartTLS()
	s.URL = strings.Replace(s.URL, "127.0.0.1", "localhost", 1)

	return s, p
}

// Reusable cross-language fixture: Go's actual control handler and durable
// store talk HTTPS to Rust's production Subscriber and two production Volumes
// workers. The Rust executable is a prebuilt lib-test binary, not a wire mock.
// Certificates are issued by a test CA with production identity URI SANs.
func TestB14ProductionCoordination(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN to the Rust lib-test executable")
	}

	for _, mode := range []string{"lost", "lost-read", "create-lost", "no-commit", "conflict", "universe", "node", "boot", "revision", "digest", "status-revision", "status-digest"} {
		t.Run(mode, func(t *testing.T) { runCoordination(t, bin, mode) })
	}
}

func TestB16ProductionHeartbeatTLS(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN to the Rust lib-test executable")
	}

	runCoordination(t, bin, "heartbeat")
}

func TestStorageProductionCoordination(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN to the Rust lib-test executable")
	}

	runCoordination(t, bin, "storage")
}

func runCoordination(t *testing.T, bin, mode string) {
	t.Helper()
	f := newCoordinationFixture(t, nil)

	var (
		mu      sync.Mutex
		problem error
	)

	released, faulted, tampered := false, false, 0
	seen := map[uint32]bool{}
	boot := ""
	heartbeat := mode == "heartbeat"
	storage := mode == "storage"
	storageDelivered, storageApplied := false, false
	storageController := newStorageTest(t, f.api, f.s)

	var previous, firstRetired, lastRetired time.Time

	requests := 0
	heldCancelled := false
	setProblem := func(err error) {
		if problem == nil {
			problem = err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /release", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		released = true

		w.Header().Set("Content-Length", "0")
	})
	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if req.Header.Get("Prefer") != "wait=0" {
			setProblem(fmt.Errorf("coordinated Subscriber must use deliberate heartbeat mode: %q", req.Header.Get("Prefer")))
		}

		now := time.Now()
		if !previous.IsZero() && (now.Sub(previous) < 100*time.Millisecond || now.Sub(previous) >= 15*time.Second) {
			setProblem(fmt.Errorf("poll cadence violates load/freshness bound: %s", now.Sub(previous)))
		}

		previous = now
		requests++

		if boot == "" {
			boot = req.Header.Get("X-Racer-Boot")
		}

		if boot != req.Header.Get("X-Racer-Boot") || len(boot) != 64 {
			setProblem(fmt.Errorf("boot identity changed"))
		}

		ack, _ := strconv.ParseUint(req.Header.Get("X-Racer-Phase"), 10, 32)
		if ack != 0 {
			seen[uint32(ack)] = true

			if req.Header.Get("X-Racer-Digest") == "" {
				setProblem(fmt.Errorf("ack without digest"))
			}
		}

		if ack == 4 {
			if storage && !storageDelivered {
				if _, err := storageController.Reconcile(req.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}}); err != nil {
					setProblem(err)
				}
			}

			if firstRetired.IsZero() {
				firstRetired = now
			}

			lastRetired = now
		}

		apiFault := mode == "lost" || mode == "lost-read" || mode == "create-lost" || mode == "no-commit" || mode == "conflict"
		if apiFault && (ack == 1 || mode == "create-lost") && !faulted {
			f.api.fault = mode
			if mode == "create-lost" {
				f.api.fault = "lost"
			}

			faulted = true
		}

		recorded := httptest.NewRecorder()
		f.s.control(recorded, req)

		if storage && req.Header.Get("X-Racer-Storage-State") == "applied" {
			key := recipient{identityBytes("universe", "default"), identityBytes("node", "node-uid")}
			storageApplied = f.s.storageReports[key].State == "applied"
		}

		body := recorded.Body.Bytes()
		if recorded.Code == 200 {
			var command pb.ControlCommand
			if err := proto.Unmarshal(body, &command); err != nil {
				setProblem(err)
				return
			}

			cm := f.durable(t)
			if cm.Data["phase"] != strconv.Itoa(int(command.Phase)) || cm.Data["revision"] != strconv.FormatUint(command.Revision, 10) {
				setProblem(fmt.Errorf("command preceded durability"))
			}

			if command.Configuration != nil {
				snapshot := configurationSnapshot(t, command.Configuration)

				hash := sha256.Sum256(snapshot)
				if hex.EncodeToString(hash[:]) != hex.EncodeToString(command.SnapshotDigest) {
					setProblem(fmt.Errorf("snapshot byte identity mismatch"))
				}
			} else if req.Header.Get("X-Racer-Digest") != hex.EncodeToString(command.SnapshotDigest) {
				setProblem(fmt.Errorf("ack digest mismatch"))
			}

			statusMode := mode == "status-revision" || mode == "status-digest"

			if storage && command.StoragePolicy != nil {
				storageDelivered = true

				if command.Configuration != nil || command.Revision != 1 || command.Phase != 4 {
					setProblem(fmt.Errorf("storage policy changed steady topology"))
				}
			}

			if !storage && !heartbeat && !apiFault && !released && (!statusMode || command.Configuration == nil) {
				switch mode {
				case "universe":
					command.Universe[0] ^= 1
				case "node":
					command.Node[0] ^= 1
				case "boot":
					command.Incarnation[0] ^= 1
				case "revision", "status-revision":
					command.Revision++
				case "digest", "status-digest":
					command.SnapshotDigest[0] ^= 1
				}

				body, _ = proto.Marshal(&command)
				tampered++
			}
		}

		if heartbeat && ack == 1 && recorded.Code == 200 && !heldCancelled {
			// Hold a command after its durable decision. /v3
			// must cancel silence and resend actual worker feedback within the
			// freshness budget, rather than inherit snapshot wait=60 semantics.
			select {
			case <-req.Context().Done():
				heldCancelled = true
			case <-time.After(4 * time.Second):
				setProblem(fmt.Errorf("coordinated request ignored first-byte bound"))
			}

			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(body)
	})

	httpServer, pki := coordinationServer(t, mux)
	defer httpServer.Close()

	dir := pki.directory(t, f.node, "pod-uid")

	limit := 12 * time.Second
	if heartbeat {
		limit = 28 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "coordination_tests::production_coordination_child", "--ignored", "--nocapture", "--test-threads=1")

	cmd.Env = append(os.Environ(),
		"RACER_TLS_DIR="+dir,
		"RACER_COORDINATION_MODE="+mode,
		"RACER_CONTROL_PLANE_URL="+httpServer.URL+"/v3/"+identity("universe", "default")+"/"+f.node,
		"RACER_UNIVERSE="+identity("universe", "default"), "RACER_NODE="+f.node,
		"RACER_POD_UID=pod-uid")
	output, err := cmd.CombinedOutput()
	t.Logf("Rust %s: %s", mode, output)

	if err != nil {
		t.Fatalf("Rust worker/subscriber: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if problem != nil {
		t.Fatal(problem)
	}

	for phase := uint32(1); phase <= 4; phase++ {
		if !seen[phase] {
			t.Fatalf("actual worker ack %d missing", phase)
		}
	}

	if f.api.hits != 1 && faulted {
		t.Fatal("API fault did not fire")
	}

	if !storage && !heartbeat && !faulted && (tampered == 0 || !released) {
		t.Fatal("verification fault not observed/released")
	}

	if storage && (!storageDelivered || !storageApplied) {
		t.Fatal("cross-language storage delivery/report missing")
	}

	if heartbeat {
		if !heldCancelled || lastRetired.Sub(firstRetired) < 15*time.Second || requests > 100 {
			t.Fatalf("heartbeat oracle missing: retired span=%s requests=%d", lastRetired.Sub(firstRetired), requests)
		}

		t.Logf("real worker phase-4 TLS heartbeat span=%s, requests=%d", lastRetired.Sub(firstRetired), requests)
	}

	if f.durable(t).Data["phase"] != "4" {
		t.Fatal("coordination did not retire")
	}
}

// Removal catch-up across missed responses, controller restarts and topology GC.

// The schedule advances only on Subscriber headers derived from real workers.
// R=2 is empty/excluded; its phase-3 response is discarded before delivery.
func TestB13ProductionCatchup(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN")
	}

	for _, phase := range []uint32{2, 3} {
		t.Run(fmt.Sprint(phase), func(t *testing.T) { runCatchup(t, bin, phase) })
	}
}

func (f *coordinationFixture) generation(t *testing.T, selected bool) {
	t.Helper()

	n, p, svc := fixtures()
	p.UID = "pod-uid"
	svc.Annotations[originPortAnnotation] = "8080"

	var services []corev1.Service
	if selected {
		services = []corev1.Service{*svc}
	}

	g, _, err := buildGeneration("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, services)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision++

	_, pointer, err := f.s.controlStore.load(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.controlStore.commit(context.Background(), g, pointer); err != nil {
		t.Fatal(err)
	}

	f.index, err = indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.install(f.index); err != nil {
		t.Fatal(err)
	}
}

func runCatchup(t *testing.T, bin string, trap uint32) {
	f := newCoordinationFixture(t, nil)

	var mu sync.Mutex

	stage := 0
	lost := 0
	digests := map[string]uint64{}
	seen := map[string]bool{}

	var oldChunks []string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		ack, _ := strconv.ParseUint(req.Header.Get("X-Racer-Phase"), 10, 32)
		rev := digests[req.Header.Get("X-Racer-Digest")]

		seen[fmt.Sprintf("%d/%d", rev, ack)] = true
		if stage == 0 && rev == 1 && ack == 4 {
			f.generation(t, false)

			_, pointer, err := f.s.controlStore.load(req.Context(), "default")
			if err != nil {
				t.Error(err)
			}

			var m manifest
			if err := json.Unmarshal([]byte(pointer.Data["manifest"]), &m); err != nil {
				t.Error(err)
			}

			oldChunks = m.Chunks
			stage = 1
		}

		if stage == 1 && rev == 2 && ack >= uint64(trap) {
			// Real controller completes its excluded-only barrier, then misses
			// delivery while three durable generations and GC intervene.
			if busy, err := f.s.rolloutBusy(req.Context(), f.index); err != nil || busy {
				t.Errorf("empty rollout: %v %v", busy, err)
			}

			for i := 0; i < 2; i++ {
				f.generation(t, false)

				if busy, err := f.s.rolloutBusy(req.Context(), f.index); err != nil || busy {
					t.Errorf("intervening rollout: %v %v", busy, err)
				}
			}

			f.generation(t, true)

			for _, name := range oldChunks {
				if err := f.api.Client.Get(req.Context(), client.ObjectKey{Namespace: "state", Name: name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
					t.Errorf("old topology not GC'd: %v", err)
				}
			}
			// Restart only the controller, retaining the Rust process at R.
			f.s = &Server{controlStore: f.s.controlStore}
			if err := f.s.install(f.index); err != nil {
				t.Error(err)
			}

			stage = 2
		}

		rr := httptest.NewRecorder()
		f.s.control(rr, req)

		body := rr.Body.Bytes()
		if rr.Code == 200 {
			var c pb.ControlCommand
			if err := proto.Unmarshal(body, &c); err != nil {
				t.Error(err)
			}

			digests[hex.EncodeToString(c.SnapshotDigest)] = c.Revision
			if c.Configuration != nil {
				hash := sha256.Sum256(configurationSnapshot(t, c.Configuration))
				if hex.EncodeToString(hash[:]) != hex.EncodeToString(c.SnapshotDigest) {
					t.Error("snapshot digest mismatch")
				}
			}

			cm := f.durable(t)
			if c.Revision == f.index.g.Revision {
				if cm.Data["phase"] != fmt.Sprint(c.Phase) {
					t.Error("command preceded persistence")
				}
			} else {
				entries, err := removalHistory(cm.Data["removals"], "default", f.index.g.Revision)
				if err != nil {
					t.Error(err)
				}

				found := false

				for _, d := range entries {
					hash := sha256.Sum256(d.Snapshot)
					if hex.EncodeToString(hash[:]) == hex.EncodeToString(c.SnapshotDigest) && d.Phase == c.Phase && d.Boot == req.Header.Get("X-Racer-Boot") {
						found = true
					}
				}

				if !found {
					t.Error("historical command preceded persistence")
				}
			}

			if stage == 2 && c.Revision == 2 && c.Phase == 4 && lost < 2 {
				// Drop two actually authorized terminal responses. A later poll
				// must still recover the same decision and exact snapshot digest.
				lost++

				w.Header().Set("Content-Length", "0")
				w.WriteHeader(503)

				return
			}
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rr.Code)
		_, _ = w.Write(body)
	})

	httpServer, pki := coordinationServer(t, mux)
	defer httpServer.Close()

	dir := pki.directory(t, f.node, "pod-uid")

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "coordination_tests::production_catchup_child", "--ignored", "--nocapture", "--test-threads=1")

	cmd.Env = append(os.Environ(), "RACER_TLS_DIR="+dir, "RACER_CONTROL_PLANE_URL="+httpServer.URL+"/v3/"+identity("universe", "default")+"/"+f.node,
		"RACER_UNIVERSE="+identity("universe", "default"), "RACER_NODE="+f.node, "RACER_POD_UID=pod-uid",
		fmt.Sprintf("RACER_CATCHUP_TRAP=%d", trap))
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)

	if err != nil {
		t.Fatalf("TLS catchup: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if lost != 2 {
		t.Errorf("lost terminal response schedule fired %d times", lost)
	}

	for _, key := range []string{"1/4", fmt.Sprintf("2/%d", trap), "5/1", "5/2", "5/3", "5/4"} {
		if !seen[key] {
			t.Errorf("missing actual worker acknowledgment %s", key)
		}
	}
}

// Forward recovery with real workers, corrected Services and replay faults.

// A durable old decision predates this process. The actual workers cannot bind
// its listener; only a corrected Service can restore progress after CP restart.
func TestB15ProductionForward(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN")
	}

	for _, phase := range []uint32{1, 2, 3, 4} {
		for _, mode := range []string{"bind", "backend", "survivor", "bad-digest", "bad-revision", "bad-pod", "replay"} {
			if mode == "replay" && phase != 2 {
				continue
			}

			if (mode == "bad-digest" || mode == "bad-revision" || mode == "bad-pod") && phase != 2 {
				continue
			}

			if mode == "survivor" && phase == 1 {
				continue
			}

			t.Run(fmt.Sprintf("%d/%s", phase, mode), func(t *testing.T) {
				f := newCoordinationFixture(t, nil)
				if mode == "backend" || mode == "replay" {
					// Exact legacy Go-accepted/Rust-rejected backend, persisted before restart.
					f.index.g.Volume.Origin.IPv4 = "HTTP://127.0.0.1:18082"

					_, pointer, err := f.s.controlStore.load(context.Background(), "default")
					if err != nil {
						t.Fatal(err)
					}

					if err = f.s.controlStore.commit(context.Background(), f.index.g, pointer); err != nil {
						t.Fatal(err)
					}

					f.index, err = indexGeneration(f.index.g)
					if err != nil {
						t.Fatal(err)
					}

					if err = f.s.install(f.index); err != nil {
						t.Fatal(err)
					}
				}

				r, err := f.s.rolloutFor(context.Background(), f.index)
				if err != nil {
					t.Fatal(err)
				}

				if err := f.s.persistPhase(context.Background(), "default", r, phase); err != nil {
					t.Fatal(err)
				}
				// Restart from only persisted topology/rollout; no cached acknowledgments.
				f.s = &Server{controlStore: f.s.controlStore}
				rec := newTestReconciler(f.api)

				rec.server, rec.store = f.s, f.s.controlStore
				if err := f.s.install(f.index); err != nil {
					t.Fatal(err)
				}

				var mu sync.Mutex

				corrected := false
				lost := 0
				tampered, checked := false, false

				var (
					replay                    []byte
					replayBoot, currentDigest string
				)

				replayed, replayChecked := false, false
				mux := http.NewServeMux()
				mux.HandleFunc("GET /failed", func(w http.ResponseWriter, req *http.Request) {
					mu.Lock()
					defer mu.Unlock()

					_, _, svc := fixtures()
					if mode != "backend" && mode != "replay" {
						if err := f.api.Delete(req.Context(), svc); err != nil {
							t.Error(err)
						}
					} // backend desired Service already contains the corrected URL

					corrected = true
					// Restart again after the actual failure/receive/activation report.
					f.s = &Server{controlStore: f.s.controlStore}
					rec = newTestReconciler(f.api)

					rec.server, rec.store = f.s, f.s.controlStore
					if phase == 1 {
						cm := f.durable(t)

						cm.Data["since"] = time.Now().Add(-24 * time.Hour).Format(time.RFC3339Nano)
						if err := f.api.Client.Update(req.Context(), cm); err != nil {
							t.Error(err)
						}

						f.s.rollouts = nil
					}

					w.WriteHeader(200)
				})
				mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
					mu.Lock()
					defer mu.Unlock()

					if replayed && !replayChecked {
						replayChecked = true

						if req.Header.Get("X-Racer-Digest") != currentDigest || req.Header.Get("X-Racer-Phase") != "2" {
							t.Errorf("stale replay hid outstanding R2 receive: phase=%s digest=%s want=%s", req.Header.Get("X-Racer-Phase"), req.Header.Get("X-Racer-Digest"), currentDigest)
						}
					}

					if mode == "replay" && !replayed && currentDigest != "" && req.Header.Get("X-Racer-Digest") == currentDigest && req.Header.Get("X-Racer-Phase") == "2" {
						if len(replay) == 0 || replayBoot != req.Header.Get("X-Racer-Boot") {
							t.Error("missing exact same-boot replay")
						}

						replayed = true

						w.Header().Set("Content-Length", strconv.Itoa(len(replay)))
						_, _ = w.Write(replay)

						return
					}

					if tampered && !checked {
						if req.Header.Get("X-Racer-Phase") != "0" {
							t.Error("invalid grant advanced workers")
						}

						checked = true
					}

					if corrected {
						_, err := rec.Reconcile(req.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}})
						if err != nil {
							t.Error(err)
						}
					}

					rr := httptest.NewRecorder()
					f.s.control(rr, req)

					if rr.Code == 200 {
						var c pb.ControlCommand
						if err := proto.Unmarshal(rr.Body.Bytes(), &c); err != nil {
							t.Error(err)
						}

						if mode == "replay" && c.Revision == 1 && c.Configuration != nil && len(replay) == 0 {
							replay = append([]byte(nil), rr.Body.Bytes()...)
							replayBoot = req.Header.Get("X-Racer-Boot")
						}

						if mode == "replay" && c.Revision == 2 {
							currentDigest = hex.EncodeToString(c.SnapshotDigest)
						}

						if len(c.ForwardDigest) > 0 {
							ds, err := forwardHistory(f.durable(t).Data["forwards"], "default", c.Revision)
							if err != nil {
								t.Error(err)
							}

							found := false

							for _, d := range ds {
								if d.Boot == req.Header.Get("X-Racer-Boot") && d.Grant == c.Revision && d.PodUID == c.PodUid {
									found = true
								}
							}

							if !found {
								t.Error("forward grant preceded durable boot binding")
							}

							if !tampered && (mode == "bad-digest" || mode == "bad-revision" || mode == "bad-pod") {
								switch mode {
								case "bad-digest":
									c.ForwardDigest[0] ^= 1
								case "bad-revision":
									c.ForwardRevision++
								case "bad-pod":
									c.PodUid = "wrong-pod"
								}

								body, _ := proto.Marshal(&c)
								tampered = true

								w.Header().Set("Content-Length", strconv.Itoa(len(body)))
								_, _ = w.Write(body)

								return
							}
						}

						if corrected && phase >= 2 && (len(c.ForwardDigest) > 0 || c.Revision == 1) && lost < 2 {
							lost++

							w.Header().Set("Content-Length", "0")
							w.WriteHeader(503)

							return
						}
					}

					w.Header().Set("Content-Length", strconv.Itoa(rr.Body.Len()))
					w.WriteHeader(rr.Code)
					_, _ = w.Write(rr.Body.Bytes())
				})

				httpServer, pki := coordinationServer(t, mux)
				defer httpServer.Close()

				dir := pki.directory(t, f.node, "pod-uid")

				ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
				defer cancel()

				cmd := exec.CommandContext(ctx, bin, "coordination_tests::production_forward_child", "--ignored", "--nocapture", "--test-threads=1")

				cmd.Env = append(os.Environ(), "RACER_TLS_DIR="+dir, "RACER_CONTROL_PLANE_URL="+httpServer.URL+"/v3/"+identity("universe", "default")+"/"+f.node,
					"RACER_UNIVERSE="+identity("universe", "default"), "RACER_NODE="+f.node, "RACER_POD_UID=pod-uid", "RACER_FORWARD_PHASE="+strconv.Itoa(int(phase)), "RACER_FORWARD_MODE="+mode)
				out, err := cmd.CombinedOutput()
				t.Logf("%s", out)

				if err != nil {
					t.Fatalf("TLS forward: %v", err)
				}

				mu.Lock()
				defer mu.Unlock()

				if !corrected || rec.loaded["default"].Revision != 2 {
					t.Fatal("correction never committed")
				}

				if phase >= 2 && lost != 2 && (phase != 4 || mode != "survivor" || lost < 1) {
					t.Fatalf("lost decision responses not exercised: %d", lost)
				}

				if (mode == "bad-digest" || mode == "bad-revision" || mode == "bad-pod") && (!tampered || !checked) {
					t.Fatalf("required tamper injection/rejection missing: mode=%s hit=%v checked=%v", mode, tampered, checked)
				}

				if mode == "replay" && (!replayed || !replayChecked) {
					t.Fatal("required replay injection missing")
				}
			})
		}
	}
}

// Multi-recipient forward recovery preserves survivor obligations and barriers.

// Legacy enrollment fixture; production control validates a distinct selected
// Pod UID from each recipient's client certificate.
type multiTokenClient struct{ client.Client }

func (c multiTokenClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if review, ok := obj.(*authenticationv1.TokenReview); ok {
		if review.Spec.Token == "pod-survivor" || review.Spec.Token == "pod-failed" {
			review.Status.Authenticated = true
			review.Status.Audiences = review.Spec.Audiences
			review.Status.User.Extra = map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {review.Spec.Token}}
		}

		return nil
	}

	return c.Client.Create(ctx, obj, opts...)
}

func TestB15ProductionMultiRecipient(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN")
	}

	for _, trap := range []uint32{2, 3, 4} {
		t.Run(fmt.Sprint(trap), func(t *testing.T) { runMultiForward(t, bin, trap) })
	}
}

func runMultiForward(t *testing.T, bin string, trap uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	n, p, bad := fixtures()
	n.Name, n.UID = "survivor", "node-survivor"
	p.Name, p.UID, p.Spec.NodeName = "survivor", "pod-survivor", n.Name
	p.Status.PodIP = "10.1.1.1"
	n2, p2 := n.DeepCopy(), p.DeepCopy()
	n2.Name, n2.UID = "failed", "node-failed"
	p2.Name, p2.UID, p2.Spec.NodeName, p2.Status.PodIP = "failed", "pod-failed", n2.Name, "10.1.1.2"
	bad.Annotations[originPortAnnotation] = "8080"
	bad.Annotations[annotationPrefix+"listener-port"] = "10000"
	good := bad.DeepCopy()
	good.Name = "good"
	good.Annotations[annotationPrefix+"listener-port"] = "10001"
	kube := multiTokenClient{fakeKube(n, n2, p, p2, bad, good)}
	store := stateStore{client: kube, namespace: "state"}
	s := &Server{controlStore: store}
	rec := newTestReconciler(kube)
	rec.server, rec.store = s, store

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	if _, err := rec.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(rec.loaded["default"])
	if err != nil {
		t.Fatal(err)
	}

	ids := map[string]string{"survivor": index.g.Nodes[n.Name].ID, "failed": index.g.Nodes[n2.Name].ID}
	digests := map[string]uint64{}

	for _, id := range ids {
		snap := index.snapshot(id)
		if len(snap.Volumes) != 2 || len(snap.Peers) != 1 {
			t.Fatalf("fixture is not an actual two-recipient peer topology: %v nodes=%v", snap, index.g.Nodes)
		}

		raw, _ := marshalSnapshot(snap)
		h := sha256.Sum256(raw)
		digests[hex.EncodeToString(h[:])] = 1
	}

	var mu sync.Mutex

	seen := map[string]bool{}
	boots := map[string]string{}
	corrected, failedStaging, failedPrepared := false, false, false
	lost := map[string]int{}
	restarts := 0
	restart := func() {
		s = &Server{controlStore: store}
		rec = newTestReconciler(kube)
		rec.server, rec.store = s, store
		restarts++
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /multi/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if seen[fmt.Sprintf("survivor/1/%d", trap)] && seen[fmt.Sprintf("failed/1/%d", trap)] {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(409)
		}
	})
	mux.HandleFunc("GET /multi/failed", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		failedStaging = true

		if err := kube.Delete(r.Context(), bad); err != nil {
			t.Error(err)
			w.WriteHeader(500)

			return
		}

		corrected = true
		// The survivor and the newly failed workers remain alive across this restart.
		restart()
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		role := "survivor"
		if req.PathValue("node") == ids["failed"] {
			role = "failed"
		}

		ack, _ := strconv.ParseUint(req.Header.Get("X-Racer-Phase"), 10, 32)
		rev := digests[req.Header.Get("X-Racer-Digest")]
		seen[fmt.Sprintf("%s/%d/%d", role, rev, ack)] = true

		boot := req.Header.Get("X-Racer-Boot")
		if old := boots[role]; old != "" && old != boot {
			seen[role+"/reboot"] = true
		}

		boots[role] = boot
		if role == "failed" && rev == 2 && ack >= 1 {
			failedPrepared = true
		}

		if !corrected && rev == 1 && ack >= uint64(trap) {
			// Freeze BEFORE handler authority, using only real Subscriber acks.
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(503)

			return
		}

		if corrected && role == "survivor" && !failedPrepared {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(503)

			return
		}
		// Reconcile reloads persisted topology/rollout on each controller restart.
		if _, err := rec.Reconcile(req.Context(), request); err != nil {
			t.Error(err)
			w.WriteHeader(503)

			return
		}

		rr := httptest.NewRecorder()
		s.control(rr, req)

		if rr.Code != 200 {
			t.Errorf("%s control HTTP %d: %s", role, rr.Code, rr.Body.String())
		}

		if rr.Code == 200 {
			var command pb.ControlCommand
			if err := proto.Unmarshal(rr.Body.Bytes(), &command); err != nil {
				t.Error(err)
			}

			digests[hex.EncodeToString(command.SnapshotDigest)] = command.Revision
			if command.Phase == 5 {
				t.Error("post-receive abort")
			}

			cm := &corev1.ConfigMap{}
			if err := kube.Get(req.Context(), client.ObjectKey{Namespace: "state", Name: stateName("default") + "-rollout"}, cm); err != nil {
				t.Error(err)
			}

			if command.Configuration != nil {
				h := sha256.Sum256(configurationSnapshot(t, command.Configuration))
				if !bytes.Equal(h[:], command.SnapshotDigest) {
					t.Error("snapshot digest changed")
				}
			}

			if role == "survivor" && len(command.ForwardDigest) > 0 {
				t.Error("survivor received a skip grant")
			}

			if corrected && command.Revision == 1 || len(command.ForwardDigest) > 0 {
				ds, err := forwardHistory(cm.Data["forwards"], "default", 2)
				if err != nil {
					t.Error(err)
				}

				found := false

				for _, d := range ds {
					h, _ := hex.DecodeString(d.snapshotRef().Digest)

					expected := command.SnapshotDigest
					if len(command.ForwardDigest) > 0 {
						expected = command.ForwardDigest
					}

					if d.Boot == boot && d.PodUID == "pod-"+role && bytes.Equal(h[:], expected) && (len(command.ForwardDigest) == 0 || d.Grant == command.Revision) {
						found = true
					}
				}

				if !found {
					t.Error("historical/forward authority not durable before delivery")
				}
			}

			if corrected && role == "survivor" && command.Revision == 2 && !seen["survivor/1/4"] {
				t.Error("survivor advanced before old retirement ack")
			}

			if command.Revision == 2 && command.Phase >= 2 && (!seen["survivor/2/1"] || !seen["failed/2/1"]) {
				t.Error("new receive barrier omitted a recipient")
			}

			if command.Revision == 2 && command.Phase >= 3 && (!seen["survivor/2/2"] || !seen["failed/2/2"]) {
				t.Error("new activation barrier omitted a recipient")
			}

			if corrected && lost[role] == 0 && (role == "survivor" && command.Revision == 1 || role == "failed" && len(command.ForwardDigest) > 0) {
				lost[role]++
				// Drop an actually authorized decision, then discard controller cache.
				restart()
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(503)

				return
			}
		}

		w.Header().Set("Content-Length", strconv.Itoa(rr.Body.Len()))
		w.WriteHeader(rr.Code)
		_, _ = w.Write(rr.Body.Bytes())
	})

	httpServer, pki := coordinationServer(t, mux)
	defer httpServer.Close()

	type child struct {
		cmd    *exec.Cmd
		output bytes.Buffer
		role   string
		waited bool
	}

	start := func(role, node string) *child {
		t.Helper()

		dir := pki.directory(t, ids[node], "pod-"+node)

		c := &child{role: role}
		c.cmd = exec.CommandContext(ctx, bin, "forward_multi_tests::production_multi_forward_child", "--ignored", "--nocapture", "--test-threads=1")

		c.cmd.Env = append(os.Environ(), "RACER_TLS_DIR="+dir, "RACER_CONTROL_PLANE_URL="+httpServer.URL+"/v3/"+identity("universe", "default")+"/"+ids[node], "RACER_UNIVERSE="+identity("universe", "default"), "RACER_NODE="+ids[node], "RACER_POD_UID=pod-"+node, "RACER_MULTI_ROLE="+role, fmt.Sprintf("RACER_MULTI_PHASE=%d", trap))
		c.cmd.Stdout = &c.output

		c.cmd.Stderr = &c.output
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if !c.waited {
				_ = c.cmd.Process.Kill()
				_ = c.cmd.Wait()
			}
		})

		return c
	}
	wait := func(c *child) {
		t.Helper()

		err := c.cmd.Wait()
		c.waited = true
		t.Logf("%s: %s", c.role, c.output.String())

		if err != nil {
			t.Fatalf("%s: %v", c.role, err)
		}
	}
	survivor := start("survivor", "survivor")
	initial := start("initial", "failed")
	wait(initial)
	mu.Lock()
	restart()
	mu.Unlock()

	failed := start("failed", "failed")
	wait(failed)
	wait(survivor)
	mu.Lock()
	defer mu.Unlock()

	if !failedStaging || !failedPrepared || !seen["failed/reboot"] || lost["survivor"] != 1 || lost["failed"] != 1 || restarts != 4 {
		t.Fatalf("schedule incomplete: staging=%v prepared=%v reboot=%v lost=%v restarts=%d", failedStaging, failedPrepared, seen["failed/reboot"], lost, restarts)
	}

	for _, role := range []string{"failed", "survivor"} {
		for _, phase := range []uint32{1, 2, 3, 4} {
			if !seen[fmt.Sprintf("%s/2/%d", role, phase)] {
				t.Errorf("missing actual worker ack %s/2/%d", role, phase)
			}
		}
	}

	if rec.loaded["default"].Revision != 2 {
		t.Fatal("unexpected corrective revision")
	}

	if seen["survivor/reboot"] {
		t.Fatal("survivor restarted instead of preserving its old obligation")
	}

	for _, role := range []string{"failed", "survivor"} {
		if s.rollouts["default"].acks[ids[role]].phase != 4 {
			t.Errorf("controller did not consume final %s retirement", role)
		}
	}
}
