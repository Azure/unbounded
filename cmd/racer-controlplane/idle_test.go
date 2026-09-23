// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pb "github.com/Azure/unbounded/api/racer"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

func idleFixtures() (*corev1.Node, *corev1.Pod, *racerapi.P2PCache) {
	n, p, s := fixtures()
	p.Namespace, p.UID, p.Spec.ServiceAccountName = "state", "pod-uid", "racer-dataplane"

	return n, p, s
}

func reconcileIdle(t *testing.T, r *reconciler) *topologyIndex {
	t.Helper()

	r.podNamespace = "state"

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(r.loaded["default"])
	if err != nil {
		t.Fatal(err)
	}

	return index
}

func TestIdleSiteAuthenticatedLifecycle(t *testing.T) {
	ctx := context.Background()
	n, p, svc := idleFixtures()
	kube := tokenClient{fakeKube(n, p)}
	r := newTestReconciler(kube)
	f := &coordinationFixture{s: r.server, api: &rolloutAPI{Client: kube}, node: identity("node", string(n.UID))}
	activate := func(idle bool, volumes int) {
		t.Helper()
		index := reconcileIdle(t, r)

		m := index.g.Nodes[n.Name]
		if m.PodUID != string(p.UID) || m.IP != p.Status.PodIP {
			t.Fatalf("idle selection missing: %+v", m)
		}

		f.digest = ""
		for phase := uint32(0); phase <= 4; phase++ {
			command := f.call(t, phase, 200)
			if command.Configuration == nil {
				continue
			}

			var snapshot pb.Snapshot
			if err := proto.Unmarshal(configurationSnapshot(t, command.Configuration), &snapshot); err != nil {
				t.Fatal(err)
			}

			if snapshot.Idle != idle || len(snapshot.Volumes) != volumes {
				t.Fatalf("unexpected configuration: %v", &snapshot)
			}
		}

		if busy, err := r.server.rolloutBusy(ctx, index); err != nil || busy {
			t.Fatalf("idle rollout failed to retire: busy=%v err=%v", busy, err)
		}
	}
	// Fresh enable authenticates even though neither a Service nor PodReady exists.
	activate(true, 0)

	if err := kube.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}

	activate(false, 1)

	if err := kube.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}

	activate(true, 0)
	// Restart recovers durable idle authorization without advancing the revision.
	revision := r.loaded["default"].Revision
	r = newTestReconciler(kube)
	f.s = r.server

	activate(true, 0)

	if r.loaded["default"].Revision != revision {
		t.Fatal("restart advanced idle revision")
	}
	// A DaemonSet upgrade replaces the Pod. Its predecessor's cached credential
	// must fail authorization against the newly committed selection.
	if err := kube.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}

	p.ResourceVersion, p.UID = "", "replacement-pod"
	if err := kube.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	index := reconcileIdle(t, r)
	if m := index.g.Nodes[n.Name]; m.PodUID != string(p.UID) || !index.snapshot(m.ID).Idle {
		t.Fatalf("upgrade did not select replacement: %+v", m)
	}

	f.call(t, 0, 403)
	// The certificate authenticates the new Pod, then the same barriers apply.
	f.podUID = string(p.UID)

	activate(true, 0)
}

func TestIdleSiteSelectionFailsClosed(t *testing.T) {
	for _, mode := range []string{"excluded", "moved", "deleted", "unready-node", "terminating", "pending", "wrong-universe", "wrong-namespace", "wrong-account", "unowned", "recreated-node"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			n, p, _ := idleFixtures()
			kube := fakeKube(n, p)
			r := newTestReconciler(kube)
			first := reconcileIdle(t, r)

			roll, err := r.server.rolloutFor(ctx, first)
			if err != nil {
				t.Fatal(err)
			}

			if err := r.server.persistPhase(ctx, "default", roll, 4); err != nil {
				t.Fatal(err)
			}

			switch mode {
			case "excluded":
				n.Labels[racer.ExcludeLabelKey] = "true"
			case "moved":
				n.Labels[racer.SiteLabelKey] = "other"
			case "deleted":
				if err := kube.Delete(ctx, n); err != nil {
					t.Fatal(err)
				}
			case "unready-node":
				n.Status.Conditions = nil
			case "terminating":
				p.DeletionTimestamp = new(metav1.Now())
				p.Finalizers = []string{"test"}
			case "pending":
				p.Status.Phase = corev1.PodPending
			case "wrong-universe":
				p.Labels[universeAnnotation] = "other"
			case "wrong-namespace":
				p.Namespace = "other"
			case "wrong-account":
				p.Spec.ServiceAccountName = "application"
			case "unowned":
				p.OwnerReferences = nil
			case "recreated-node":
				n.UID = "new-node"
			}
			// Replace the cached view while retaining the live Pod in the direct API
			// so removal authority survives selector loss and Node deletion.
			if mode == "deleted" {
				r.client = fakeKube(p)
			} else {
				r.client = fakeKube(n, p)
			}

			next := reconcileIdle(t, r)

			oldID := first.g.Nodes["node"].ID
			if snap := next.snapshot(oldID); snap == nil || snap.Idle || len(snap.Volumes) != 0 {
				t.Fatalf("historical recipient gained idle readiness: %v", snap)
			}

			for _, m := range next.g.Nodes {
				if m.IP != "" || next.snapshot(m.ID).Idle {
					t.Fatalf("ineligible member selected: %+v", m)
				}
			}
		})
	}
}

// Real mutual-TLS delivery and production Rust Subscriber/Volumes workers;
// Kubernetes discovery/persistence use the existing fake API.
func TestProductionIdleSiteLifecycle(t *testing.T) {
	bin := os.Getenv("RACER_COORDINATION_TEST_BIN")
	if bin == "" {
		t.Skip("set RACER_COORDINATION_TEST_BIN to the Rust lib-test executable")
	}

	ctx := context.Background()
	n, p, svc := idleFixtures()
	kube := tokenClient{fakeKube(n, p)}
	r := newTestReconciler(kube)
	index := reconcileIdle(t, r)
	node := index.g.Nodes[n.Name].ID

	var (
		mu      sync.Mutex
		problem error
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/{universe}/{node}", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		r.server.control(w, req)
	})
	mux.HandleFunc("GET /advance", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		busy, err := r.server.rolloutBusy(ctx, index)
		if err != nil || busy {
			http.Error(w, "waiting for retirement", http.StatusConflict)
			return
		}

		switch index.g.Revision {
		case 1:
			err = kube.Create(ctx, svc)
		case 2:
			err = kube.Delete(ctx, svc)
		case 3:
			n.Labels[racer.ExcludeLabelKey] = "true"
			err = kube.Update(ctx, n)
		case 4:
			delete(n.Labels, racer.ExcludeLabelKey)
			err = kube.Update(ctx, n)
		default:
			err = nil
		}

		if err == nil {
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}})
		}

		if err == nil {
			index, err = indexGeneration(r.loaded["default"])
		}

		if err != nil {
			problem = err
			http.Error(w, err.Error(), 500)

			return
		}

		w.WriteHeader(http.StatusOK)
	})

	server, pki := coordinationServer(t, mux)
	defer server.Close()

	dir := pki.directory(t, node, string(p.UID))

	childCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	cmd := exec.CommandContext(childCtx, bin, "coordination_tests::production_idle_site_child", "--ignored", "--nocapture", "--test-threads=1")

	cmd.Env = append(os.Environ(),
		"RACER_TLS_DIR="+dir,
		"RACER_CONTROL_PLANE_URL="+server.URL+"/v3/"+identity("universe", "default")+"/"+node,
		"RACER_UNIVERSE="+identity("universe", "default"), "RACER_NODE="+node,
		"RACER_POD_UID="+string(p.UID))
	output, err := cmd.CombinedOutput()
	t.Logf("Rust idle lifecycle: %s", output)

	if err != nil {
		t.Fatalf("Rust idle lifecycle failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if problem != nil {
		t.Fatal(problem)
	}

	if index.g.Revision != 5 || !index.snapshot(node).Idle {
		t.Fatal("idle lifecycle did not complete")
	}

	roll := r.server.rollouts["default"]
	if roll.phase != 4 || roll.acks[node].phase != 4 {
		t.Fatal("final idle generation not acknowledged")
	}
	// Snapshot identity remains pinned through exclusion and re-enrollment.
	if hex.EncodeToString(index.snapshot(node).Node) != node {
		t.Fatal("identity changed")
	}
}
