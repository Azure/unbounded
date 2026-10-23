//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

// TestControlPlaneConfiguredSeatCount checks that a node without a seat
// receives new goal state from the configured two-node control plane.
func TestControlPlaneConfiguredSeatCount(t *testing.T) {
	h := cluster(t)
	h.applyCache()
	h.installRacer()

	const addedCache = "e2e-unseated-client"

	t.Cleanup(func() {
		h.kubectl("delete", "clustercache", addedCache, "--ignore-not-found")
		h.setSeatCount("auto")
		h.waitHealthy("automatic seat count restored")
	})

	h.setSeatCount("2")

	var view controlView

	h.waitFor("exactly two controllers with every dataplane ready", 5*time.Minute, func() (bool, string) {
		view = h.controlView()
		desired := h.dataplaneDesired()
		_, primaryOK := view.primarySeat()

		return desired > 2 && len(view.Pods) == desired && view.readyPods() == desired &&
			primaryOK && len(view.validSeats()) == 2 && len(view.seatProblems()) == 0, view.String()
	})

	before, view := h.waitConverged("goal state from two elected controllers")
	seated := map[string]bool{}

	for _, seat := range view.validSeats() {
		seated[seat.Pod.UID] = true
	}

	var follower dataplanePod

	for _, pod := range view.Pods {
		if !seated[pod.UID] {
			follower = pod
			break
		}
	}

	if follower.UID == "" {
		t.Fatal("no unseated dataplane remained with the two-controller setting")
	}

	h.apply("apiVersion: racer.unbounded-cloud.io/v1alpha1\nkind: ClusterCache\nmetadata:\n  name: " + addedCache + "\n")
	h.waitFor("new cache socket on an unseated dataplane", 5*time.Minute, func() (bool, string) {
		current := h.controlView()
		for _, seat := range current.validSeats() {
			if seat.Pod.UID == follower.UID {
				return false, "the observed dataplane acquired a seat"
			}
		}

		_, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace,
			"exec", follower.Name, "-c", "dataplane", "--", "test", "-S",
			"/run/racer/"+addedCache+"/client/socket")

		return err == nil, fmt.Sprintf("unseated pod %s: %v", follower.Name, err)
	})

	after, _ := h.waitConverged("new catalog applied by controllers and unseated dataplanes")
	if err := checkMembershipAdvance(before, after); err != nil {
		t.Fatalf("goal state after adding a cache: %v", err)
	}

	if after.AcceptedSequence <= before.AcceptedSequence {
		t.Fatal("new cache did not advance the publication sequence")
	}
}

func (h *harness) setSeatCount(count string) {
	h.t.Helper()
	h.kubectl("-n", operatorNamespace, "patch", "configmap", "racer-dataplane-config",
		"--type=merge", "-p", fmt.Sprintf(`{"data":{"RACER_CONTROL_SEATS":%q}}`, count))

	var config corev1.ConfigMap
	if !h.getJSON(&config, "-n", operatorNamespace, "get", "configmap", "racer-dataplane-config") {
		h.t.Fatal("dataplane configuration could not be read after changing the seat count")
	}

	expected := component.ConfigMapPayloadHash(&config)

	h.waitFor("DaemonSet to use seat count "+count, time.Minute, func() (bool, string) {
		var ds appsv1.DaemonSet
		if !h.getJSON(&ds, "-n", operatorNamespace, "get", "daemonset", "racer-dataplane") {
			return false, "DaemonSet could not be read"
		}

		return ds.Spec.Template.Annotations["unbounded-cloud.io/racer-dataplane-config-hash"] == expected,
			"configuration hash not yet applied"
	})
	h.rollout("daemonset", "racer-dataplane", 5*time.Minute)
}
