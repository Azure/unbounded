// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestReplicaInstallationAndFreshness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leader := initializedTopology(t)
		publication := reconcileTopology(t, leader, t.Context())
		follower := Assemble(leader.Config, leader.Client, leader.APIReader)

		process, cancel := context.WithCancel(t.Context())
		defer cancel()

		follower.authority.BindProcess(process)

		image, err := wire.DecodePublication(strings.NewReader(publication.encoded))
		if err != nil {
			t.Fatal(err)
		}

		bad := image

		bad.Sequence++
		if err := follower.Replication.installReplica(t.Context(), process, bad); err == nil {
			t.Fatal("unconfirmed counters installed")
		}

		if err := follower.Replication.installReplica(t.Context(), process, image); err != nil {
			t.Fatal(err)
		}

		_, err = follower.authority.Current()
		if err != nil || capturePublication(t, follower.authority).encoded != publication.encoded {
			t.Fatal("replica did not install canonical image", err)
		}

		time.Sleep(20 * time.Second)

		if err := follower.authority.AcceptReplica(t.Context(), process, image); err != nil {
			t.Fatal(err)
		}

		time.Sleep(20 * time.Second)

		if follower.authority.PublicationReady() != nil {
			t.Fatal("unchanged authoritative confirmation did not renew freshness")
		}

		time.Sleep(11 * time.Second)

		if follower.authority.PublicationReady() == nil {
			t.Fatal("expired image still serves")
		}

		if err := follower.Replication.installReplica(t.Context(), process, image); err != nil {
			t.Fatal(err)
		}

		if capturePublication(t, follower.authority).encoded != publication.encoded {
			t.Fatal("interruption discarded validated image")
		}

		rollback := publication.record

		rollback.ContentHash = strings.Repeat("0", 64)

		cm, _, err := readVersion(t.Context(), leader.APIReader, leader.Config)
		if err != nil {
			t.Fatal(err)
		}

		cm.Data = versionData(rollback)
		if err := leader.Update(t.Context(), cm); err != nil {
			t.Fatal(err)
		}

		if follower.authority.AcceptReplica(t.Context(), process, image) == nil {
			t.Fatal("same-counter corruption accepted")
		}

		cancel()

		if follower.authority.PublicationReady() == nil {
			t.Fatal("process cancellation ignored")
		}
	})
}

func TestReplicaServingSurvivesPublisherCancellation(t *testing.T) {
	r := initializedTopology(t)
	r.authority.BindProcess(t.Context())
	publisher, cancel := context.WithCancel(t.Context())
	publication := reconcileTopology(t, r, publisher)

	cancel()

	if _, err := r.authority.Current(); err != nil {
		t.Fatal("publisher lifetime leaked into serving", err)
	}

	if _, _, err := publication.writeContext(t.Context()); err != nil {
		t.Fatal("image bound to publisher instead of process")
	}
}

func TestReplicaObservationsFailClosed(t *testing.T) {
	f := newServingFixture(t)
	r := f.a.Replication
	r.observe(f.ctx)

	if f.a.Server.Ready(nil) != nil {
		t.Fatal("valid observation withdrew readiness")
	}

	r.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("offline")
	}})
	fixtureDependencies[r.authority].reader = r.APIReader
	r.observe(f.ctx)

	if f.a.Server.Ready(nil) != nil {
		t.Fatal("transport interruption discarded recent state")
	}

	r.APIReader = f.a.Topology.APIReader
	fixtureDependencies[r.authority].reader = r.APIReader

	cm, _, err := readVersion(f.ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	cm.Data["sequence"] = "0"
	if err := r.Client.Update(f.ctx, cm); err != nil {
		t.Fatal(err)
	}

	r.observe(f.ctx)

	if f.a.Server.Ready(nil) == nil {
		t.Fatal("observed invalid authority still serves")
	}
}

func TestReplicaLeaderDiscovery(t *testing.T) {
	f := newServingFixture(t)

	r := f.a.Replication
	if err := coordv1.AddToScheme(r.Client.Scheme()); err != nil {
		t.Fatal(err)
	}

	r.Config.ControllerServiceAccount = "racer-controller"
	r.Config.ReplicationPort = 8443
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "controller", UID: "controller-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "racer-controller"}, Status: corev1.PodStatus{PodIP: "192.0.2.10"}}

	lease := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: "racer-controller"}, Spec: coordv1.LeaseSpec{HolderIdentity: ptr.To("controller/controller-uid"), RenewTime: ptr.To(metav1.NewMicroTime(time.Now())), LeaseDurationSeconds: ptr.To(int32(15))}}
	for _, obj := range []client.Object{pod, lease} {
		if err := r.Client.Create(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	if address, err := r.leaderAddress(f.ctx); err != nil || address != "192.0.2.10:8443" {
		t.Fatal(address, err)
	}

	lease.Spec.HolderIdentity = ptr.To("controller/replaced-uid")
	if err := r.Client.Update(f.ctx, lease); err != nil {
		t.Fatal(err)
	}

	if _, err := r.leaderAddress(f.ctx); err == nil {
		t.Fatal("replaced leader Pod accepted")
	}
}
