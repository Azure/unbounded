// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kubelease_test

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/kubelease"
	"github.com/Azure/unbounded/internal/cachemesh/memtransport"
)

// Example wires a Mesh to a Kubernetes Lease. In production the clientset
// comes from kubernetes.NewForConfig and the transport is the application's
// own; every member of the cluster uses the same Lease name and namespace.
func Example() {
	cs := fake.NewClientset()
	store := kubelease.New(cs.CoordinationV1().Leases("gantry-system"), "gantry-cachemesh")

	net := memtransport.NewNetwork()

	m, err := cachemesh.New(cachemesh.Config{
		ID:        "node-a",
		Addr:      "10.0.0.1:7946",
		Transport: net.Transport("10.0.0.1:7946"),
		Lease:     store,
		// A small cluster: the lone first member bootstraps at once.
		ExpectedSize: 3,
		Logger:       slog.New(slog.DiscardHandler),
		Tuning: cachemesh.Tuning{
			Election: cachemesh.ElectionConfig{LotteryRound: 100 * time.Millisecond},
		},
	})
	if err != nil {
		panic(err)
	}

	net.Register(m.Addr(), m.ServeControl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = m.Run(ctx) }()

	for !m.Leading() {
		time.Sleep(10 * time.Millisecond)
	}

	l, err := cs.CoordinationV1().Leases("gantry-system").Get(ctx, "gantry-cachemesh", metav1.GetOptions{})
	if err != nil {
		panic(err)
	}

	fmt.Println("holder:", *l.Spec.HolderIdentity)
	fmt.Println("addr:", l.Annotations[kubelease.AnnotationAddr])

	// Output:
	// holder: node-a
	// addr: 10.0.0.1:7946
}
