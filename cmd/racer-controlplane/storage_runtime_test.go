// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer"
)

// Runs the shipping signed handler, subscriber, storage coordinator, workers,
// management endpoint and status reconciler. Kubernetes/TokenReview is fake;
// the daemon, ext4 inodes, io_uring and process restarts are real.
func TestStorageRuntimeSignedResizeRestart(t *testing.T) {
	binary := os.Getenv("RACER_DATAPLANE_BINARY")
	if binary == "" {
		t.Skip("set RACER_DATAPLANE_BINARY to test signed runtime resizing")
	}

	ctx := context.Background()
	node, pod, _ := idleFixtures()
	node.Annotations = map[string]string{racer.CacheSizeAnnotationKey: "64Mi"}
	kube := tokenClient{fakeKube(node, pod)}
	r := newTestReconciler(kube)
	r.server.signer = testSigner(t, 7)
	index := reconcileIdle(t, r)
	storage := newStorageTest(t, kube, r.server)
	reconcile := func() {
		t.Helper()

		if _, err := storage.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: node.Name}}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()

	var mu sync.Mutex

	available := true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if !available {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		r.server.control(w, req)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dir := t.TempDir()

	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(keys, "controller.pub"), r.server.signer.key[32:], 0o600); err != nil {
		t.Fatal(err)
	}

	peer := coordinationPeerKey(t, dir, keys)

	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("pod-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	listener.Close()

	slab := filepath.Join(dir, "cache.slab")

	var stop func()

	start := func(creationSize string) {
		t.Helper()

		log, err := os.CreateTemp(dir, "daemon-*.log")
		if err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(binary)

		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "RACER_") {
				cmd.Env = append(cmd.Env, value)
			}
		}

		cmd.Env = append(cmd.Env,
			"RACER_CONTROL_PLANE_URL="+server.URL+"/v2/"+identity("universe", "default")+"/"+identity("node", string(node.UID)),
			"RACER_UNIVERSE="+identity("universe", "default"), "RACER_NODE="+identity("node", string(node.UID)),
			"RACER_PEER_KEYS_DIR="+peer, "RACER_CONFIG_KEYS_DIR="+keys, "RACER_CONTROL_TOKEN_FILE="+token,
			"RACER_SLAB_PATH="+slab, "RACER_SLAB_SIZE="+creationSize,
			"RACER_SHARDS=1", "RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=8",
			"RACER_METRICS_ADDR="+address, "RACER_RDMA_MODE=disabled")

		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}

		exit := make(chan error, 1)

		go func() { exit <- cmd.Wait() }()

		var once sync.Once

		stop = func() {
			once.Do(func() {
				_ = cmd.Process.Signal(os.Interrupt)

				select {
				case err := <-exit:
					if err != nil {
						t.Errorf("daemon exit: %v", err)
					}
				case <-time.After(15 * time.Second):
					_ = cmd.Process.Kill()

					<-exit
					t.Error("daemon shutdown timed out")
				}

				log.Close()

				if t.Failed() {
					contents, _ := os.ReadFile(log.Name())
					t.Logf("daemon log:\n%s", contents)
				}
			})
		}
		t.Cleanup(stop)
	}

	type processStatus struct {
		Storage struct {
			Phase          string `json:"phase"`
			AppliedBytes   uint64 `json:"appliedBytes"`
			AppliedVersion uint64 `json:"appliedVersion"`
			Shards         uint64 `json:"shards"`
			Boot           string `json:"boot"`
		} `json:"storage"`
	}

	httpClient := &http.Client{Timeout: time.Second}
	observe := func() cacheStatus {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()

		reconcile()

		var current corev1.Node
		if err := kube.Get(ctx, client.ObjectKeyFromObject(node), &current); err != nil {
			t.Fatal(err)
		}

		var observed cacheStatus
		if err := json.Unmarshal([]byte(current.Annotations[racer.CacheStatusAnnotationKey]), &observed); err != nil {
			t.Fatal(err)
		}

		return observed
	}
	await := func(phase string, bytes uint64, report bool) (processStatus, cacheStatus) {
		t.Helper()

		var (
			local    processStatus
			observed cacheStatus
		)

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := httpClient.Get("http://" + address + "/status")
			if err == nil {
				err = json.NewDecoder(resp.Body).Decode(&local)
				resp.Body.Close()
			}

			observed = observe()

			if err == nil && local.Storage.Phase == phase && local.Storage.AppliedBytes == bytes &&
				(!report || (observed.PolicyPhase == phase && observed.AppliedBytes == bytes && observed.Fresh && observed.Boot == local.Storage.Boot && observed.Shards == local.Storage.Shards)) {
				return local, observed
			}

			time.Sleep(50 * time.Millisecond)
		}

		t.Fatalf("status did not converge to %s/%d: local=%+v node=%+v", phase, bytes, local, observed)

		return local, observed
	}
	setSize := func(value string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()

		if err := kube.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
			t.Fatal(err)
		}

		node.Annotations[racer.CacheSizeAnnotationKey] = value
		if err := kube.Update(ctx, node); err != nil {
			t.Fatal(err)
		}

		reconcile()
		// An input-only edit must leave the complete signed topology unchanged.
		current := reconcileIdle(t, r)
		if current.g.Revision != index.g.Revision || !proto.Equal(current.snapshot(current.g.Nodes[node.Name].ID), index.snapshot(index.g.Nodes[node.Name].ID)) {
			t.Fatal("storage changed topology identity")
		}
	}
	stat := func() os.FileInfo {
		t.Helper()

		info, err := os.Stat(slab)
		if err != nil {
			t.Fatal(err)
		}

		return info
	}

	start("67108864")

	initial, first := await("applied", 64<<20, true)
	old := stat()

	for _, size := range []struct {
		quantity string
		bytes    uint64
		shards   uint64
	}{{"20Gi", 20 << 30, 2}, {"96Mi", 96 << 20, 1}} {
		setSize(size.quantity)
		local, status := await("applied", size.bytes, true)

		info := stat()
		if local.Storage.Boot != initial.Storage.Boot || local.Storage.Shards != size.shards || status.PolicyIdentity != first.PolicyIdentity ||
			status.AppliedVersion != status.PolicyVersion || os.SameFile(old, info) || info.Size() != int64(size.bytes) {
			t.Fatalf("resize did not replace storage in the same process: %+v %+v", local, status)
		}

		old = info
	}
	// Syntactically valid policy above the runtime envelope fails locally and
	// reports the retained actual capacity without poisoning topology readiness.
	setSize("5Ti")

	_, failed := await("failed", 96<<20, true)
	if failed.Error == "" || failed.EffectiveBytes != 5<<40 || !os.SameFile(old, stat()) {
		t.Fatalf("runtime rejection lost last-good cache: %+v", failed)
	}

	resp, err := httpClient.Get("http://" + address + "/readyz")
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("storage failure poisoned topology readiness: %d", resp.StatusCode)
	}

	setSize("invalid")

	_, invalid := await("failed", 96<<20, true)
	if invalid.Phase != "invalid" || invalid.ValidationError == "" || invalid.PolicyVersion != failed.PolicyVersion {
		t.Fatalf("invalid input replaced last-good policy: %+v", invalid)
	}

	setSize("96Mi")

	_, applied := await("applied", 96<<20, true)

	setSize("100663296")

	_, equivalent := await("applied", 96<<20, true)
	if equivalent.PolicyVersion != applied.PolicyVersion || !os.SameFile(old, stat()) {
		t.Fatal("equivalent capacity reset cache or policy version")
	}

	stop()
	func() {
		mu.Lock()
		defer mu.Unlock()

		available = false
		// Recover both controller authorities from durable state, retaining keys.
		signer := r.server.signer
		r = newTestReconciler(kube)
		r.server.signer = signer
		reconcileIdle(t, r)
		storage = newStorageTest(t, kube, r.server)

		reconcile()
	}()
	// Invalid creation environment proves restart opens the recorded layout
	// before receiving policy. No in-memory acknowledgment survives either restart.
	start("not-a-size")

	restarted, _ := await("unmanaged", 96<<20, false)
	if !os.SameFile(old, stat()) || restarted.Storage.AppliedVersion != 0 {
		t.Fatal("restart did not recover authoritative inode without policy")
	}

	mu.Lock()
	available = true
	mu.Unlock()

	final, reported := await("applied", 96<<20, true)
	if final.Storage.Boot == initial.Storage.Boot || reported.PolicyIdentity != applied.PolicyIdentity || reported.PolicyVersion != applied.PolicyVersion || !os.SameFile(old, stat()) {
		t.Fatalf("restart failed durable policy reacknowledgment: %+v", reported)
	}

	t.Logf("signed grow/shrink, 5TiB rejection, status and restart: policy version %d, shards %d", reported.PolicyVersion, reported.Shards)
}
