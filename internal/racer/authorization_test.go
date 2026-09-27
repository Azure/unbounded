// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

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

	f.a.Server.Bootstrap.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
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
