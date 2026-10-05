// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestTerminalPodsCannotReplaceEndpoints(t *testing.T) {
	ownership := memberOwnership(t, testDaemonSetUID)
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodUnknown, corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			old := memberPod("old", 1, "192.0.2.1")
			newest := memberPod("new", 2, "192.0.2.2")
			newest.Status.Phase = phase
			terminal := phase == corev1.PodFailed || phase == corev1.PodSucceeded

			want := "192.0.2.2:8082"
			if terminal {
				want = "192.0.2.1:8082"
			}

			endpoint, err := selectEndpoint([]corev1.Pod{old, newest}, ownership, old.Spec.NodeName, 8082)
			if err != nil || endpoint != want {
				t.Fatalf("selected %q, %v; want %q", endpoint, err, want)
			}

			endpoint, err = selectEndpoint([]corev1.Pod{newest}, ownership, old.Spec.NodeName, 8082)
			if terminal {
				if endpoint != "" || !errors.Is(err, wire.Unavailable) {
					t.Fatalf("terminal-only endpoint: %q, %v", endpoint, err)
				}

				node := memberNode()
				groups := map[string][]corev1.Pod{node.Name: {newest}}

				candidate, _, err := reconcileMembers([]corev1.Node{node}, groups, ownership, nil, 8082)
				if err != nil || len(candidate) != 0 {
					t.Fatalf("terminal-only Pod admitted a new node: %v, %v", candidate, err)
				}

				previous := wire.Member{Node: testNodeUID, Shares: wire.DefaultShares, PeerEndpoint: want}

				candidate, _, err = reconcileMembers([]corev1.Node{node}, groups, ownership, AcceptedMembers{testNodeUID: previous}, 8082)
				if err != nil || candidate[testNodeUID].PeerEndpoint != previous.PeerEndpoint {
					t.Fatalf("terminal-only gap lost admitted endpoint: %v, %v", candidate, err)
				}
			} else if err != nil || endpoint != want {
				t.Fatalf("unready live endpoint: %q, %v", endpoint, err)
			}

			pred := managedPodChanges(Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName})
			before := newest.DeepCopy()

			before.Status.Phase = corev1.PodRunning
			if pred.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: &newest}) != (phase != corev1.PodRunning) {
				t.Fatal("Pod phase transition predicate mismatch")
			}

			before = newest.DeepCopy()

			newest.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			if pred.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: &newest}) {
				t.Fatal("readiness-only change triggered topology")
			}
		})
	}
}

func TestWorkloadNameLabelBounds(t *testing.T) {
	for _, name := range []string{"racer", "racer.custom", strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 63) + ".b", "", "Invalid"} {
		t.Run(name, func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.DaemonSetName = name
			valid := len(validation.IsDNS1123Subdomain(name)) == 0 && len(validation.IsValidLabelValue(name)) == 0

			ds, err := members.DesiredDaemonSet(cfg)
			if !valid {
				if !errors.Is(err, wire.InvalidRequest) || ds != nil {
					t.Fatalf("invalid name accepted: %v", err)
				}

				return
			}

			if err != nil || ds.Name != name || ds.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != name || ds.Spec.Template.Labels["app.kubernetes.io/instance"] != name {
				t.Fatalf("valid name not preserved: %v", err)
			}
		})
	}

	// Other resource names are not instance labels and retain DNS subdomain bounds.
	cfg := workloadConfig(t)
	cfg.BootstrapTrustConfigMap = strings.Repeat("a", 63) + ".trust"

	cfg.DataplaneServiceAccount = strings.Repeat("a", 63) + ".account"
	if _, err := members.DesiredDaemonSet(cfg); err != nil {
		t.Fatal(err)
	}
}
