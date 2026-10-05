// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/server"
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

func TestReconcilerDependencyCancellationRetries(t *testing.T) {
	for _, controller := range []string{"topology", "keyring"} {
		for _, stage := range []string{"read", "write", "completion read"} {
			if controller == "topology" && stage == "completion read" {
				continue
			}

			for _, dependencyErr := range []error{context.DeadlineExceeded, context.Canceled} {
				for _, cancelParent := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%v/parent=%v", controller, stage, dependencyErr, cancelParent), func(t *testing.T) {
						topology := initializedTopology(t)
						app := assembleFixture(topology.Config, topology.Client, topology.APIReader)

						var target reconcile.Reconciler = app.Topology
						if controller == "keyring" {
							runKeys(t, app.Keyring)
							_, _, state, _ := keyState(t, app.Keyring)
							fixtureDependencies[app.authority].now = func() time.Time { return state.NextRotation }
							target = app.Keyring
						}

						ctx, cancel := context.WithCancel(t.Context())
						defer cancel()

						injected, reads := false, 0
						fail := func() error {
							injected = true

							if cancelParent {
								cancel()
							}

							return fmt.Errorf("dependency: %w", dependencyErr)
						}
						base := topology.Client.(client.WithWatch)
						wrapped := interceptor.NewClient(base, interceptor.Funcs{
							Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
								if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == topology.Config.VersionConfigMapName {
									reads++
									if stage == "read" || stage == "completion read" && reads == 2 {
										return fail()
									}
								}

								return c.Get(ctx, key, obj, opts...)
							},
							Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
								if stage == "write" {
									return fail()
								}

								return c.Update(ctx, obj, opts...)
							},
						})
						app.Topology.Client, app.Topology.APIReader = wrapped, wrapped
						fixtureDependencies[app.authority].Client, fixtureDependencies[app.authority].reader = wrapped, wrapped

						_, err := target.Reconcile(ctx, ctrl.Request{})

						want := dependencyErr
						if cancelParent {
							want = context.Canceled
						}

						if !injected || !errors.Is(err, want) || errors.Is(err, reconcile.TerminalError(nil)) != cancelParent {
							t.Fatalf("injected=%v, error=%v, parent canceled=%v", injected, err, cancelParent)
						}

						// A fresh reconcile succeeds without waiting for another watch event.
						app.Topology.Client, app.Topology.APIReader = base, base
						fixtureDependencies[app.authority].Client, fixtureDependencies[app.authority].reader = base, base

						if _, err := target.Reconcile(t.Context(), ctrl.Request{}); err != nil {
							t.Fatalf("retry failed: %v", err)
						}
					})
				}
			}
		}
	}
}

// These are real HTTPS protocol clients, not Rust processes or in-memory Wait
// calls. Kubernetes authority is fake. Never interpret this as API capacity.
func replicationSmokePublish(t *testing.T, ctx context.Context, r *TopologyReconciler, accepted AcceptedMembers) *CommittedPublication {
	t.Helper()

	_, err := r.authority.PublishTopology(ctx, func(context.Context) (TopologyObservation, error) {
		nodes := corev1.NodeList{}

		for id, member := range accepted {
			encoded, err := json.Marshal(member)
			if err != nil {
				return TopologyObservation{}, err
			}

			nodes.Items = append(nodes.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: string(id), UID: types.UID(id), Annotations: map[string]string{admittedMemberAnnotation: string(encoded), wire.SharesAnnotation: strconv.FormatUint(uint64(member.Shares), 10)}}})
		}

		return TopologyObservation{Nodes: nodes, Input: members.Input{Nodes: nodes.Items, PeerPort: r.Config.PeerPort}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return capturePublication(t, r.authority)
}

const (
	testNodeUID                = "11111111-1111-4111-8111-111111111111"
	testOtherUID               = "22222222-2222-4222-8222-222222222222"
	testDaemonSetUID types.UID = "33333333-3333-4333-8333-333333333333"
)

func memberNode() corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: testNodeUID}}
}

func memberOwnership(t *testing.T, uid types.UID) DataplaneWorkloadIdentities {
	t.Helper()
	r := initializedTopology(t, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: DataplaneDaemonSetName, Namespace: "racer", UID: uid}})

	ids, err := readManagedWorkloadIdentities(t.Context(), r.APIReader, Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName})
	if err != nil {
		t.Fatal(err)
	}

	return ids
}

func memberPod(uid types.UID, created int64, ip string) corev1.Pod {
	controller := true

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "racer-" + string(uid), UID: uid, Namespace: "racer",
			CreationTimestamp: metav1.NewTime(time.Unix(created, 0)),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: DataplaneDaemonSetName, UID: testDaemonSetUID, Controller: &controller}},
		},
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func TestParseAnnotations(t *testing.T) {
	zero, maxNUMA := uint32(0), ^uint32(0)
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        MemberAttributes
		wantError   bool
	}{
		{"defaults", nil, MemberAttributes{Shares: 4, RDMANICs: []wire.RDMANIC{}}, false},
		{"explicit empty NICs", map[string]string{wire.RDMANICsAnnotation: "[]", enrolledRDMANICsAnnotation: `[{"device":"a","port":1,"rail":0}]`}, MemberAttributes{Shares: 4, RDMANICs: []wire.RDMANIC{}}, false},
		{"enrolled fallback", map[string]string{enrolledRDMANICsAnnotation: `[{"device":"a","port":1,"rail":0}]`}, MemberAttributes{Shares: 4, RDMANICs: []wire.RDMANIC{{Device: "a", Port: 1}}}, false},
		{"legacy ignored", map[string]string{wire.RailsAnnotation: "malformed", wire.AlignmentAnnotation: "false"}, MemberAttributes{Shares: 4, RDMANICs: []wire.RDMANIC{}}, false},
		{"bounds and sorting", map[string]string{
			wire.SharesAnnotation:   "4294967295",
			wire.RDMANICsAnnotation: `[{"rail":65535,"device":"β<&>","port":255,"numa_node":4294967295},{"rail":0,"device":"a","port":1,"numa_node":0}]`,
		}, MemberAttributes{Shares: ^uint32(0), RDMANICs: []wire.RDMANIC{{Rail: 0, Device: "a", Port: 1, NUMANode: &zero}, {Rail: 65535, Device: "β<&>", Port: 255, NUMANode: &maxNUMA}}}, false},
		{
			"identical duplicates",
			map[string]string{wire.RDMANICsAnnotation: `[{"rail":2,"device":"b","port":1},{"rail":2,"device":"b","port":1}]`},
			MemberAttributes{},
			true,
		},
		{
			"same rail different physical ports",
			map[string]string{wire.RDMANICsAnnotation: `[{"rail":1,"device":"b","port":1},{"rail":1,"device":"a","port":2},{"rail":1,"device":"a","port":1}]`},
			MemberAttributes{Shares: 4, RDMANICs: []wire.RDMANIC{{Rail: 1, Device: "a", Port: 1}, {Rail: 1, Device: "a", Port: 2}, {Rail: 1, Device: "b", Port: 1}}},
			false,
		},
		{
			"unknown fields rejected",
			map[string]string{wire.RDMANICsAnnotation: `[{"rail":0,"device":"a","port":1,"Rail":1,"future":{"x":true}}]`},
			MemberAttributes{},
			true,
		},
		{"decimal shares", map[string]string{wire.SharesAnnotation: "0008"}, MemberAttributes{Shares: 8, RDMANICs: []wire.RDMANIC{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := memberNode()
			node.Annotations = tc.annotations

			got, err := ParseAnnotations(&node)
			if tc.wantError {
				if err == nil {
					t.Fatal("unknown wire fields accepted")
				}

				return
			}

			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestParseAnnotationsRejectsInvalidUpdates(t *testing.T) {
	for field, values := range map[string][]string{
		wire.SharesAnnotation: {"", "0", "-1", "+1", "4294967296", "1.0", "1e2", "0x10", " 4", "4 ", "٤"},
		wire.RDMANICsAnnotation: {
			"", "null", "{}", "[null]", "[{}]", `[{"rail":0}]`, `[{"device":"a","port":1}]`,
			`[{"rail":65536,"device":"a","port":1}]`, `[{"rail":-1,"device":"a","port":1}]`, `[{"rail":1.0,"device":"a","port":1}]`,
			`[{"rail":"0","device":"a","port":1}]`, `[{"rail":0,"device":"","port":1}]`, `[{"rail":0,"device":"a\n","port":1}]`,
			`[{"rail":0,"device":"a\r","port":1}]`, `[{"rail":0,"device":"a\u0000","port":1}]`,
			`[{"rail":0,"device":"a","port":1,"numa_node":null}]`, `[{"rail":0,"device":"a","port":1,"numa_node":4294967296}]`,
			`[{"rail":0,"device":"a","port":1,"numa_node":-1}]`, `[{"rail":0,"device":"a","port":1,"numa_node":1e0}]`,
			`[{"rail":0,"device":"a","port":1,"rail":1}]`, `[{"rail":0,"device":"a","port":1,"\u0072ail":1}]`,
			`[{"rail":0,"device":"a","port":1,"future":{"x":1,"x":2}}]`,
			`[{"Rail":0,"device":"a","port":1}]`, `[{"rail":0,"device":"\ud800","port":1}]`,
			"[{\"rail\":0,\"device\":\"\xff\",\"port\":1}]", "[] []",
			`[{"rail":0,"device":"a","port":1},{"rail":1,"device":"a","port":1}]`,
			`[{"rail":0,"device":"a","port":1},{"rail":0,"device":"a","port":1,"numa_node":0}]`,
			`[{"rail":0,"device":"a","port":1,"numa_node":1},{"rail":0,"device":"a","port":1,"numa_node":2}]`,
			`[{"rail":0,"device":"a","port":1,"future":` + strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64) + "}]",
		},
	} {
		for _, value := range values {
			t.Run(field+"/"+value, func(t *testing.T) {
				node := memberNode()
				node.Annotations = map[string]string{field: value}

				got, err := ParseAnnotations(&node)
				if !errors.Is(err, wire.InvalidRequest) || !reflect.DeepEqual(got, MemberAttributes{}) {
					t.Fatalf("invalid update produced %#v, %v", got, err)
				}
			})
		}
	}

	if _, err := ParseAnnotations(nil); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("nil node: %v", err)
	}

	node := memberNode()

	node.Annotations = map[string]string{wire.RDMANICsAnnotation: strings.Repeat(" ", 256*1024+1)}
	if _, err := ParseAnnotations(&node); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("oversized rails: %v", err)
	}
}

func TestSelectEndpoint(t *testing.T) {
	pods := []corev1.Pod{
		memberPod("a", 1, "192.0.2.1"), memberPod("z", 2, "2001:db8::1"), memberPod("b", 2, "192.0.2.2"),
	}
	// The newest Pod need not be Ready; a ready older Pod is not preferred.
	pods[0].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	for range 2 {
		got, err := selectEndpoint(pods, memberOwnership(t, testDaemonSetUID), "node-a", 7443)
		if err != nil || got != "[2001:db8::1]:7443" {
			t.Fatalf("got %q, %v", got, err)
		}

		slices.Reverse(pods)
	}

	for name, mutate := range map[string]func(*corev1.Pod){
		"terminating":      func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{} },
		"other node":       func(p *corev1.Pod) { p.Spec.NodeName = "node-b" },
		"unassigned":       func(p *corev1.Pod) { p.Spec.NodeName = "" },
		"missing uid":      func(p *corev1.Pod) { p.UID = "" },
		"no owner":         func(p *corev1.Pod) { p.OwnerReferences = nil },
		"other owner":      func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"not controller":   func(p *corev1.Pod) { p.OwnerReferences[0].Controller = nil },
		"false controller": func(p *corev1.Pod) { *p.OwnerReferences[0].Controller = false },
		"wrong kind":       func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "ReplicaSet" },
		"wrong api":        func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "other/v1" },
		"wrong namespace":  func(p *corev1.Pod) { p.Namespace = "other" },
		"wrong owner name": func(p *corev1.Pod) { p.OwnerReferences[0].Name = "other" },
		"no ip":            func(p *corev1.Pod) { p.Status.PodIP = "" },
		"hostname":         func(p *corev1.Pod) { p.Status.PodIP = "example.com" },
		"ip with port":     func(p *corev1.Pod) { p.Status.PodIP = "192.0.2.9:7443" },
		"ip with zone":     func(p *corev1.Pod) { p.Status.PodIP = "fe80::1%eth0" },
	} {
		t.Run(name, func(t *testing.T) {
			ineligible := memberPod("new", 10, "192.0.2.9")
			mutate(&ineligible)

			if _, err := selectEndpoint([]corev1.Pod{ineligible}, memberOwnership(t, testDaemonSetUID), "node-a", 7443); !errors.Is(err, wire.Unavailable) {
				t.Fatalf("ineligible Pod admitted: %v", err)
			}

			got, err := selectEndpoint([]corev1.Pod{ineligible, memberPod("old", 1, "192.0.2.1")}, memberOwnership(t, testDaemonSetUID), "node-a", 7443)
			if err != nil || got != "192.0.2.1:7443" {
				t.Fatalf("eligible older Pod lost: %q, %v", got, err)
			}
		})
	}

	for _, tc := range []struct {
		uid  types.UID
		node string
		port uint16
	}{
		{testDaemonSetUID, "", 7443}, {testDaemonSetUID, "node-a", 0},
	} {
		if _, err := selectEndpoint(pods, memberOwnership(t, tc.uid), tc.node, tc.port); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("invalid endpoint configuration: %v", err)
		}
	}

	if _, err := selectEndpoint(pods, DataplaneWorkloadIdentities{}, "node-a", 7443); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("missing workload must have no eligible endpoint: %v", err)
	}
}

func TestReconcileMembersColdStartAndRetention(t *testing.T) {
	node := memberNode()
	pod := memberPod("a", 1, "192.0.2.1")
	initial, diagnostics, err := reconcileMembers([]corev1.Node{node}, map[string][]corev1.Pod{node.Name: {pod}}, memberOwnership(t, testDaemonSetUID), nil, 7443)

	want := wire.Member{Node: testNodeUID, Shares: 4, RDMANICs: []wire.RDMANIC{}, PeerEndpoint: "192.0.2.1:7443"}
	if err != nil || len(diagnostics) != 0 || !reflect.DeepEqual(initial[testNodeUID], want) {
		t.Fatalf("cold start: %#v, %v, %v", initial, diagnostics, err)
	}

	for _, tc := range []struct {
		name        string
		annotations map[string]string
		pods        []corev1.Pod
		shares      uint32
		endpoint    string
		diagnostics int
	}{
		{"pod gap", map[string]string{wire.SharesAnnotation: "8"}, nil, 8, "192.0.2.1:7443", 1},
		{"invalid annotations", map[string]string{wire.SharesAnnotation: "0"}, []corev1.Pod{memberPod("b", 2, "192.0.2.2")}, 4, "192.0.2.2:7443", 1},
		{"both missing", map[string]string{wire.SharesAnnotation: "0"}, nil, 4, "192.0.2.1:7443", 2},
		{"annotation unit", map[string]string{wire.SharesAnnotation: "8", wire.RDMANICsAnnotation: "invalid"}, []corev1.Pod{pod}, 4, "192.0.2.1:7443", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := node.DeepCopy()
			node.Annotations = tc.annotations

			got, diagnostics, err := reconcileMembers([]corev1.Node{*node}, map[string][]corev1.Pod{node.Name: tc.pods}, memberOwnership(t, testDaemonSetUID), initial, 7443)
			if err != nil || len(diagnostics) != tc.diagnostics || got[testNodeUID].Shares != tc.shares || got[testNodeUID].PeerEndpoint != tc.endpoint {
				t.Fatalf("warm reconcile: %#v, %v, %v", got, diagnostics, err)
			}

			if initial[testNodeUID].Shares != 4 || initial[testNodeUID].PeerEndpoint != "192.0.2.1:7443" {
				t.Fatal("mutated accepted input")
			}

			cold, diagnostics, err := reconcileMembers([]corev1.Node{*node}, map[string][]corev1.Pod{node.Name: tc.pods}, memberOwnership(t, testDaemonSetUID), nil, 7443)
			if err != nil || len(cold) != 0 || len(diagnostics) != tc.diagnostics {
				t.Fatalf("cold reconcile: %#v, %v, %v", cold, diagnostics, err)
			}

			for _, d := range diagnostics {
				if d.Object != node.Name || d.Field == "" || d.Reason == "" {
					t.Fatalf("unactionable diagnostic: %#v", d)
				}
			}
		})
	}
}

func TestReconcileMembersIdentityAndRemoval(t *testing.T) {
	node := memberNode()
	pod := memberPod("a", 1, "192.0.2.1")

	pods := map[string][]corev1.Pod{node.Name: {pod}}

	accepted, _, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	for _, label := range []string{"", "false", "true"} {
		node.Labels = map[string]string{wire.ExclusionLabel: label}

		got, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
		if err != nil || len(got) != 0 || len(diagnostics) != 0 {
			t.Fatalf("exclusion: %v, %v, %v", got, diagnostics, err)
		}

		node.Labels = nil

		got, _, err = reconcileMembers([]corev1.Node{node}, nil, memberOwnership(t, testDaemonSetUID), got, 7443)
		if err != nil || len(got) != 0 {
			t.Fatalf("exclusion history survived: %v, %v", got, err)
		}
	}

	got, _, err := reconcileMembers(nil, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("deletion: %v, %v", got, err)
	}

	node.UID = testOtherUID

	got, _, err = reconcileMembers([]corev1.Node{node}, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil || len(got) != 0 {
		t.Fatalf("same-name recreation inherited history: %v, %v", got, err)
	}

	got, _, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil || len(got) != 1 || got[testOtherUID].Node != testOtherUID {
		t.Fatalf("new UID not admitted: %v, %v", got, err)
	}
	// Node readiness and deletion timestamps do not change ownership while the
	// Node remains in the Kubernetes input and is not explicitly excluded.
	node.DeletionTimestamp = &metav1.Time{}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}

	got, _, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), got, 7443)
	if err != nil || len(got) != 1 {
		t.Fatalf("readiness removed ownership: %v, %v", got, err)
	}
}

func TestReconcileMembersDefaultsAndIsolation(t *testing.T) {
	node := memberNode()
	node.Annotations = map[string]string{wire.SharesAnnotation: "8", wire.RDMANICsAnnotation: `[{"rail":0,"device":"a","port":1,"numa_node":1}]`}
	pod := memberPod("a", 1, "192.0.2.1")

	pods := map[string][]corev1.Pod{node.Name: {pod}}

	accepted, _, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	pods[node.Name][0].Status.PodIP = "192.0.2.9"
	*pods[node.Name][0].OwnerReferences[0].Controller = false
	delete(pods, node.Name)
	node.Annotations[wire.SharesAnnotation] = "invalid"

	if accepted[testNodeUID].PeerEndpoint != "192.0.2.1:7443" || accepted[testNodeUID].Shares != 8 {
		t.Fatal("candidate aliases Kubernetes inputs")
	}

	got, _, err := reconcileMembers([]corev1.Node{node}, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil || !reflect.DeepEqual(got, accepted) {
		t.Fatalf("retention: %v, %v", got, err)
	}

	got[testNodeUID].RDMANICs[0].Device = "changed"
	*got[testNodeUID].RDMANICs[0].NUMANode = 9
	delete(got, testNodeUID)

	if accepted[testNodeUID].RDMANICs[0].Device != "a" || *accepted[testNodeUID].RDMANICs[0].NUMANode != 1 {
		t.Fatal("retention aliases accepted state")
	}

	got, _, err = reconcileMembers([]corev1.Node{node}, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil {
		t.Fatal(err)
	}

	accepted[testNodeUID].RDMANICs[0].Device = "changed input"

	*accepted[testNodeUID].RDMANICs[0].NUMANode = 7
	if got[testNodeUID].RDMANICs[0].Device != "a" || *got[testNodeUID].RDMANICs[0].NUMANode != 1 {
		t.Fatal("accepted input mutation changed retained output")
	}

	node.Annotations = nil

	got, _, err = reconcileMembers([]corev1.Node{node}, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil || got[testNodeUID].Shares != 4 || len(got[testNodeUID].RDMANICs) != 0 {
		t.Fatalf("removed annotations did not default: %v, %v", got, err)
	}
}

func TestReconcileMembersRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []corev1.Node
		uid   types.UID
		port  uint16
	}{
		{"missing port", nil, testDaemonSetUID, 0},
		{"duplicate node", []corev1.Node{memberNode(), memberNode()}, testDaemonSetUID, 7443},
		{"missing uid", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}, testDaemonSetUID, 7443},
		{"malformed uid", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "invalid"}}}, testDaemonSetUID, 7443},
		{"missing name", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{UID: testNodeUID}}}, testDaemonSetUID, 7443},
		{"duplicate name", []corev1.Node{memberNode(), {ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: testOtherUID}}}, testDaemonSetUID, 7443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := reconcileMembers(tc.nodes, nil, memberOwnership(t, tc.uid), nil, tc.port)
			if !errors.Is(err, wire.InvalidRequest) || got != nil {
				t.Fatalf("invalid inputs accepted: %v, %v", got, err)
			}
		})
	}
}

func TestReconcileMembersGroupedPods(t *testing.T) {
	nodeA, nodeB := memberNode(), memberNode()
	nodeB.Name, nodeB.UID = "node-b", testOtherUID
	nodeB.Annotations = map[string]string{wire.SharesAnnotation: "invalid"}
	podA := memberPod("a", 1, "192.0.2.1")
	wrongNode := memberPod("wrong-node", 3, "192.0.2.3")
	wrongNode.Spec.NodeName = nodeB.Name
	wrongOwner := memberPod("wrong-owner", 4, "192.0.2.4")
	wrongOwner.OwnerReferences[0].UID = "old-daemonset"
	nodes := []corev1.Node{nodeB, nodeA}
	pods := map[string][]corev1.Pod{
		nodeA.Name: {wrongNode, podA, wrongOwner},
		nodeB.Name: {podA},
		"absent":   {wrongNode},
	}

	podsBefore := make(map[string][]corev1.Pod, len(pods))
	for name, group := range pods {
		for _, pod := range group {
			podsBefore[name] = append(podsBefore[name], *pod.DeepCopy())
		}
	}

	var firstDiagnostics []Diagnostic

	for range 2 {
		members, diagnostics, err := reconcileMembers(nodes, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
		if err != nil || len(members) != 1 || members[testNodeUID].PeerEndpoint != "192.0.2.1:7443" {
			t.Fatalf("grouped endpoint checks: %v, %v", members, err)
		}

		if len(diagnostics) != 2 || diagnostics[0].Object != nodeB.Name || diagnostics[0].Field != "annotations" || diagnostics[1].Object != nodeB.Name || diagnostics[1].Field != "peer_endpoint" {
			t.Fatalf("grouped diagnostics: %v", diagnostics)
		}

		if firstDiagnostics != nil && !reflect.DeepEqual(diagnostics, firstDiagnostics) {
			t.Fatalf("input order changed diagnostics: %v, %v", diagnostics, firstDiagnostics)
		}

		firstDiagnostics = diagnostics

		if !reflect.DeepEqual(pods, podsBefore) {
			t.Fatal("mutated grouped Pod inputs")
		}

		slices.Reverse(nodes)
		slices.Reverse(pods[nodeA.Name])
		slices.Reverse(podsBefore[nodeA.Name])
	}

	// Multiple rejected nodes must report in UID order, not input or map order.
	nodeA.Annotations = nodeB.Annotations
	for _, nodes := range [][]corev1.Node{{nodeB, nodeA}, {nodeA, nodeB}} {
		_, diagnostics, err := reconcileMembers(nodes, nil, memberOwnership(t, testDaemonSetUID), nil, 7443)
		if err != nil || len(diagnostics) != 4 || diagnostics[0].Object != nodeA.Name || diagnostics[1].Object != nodeA.Name || diagnostics[2].Object != nodeB.Name || diagnostics[3].Object != nodeB.Name {
			t.Fatalf("node diagnostic order: %v, %v", diagnostics, err)
		}
	}
}

func TestReconcileCandidateHashesAndOrdering(t *testing.T) {
	nodeA, nodeB := memberNode(), memberNode()
	nodeB.Name, nodeB.UID = "node-b", testOtherUID
	nodeA.Annotations = map[string]string{wire.RDMANICsAnnotation: `[{"rail":1,"device":"b","port":1},{"rail":1,"device":"a","port":1}]`}
	podA, podB := memberPod("a", 1, "192.0.2.1"), memberPod("b", 1, "192.0.2.2")
	podB.Spec.NodeName = "node-b"
	nodes := []corev1.Node{nodeB, nodeA}
	pods := map[string][]corev1.Pod{nodeA.Name: {podA}, nodeB.Name: {podB}}
	nodesBefore := []corev1.Node{*nodeB.DeepCopy(), *nodeA.DeepCopy()}
	podsBefore := map[string][]corev1.Pod{nodeA.Name: {*podA.DeepCopy()}, nodeB.Name: {*podB.DeepCopy()}}

	members, diagnostics, err := reconcileMembers(nodes, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	if err != nil || len(diagnostics) != 0 || !reflect.DeepEqual(nodes, nodesBefore) || !reflect.DeepEqual(pods, podsBefore) {
		t.Fatalf("candidate failed or mutated inputs: %v, %v", diagnostics, err)
	}

	candidate := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: testNodeUID, Members: []wire.Member{members[testOtherUID], members[testNodeUID]}}

	content, membership, err := wire.ContentHashes(candidate)
	if err != nil {
		t.Fatal(err)
	}

	slices.Reverse(nodes)

	pods = map[string][]corev1.Pod{nodeB.Name: {podB}, nodeA.Name: {podA}}

	nodes[0].Annotations[wire.RDMANICsAnnotation] = `[{"rail":1,"device":"a","port":1},{"rail":1,"device":"b","port":1}]`

	members, _, err = reconcileMembers(nodes, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	candidate.Members = []wire.Member{members[testNodeUID], members[testOtherUID]}

	contentAgain, membershipAgain, err := wire.ContentHashes(candidate)
	if err != nil || contentAgain != content || membershipAgain != membership {
		t.Fatalf("order/dedup changed hashes: %v", err)
	}

	candidate.Caches, err = BuildCatalog([]racerv1.ClusterCache{catalogCache("cache-a", testNodeUID)})
	if err != nil {
		t.Fatal(err)
	}

	contentAgain, membershipAgain, err = wire.ContentHashes(candidate)
	if err != nil || contentAgain == content || membershipAgain != membership {
		t.Fatalf("cache-only hash change: %v", err)
	}

	candidate.Members[0].PeerEndpoint = "192.0.2.9:7443"

	_, membershipAgain, err = wire.ContentHashes(candidate)
	if err != nil || membershipAgain == membership {
		t.Fatalf("endpoint must change membership hash: %v", err)
	}
}

func TestReconcileMembersLimit(t *testing.T) {
	nodes := make([]corev1.Node, wire.MaxMembers+1)

	accepted := make(AcceptedMembers, len(nodes))
	for i := range nodes {
		id := fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)
		nodes[i] = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: id, UID: types.UID(id)}}
		accepted[wire.NodeID(id)] = wire.Member{Node: wire.NodeID(id), Shares: 4, PeerEndpoint: "192.0.2.1:7443", RDMANICs: []wire.RDMANIC{}}
	}

	got, _, err := reconcileMembers(nodes, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if !errors.Is(err, wire.TooLarge) || got != nil {
		t.Fatalf("oversized membership: %d, %v", len(got), err)
	}
	// The bound is on admitted members, not all observed Nodes.
	nodes[0].Labels = map[string]string{wire.ExclusionLabel: ""}

	got, _, err = reconcileMembers(nodes, nil, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	if err != nil || len(got) != wire.MaxMembers {
		t.Fatalf("membership at bound: %d, %v", len(got), err)
	}
}

func TestReconcileMembersMissingWorkloadAndRecovery(t *testing.T) {
	empty, diagnostics, err := reconcileMembers(nil, nil, DataplaneWorkloadIdentities{}, nil, 7443)
	if err != nil || empty == nil || len(empty) != 0 || len(diagnostics) != 0 {
		t.Fatalf("empty initial reconcile: %#v, %v, %v", empty, diagnostics, err)
	}

	node := memberNode()
	pods := map[string][]corev1.Pod{node.Name: {memberPod("a", 1, "192.0.2.1")}}

	accepted, _, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	var got AcceptedMembers
	for _, uid := range []types.UID{"", "replacement-daemonset"} {
		got, diagnostics, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, uid), accepted, 7443)
		if err != nil || !reflect.DeepEqual(got, accepted) || len(diagnostics) != 1 {
			t.Fatalf("workload gap: %v, %v, %v", got, diagnostics, err)
		}

		got, diagnostics, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, uid), nil, 7443)
		if err != nil || len(got) != 0 || len(diagnostics) != 1 {
			t.Fatalf("cold workload gap: %v, %v, %v", got, diagnostics, err)
		}
	}

	node.Annotations = map[string]string{wire.SharesAnnotation: "invalid-sensitive-input"}

	cold, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	if err != nil || len(cold) != 0 || len(diagnostics) != 1 || strings.Contains(diagnostics[0].Reason, "invalid-sensitive-input") {
		t.Fatalf("cold invalid annotations: %v, %v, %v", cold, diagnostics, err)
	}

	node.Annotations[wire.SharesAnnotation] = "16"

	got, diagnostics, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), cold, 7443)
	if err != nil || got[testNodeUID].Shares != 16 || len(diagnostics) != 0 {
		t.Fatalf("corrected inputs not admitted: %v, %v, %v", got, diagnostics, err)
	}
}

func TestTopologyIndexedPodGroups(t *testing.T) {
	for _, stage := range []string{"success", "list error", "canceled list"} {
		t.Run(stage, func(t *testing.T) {
			nodeA, nodeB := memberNode(), memberNode()
			nodeB.Name, nodeB.UID = "node-b", testOtherUID
			podA, podB := memberPod("a", 1, "192.0.2.1"), memberPod("b", 1, "192.0.2.2")
			podB.Spec.NodeName = nodeB.Name
			r := initializedTopology(t, &nodeA, &nodeB, &podA, &podB, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: "racer-dataplane", Namespace: "racer", UID: testDaemonSetUID,
			}})
			r.Config.PeerPort = 7443

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			boom := errors.New("pod list failed")
			queries := map[string]int{}
			writes := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						options := (&client.ListOptions{}).ApplyOptions(opts)
						if options.Namespace != r.Config.Namespace || options.FieldSelector == nil {
							t.Fatalf("Pod query lacks namespace or node index: %+v", options)
						}

						name, exact := options.FieldSelector.RequiresExactMatch(podNodeIndex)
						if !exact || (name != nodeA.Name && name != nodeB.Name) {
							t.Fatalf("unexpected Pod node selector: %v", options.FieldSelector)
						}

						queries[name]++

						if stage == "list error" {
							return boom
						}

						if stage == "canceled list" {
							defer cancel()
						}
					}

					return c.List(ctx, list, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
			})

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if stage == "success" {
				if err != nil || result.RequeueAfter != 0 || queries[nodeA.Name] != 1 || queries[nodeB.Name] != 1 || len(acceptedMembers(t, r)) != 2 || acceptedMembers(t, r)[testNodeUID].PeerEndpoint != "192.0.2.1:7443" || acceptedMembers(t, r)[testOtherUID].PeerEndpoint != "192.0.2.2:7443" {
					t.Fatalf("indexed groups: queries=%v members=%v result=%v err=%v", queries, acceptedMembers(t, r), result, err)
				}

				return
			}

			wantErr := boom
			if stage == "canceled list" {
				wantErr = context.Canceled

				if !errors.Is(err, reconcile.TerminalError(nil)) {
					t.Fatalf("canceled list is not terminal: %v", err)
				}
			}

			if !errors.Is(err, wantErr) || result.RequeueAfter != 0 || len(queries) != 1 || writes != 0 || len(acceptedMembers(t, r)) != 0 {
				t.Fatalf("failed listing changed state or continued: queries=%v writes=%d members=%v result=%v err=%v", queries, writes, acceptedMembers(t, r), result, err)
			}

			if _, err := r.authority.Current(); err == nil {
				t.Fatal("installed publication after failed listing")
			}
		})
	}
}

// Exercise the same ownership snapshot through discovery, durable publication,
// key admission, and restart rather than injecting an endpoint callback.
func TestTopologyOwnershipHistoryAndCatalogRestart(t *testing.T) {
	for _, workload := range []string{DataplaneDaemonSetName, PodNetworkDaemonSetName, "standalone-racer"} {
		t.Run(workload, func(t *testing.T) {
			node := memberNode()
			node.Annotations = map[string]string{wire.SharesAnnotation: "8"}
			pod := memberPod("current", 1, "192.0.2.1")
			pod.OwnerReferences[0].Name = workload
			ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: workload, UID: testDaemonSetUID}}

			r := initializedTopology(t, &node, &pod, ds)
			if workload == "standalone-racer" {
				r.Config.DaemonSetName = workload
			}

			r.Config.PeerPort = 7443
			a := Assemble(r.Config, r.Client, r.APIReader)
			r = a.Topology
			runKeys(t, a.Keyring)
			first := reconcileTopology(t, r, t.Context())
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			history := node.Annotations[admittedMemberAnnotation]
			require.NotEmpty(t, history)

			cache := catalogCache("cache-a", testOtherUID)
			require.NoError(t, r.Create(t.Context(), &cache))
			require.Equal(t, first.encoded, reconcileTopology(t, r, t.Context()).encoded, "cache waits for committed keys")
			runKeys(t, a.Keyring)
			withCache := reconcileTopology(t, r, t.Context())
			published, err := wire.DecodePublication(strings.NewReader(withCache.encoded))
			require.NoError(t, err)
			require.Equal(t, []wire.CacheDefinition{{ID: testOtherUID, Name: "cache-a", ClientSocket: "/run/racer/cache-a/client/socket", OriginSocket: "/run/racer/cache-a/origin/socket"}}, published.Caches)
			require.Len(t, published.Members, 1)
			require.Equal(t, first.record.MembershipVersion, withCache.record.MembershipVersion)

			// The workload was recreated while the old Pod still exists. A fresh
			// process must recover history, not admit that Pod under its stale UID.
			require.NoError(t, r.Delete(t.Context(), ds))
			ds.UID, ds.ResourceVersion = "replacement", ""
			require.NoError(t, r.Create(t.Context(), ds))

			node.Annotations[wire.SharesAnnotation] = "malformed"
			require.NoError(t, r.Update(t.Context(), &node))
			a = Assemble(r.Config, r.Client, r.APIReader)
			r = a.Topology
			restarted := reconcileTopology(t, r, t.Context())
			require.Equal(t, withCache.encoded, restarted.encoded)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			require.Equal(t, history, node.Annotations[admittedMemberAnnotation])

			// A new owned endpoint recovers independently of malformed attributes.
			replacement := memberPod("replacement", 2, "2001:db8::2")
			replacement.OwnerReferences[0] = *metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))
			require.NoError(t, r.Create(t.Context(), &replacement))
			recovered := reconcileTopology(t, r, t.Context())
			published, err = wire.DecodePublication(strings.NewReader(recovered.encoded))
			require.NoError(t, err)
			require.Equal(t, "[2001:db8::2]:7443", published.Members[0].PeerEndpoint)
			require.Equal(t, uint32(8), published.Members[0].Shares)
			require.Len(t, published.Caches, 1)

			// Whole-candidate rejection leaves both publication and Node history
			// unchanged even when there is a valid membership update to publish.
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			history = node.Annotations[admittedMemberAnnotation]
			node.Annotations[wire.SharesAnnotation] = "16"
			require.NoError(t, r.Update(t.Context(), &node))

			invalid := catalogCache("cache-b", "not-a-uuid")
			require.NoError(t, r.Create(t.Context(), &invalid))
			_, err = r.Reconcile(t.Context(), ctrl.Request{})
			require.ErrorIs(t, err, wire.InvalidRequest)
			current, err := r.authority.Current()
			require.NoError(t, err)
			require.Equal(t, recovered.encoded, captureHandle(t, current).encoded)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			require.Equal(t, history, node.Annotations[admittedMemberAnnotation])

			require.NoError(t, r.Delete(t.Context(), &invalid))
			require.NoError(t, r.Delete(t.Context(), &cache))

			node.Labels = map[string]string{wire.ExclusionLabel: ""}
			require.NoError(t, r.Update(t.Context(), &node))
			excluded := reconcileTopology(t, r, t.Context())
			published, err = wire.DecodePublication(strings.NewReader(excluded.encoded))
			require.NoError(t, err)
			require.Empty(t, published.Members)
			require.Empty(t, published.Caches)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
			require.Empty(t, node.Annotations[admittedMemberAnnotation])
		})
	}
}

func TestTopologyCustomWorkloadOwnership(t *testing.T) {
	for _, scenario := range []string{"current", "wrong name", "stale UID", "wrong namespace", "wrong kind", "wrong API version", "not controller", "labels only", "missing workload", "deleting workload"} {
		t.Run(scenario, func(t *testing.T) {
			node := memberNode()
			pod := memberPod("custom", 1, "192.0.2.1")
			pod.OwnerReferences[0].Name = "standalone-racer"
			ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "standalone-racer", UID: testDaemonSetUID}}

			switch scenario {
			case "wrong name":
				pod.OwnerReferences[0].Name = DataplaneDaemonSetName
			case "stale UID":
				pod.OwnerReferences[0].UID = "stale"
			case "wrong namespace":
				pod.Namespace = "other"
			case "wrong kind":
				pod.OwnerReferences[0].Kind = "Deployment"
			case "wrong API version":
				pod.OwnerReferences[0].APIVersion = "apps/v2"
			case "not controller":
				pod.OwnerReferences[0].Controller = nil
			case "labels only":
				pod.OwnerReferences = nil
				pod.Labels = map[string]string{"app.kubernetes.io/name": ds.Name}
			case "deleting workload":
				ds.Finalizers = []string{"test/hold"}
			}

			r := initializedTopology(t, &node, &pod, ds)

			r.Config.DaemonSetName = ds.Name
			if scenario == "missing workload" || scenario == "deleting workload" {
				require.NoError(t, r.Delete(t.Context(), ds))
			}

			committed := reconcileTopology(t, r, t.Context())
			published, err := wire.DecodePublication(strings.NewReader(committed.encoded))
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))

			if scenario == "current" {
				require.Len(t, published.Members, 1)
				require.NotEmpty(t, node.Annotations[admittedMemberAnnotation])
			} else {
				require.Empty(t, published.Members)
				require.Empty(t, node.Annotations[admittedMemberAnnotation])
			}
		})
	}
}

// Test-local vocabulary keeps the original membership scenarios readable without
// re-exporting the extracted packages through the production controller API.
type (
	AcceptedMembers  = members.History
	MemberAttributes = members.MemberAttributes
	Diagnostic       = members.Diagnostic
)

const (
	DataplaneDaemonSetName  = members.DataplaneDaemonSetName
	PodNetworkDaemonSetName = members.PodNetworkDaemonSetName
)

func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool {
	return ids.observed().Owns(pod)
}

func ParseAnnotations(node *corev1.Node) (MemberAttributes, error) {
	return members.ParseAnnotations(node)
}

func selectEndpoint(pods []corev1.Pod, ownership DataplaneWorkloadIdentities, nodeName string, port uint16) (string, error) {
	return members.SelectEndpoint(pods, ownership.observed(), nodeName, port)
}

func reconcileMembers(nodes []corev1.Node, podsByNode map[string][]corev1.Pod, ownership DataplaneWorkloadIdentities, accepted AcceptedMembers, port uint16) (AcceptedMembers, []Diagnostic, error) {
	result, err := members.Reconcile(members.Input{
		Nodes: nodes, PodsByNode: podsByNode, Ownership: ownership.observed(), PeerPort: port,
	}, accepted)

	return result.Members, result.Diagnostics, err
}

func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return members.BuildCatalog(caches)
}

func TestLegacyRDMADiagnosticsAndMalformedUnitRetention(t *testing.T) {
	node := memberNode()
	node.Annotations = map[string]string{wire.RailsAnnotation: "malformed", wire.AlignmentAnnotation: "false"}
	pods := map[string][]corev1.Pod{node.Name: {memberPod("a", 1, "192.0.2.1")}}
	accepted, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), nil, 7443)
	require.NoError(t, err)
	require.Len(t, diagnostics, 2)
	require.Empty(t, accepted[testNodeUID].RDMANICs)

	for _, diagnostic := range diagnostics {
		require.Contains(t, diagnostic.Reason, "legacy annotation ignored")
	}

	node.Annotations = map[string]string{wire.SharesAnnotation: "8", enrolledRDMANICsAnnotation: `[{"device":"a","port":1,"rail":0}]`}
	accepted, _, err = reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	require.NoError(t, err)

	node.Annotations[wire.SharesAnnotation] = "9"
	node.Annotations[wire.RDMANICsAnnotation] = "null"
	retained, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	require.Equal(t, accepted, retained)

	node.Annotations[wire.RDMANICsAnnotation] = "[]"
	attributes, err := ParseAnnotations(&node)
	require.NoError(t, err)
	require.Equal(t, uint32(9), attributes.Shares)
	require.Empty(t, attributes.RDMANICs)
}

func TestMixedControllerTopologyLiveOwnership(t *testing.T) {
	node := memberNode()
	pod := memberPod("podnet", 1, "192.0.2.2")
	pod.OwnerReferences[0].Name = PodNetworkDaemonSetName
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: testDaemonSetUID}}
	r := initializedTopology(t, &node, &pod, ds)
	reconcileTopology(t, r, t.Context())
	require.Len(t, acceptedMembers(t, r), 1)
	before := acceptedMembers(t, r)[testNodeUID]
	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.DaemonSet); ok {
				return errors.New("injected workload read failure")
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.Error(t, err)
	require.Equal(t, before, acceptedMembers(t, r)[testNodeUID])
	r.APIReader = r.Client
	require.NoError(t, r.Delete(t.Context(), ds))
	ds.UID, ds.ResourceVersion = "replacement", ""
	require.NoError(t, r.Create(t.Context(), ds))
	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, r, t.Context())
	require.Equal(t, before, acceptedMembers(t, r)[testNodeUID])
}

type mixedFailReader struct{ client.Reader }

func (r mixedFailReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("injected read failure")
}

func TestMixedControllerAuthorization(t *testing.T) {
	cfg := Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName, DataplaneServiceAccount: "racer"}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "racer", UID: "sa-current"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: PodNetworkDaemonSetName, UID: "pod-current"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ds).Build()
	pod := memberPod("pod", 1, "192.0.2.1")
	pod.Spec.ServiceAccountName = "racer"
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}
	require.NoError(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, "old-sa"), wire.Forbidden)
	require.ErrorIs(t, authorizePod(t.Context(), mixedFailReader{c}, cfg, &pod, string(sa.UID)), wire.Unavailable)
	ids, err := readManagedWorkloadIdentities(t.Context(), mixedFailReader{c}, cfg)
	require.Error(t, err)
	require.False(t, ids.Owns(&pod))

	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.OwnerReferences[0].Name = "arbitrary" },
		func(p *corev1.Pod) { p.OwnerReferences[0].UID = "stale" },
		func(p *corev1.Pod) {
			p.OwnerReferences = nil
			p.Labels = map[string]string{"app": DataplaneDaemonSetName}
		},
		func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		func(p *corev1.Pod) { p.Spec.ServiceAccountName = "other" },
		func(p *corev1.Pod) { p.UID = "" },
		func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{} },
	} {
		bad := pod.DeepCopy()
		mutate(bad)
		require.ErrorIs(t, authorizePod(t.Context(), c, cfg, bad, string(sa.UID)), wire.Forbidden)
	}

	require.NoError(t, c.Delete(t.Context(), ds))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)), wire.Forbidden)
	ds.ResourceVersion, ds.UID = "", "recreated"
	require.NoError(t, c.Create(t.Context(), ds))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)), wire.Forbidden)
	pod.OwnerReferences[0].UID = ds.UID
	require.NoError(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)))

	ds.Finalizers = []string{"test/hold"}
	require.NoError(t, c.Update(t.Context(), ds))
	require.NoError(t, c.Delete(t.Context(), ds))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)), wire.Forbidden)
}

func TestMixedControllerMembership(t *testing.T) {
	node := memberNode()
	node.Annotations = map[string]string{enrolledSharesAnnotation: "8"}
	host := memberPod("host", 1, "192.0.2.1")
	host.OwnerReferences[0].Name = DataplaneDaemonSetName
	pod := memberPod("pod", 2, "192.0.2.2")
	pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = PodNetworkDaemonSetName, "podnet"
	hostDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: DataplaneDaemonSetName, UID: testDaemonSetUID}}
	podDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "podnet"}}
	r := initializedTopology(t, &node, &host, hostDS, podDS)
	r.Config.PeerPort = 7443
	reconcileTopology(t, r, t.Context())
	require.NoError(t, r.Create(t.Context(), &pod))
	reconcileTopology(t, r, t.Context())
	after := acceptedMembers(t, r)
	require.Len(t, after, 1)
	require.Equal(t, uint32(8), after[testNodeUID].Shares)
	require.Equal(t, "192.0.2.2:7443", after[testNodeUID].PeerEndpoint)

	require.NoError(t, r.Delete(t.Context(), &host))
	require.NoError(t, r.Delete(t.Context(), podDS))
	podDS.UID, podDS.ResourceVersion = "recreated", ""
	require.NoError(t, r.Create(t.Context(), podDS))
	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, r, t.Context())
	require.Equal(t, after, acceptedMembers(t, r), "restart retains UID-bound last admitted endpoint")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
	delete(node.Annotations, admittedMemberAnnotation)
	require.NoError(t, r.Update(t.Context(), &node))
	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, r, t.Context())
	require.Empty(t, acceptedMembers(t, r), "stale owner cannot admit a new member")

	pod.OwnerReferences[0].UID = hostDS.UID
	pod.OwnerReferences[0].Name = "arbitrary"
	require.NoError(t, r.Update(t.Context(), &pod))
	reconcileTopology(t, r, t.Context())
	require.Empty(t, acceptedMembers(t, r), "a live UID with the wrong owner name cannot admit a member")
}

func TestTrustRequiresFreshPostReconcileCredentials(t *testing.T) {
	for _, resource := range []string{"racer-installation", "racer-version", "issuer.json", "bundle.json"} {
		for _, failure := range []string{"outage", "deleted", "malformed"} {
			t.Run(resource+"/"+failure, func(t *testing.T) {
				resourceName := resource
				if resource == "issuer.json" || resource == "bundle.json" {
					resourceName = "racer-credentials"
				}

				r, now := testKeyring(t)
				runKeys(t, r)
				_, _, initial, _ := keyState(t, r)
				*now = initial.NextRotation

				accepted, err := r.authority.TrustPool()
				if err != nil {
					t.Fatal(err)
				}

				acceptedBundle, err := r.authority.Keyring()
				if err != nil || acceptedBundle.Generation() != 1 {
					t.Fatalf("initial delivery state: %v", err)
				}

				reads := 0
				d := fixtureDependencies[r.authority]
				d.reader = interceptor.NewClient(d.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == resourceName {
						reads++
						if reads == 2 {
							switch failure {
							case "outage":
								return errors.New("post-reconcile API outage")
							case "deleted":
								return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
							case "malformed":
								if err := c.Get(ctx, key, obj, opts...); err != nil {
									return err
								}

								switch value := obj.(type) {
								case *corev1.Secret:
									value.Data = nil
								case *corev1.ConfigMap:
									value.Data = nil
								}

								return nil
							}
						}
					}

					return c.Get(ctx, key, obj, opts...)
				}})

				if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err == nil || reads != 2 {
					t.Fatalf("post-reconcile failure bypassed: %v, reads=%d", err, reads)
				}

				current, err := r.authority.TrustPool()
				if failure == "outage" {
					if err != nil || !current.Equal(accepted) || r.authority.TrustReady() != nil {
						t.Fatalf("read outage replaced accepted trust with candidate roots: %v", err)
					}
				} else if err == nil || r.authority.TrustReady() == nil {
					t.Fatal("observed invalid authority retained or installed trust")
				}

				currentBundle, bundleErr := r.authority.Keyring()
				if failure == "outage" {
					if bundleErr != nil || currentBundle != acceptedBundle {
						t.Fatalf("read outage exposed candidate delivery state: %v", bundleErr)
					}
				} else if bundleErr == nil {
					t.Fatal("observed invalid authority retained delivery state")
				}

				d.reader = d.Client

				_, staged, _, _ := keyState(t, r)
				if staged.Generation != 2 || len(staged.PeerTrustRoots) != 2 {
					t.Fatal("failure preceded successful rotation publication")
				}

				runKeys(t, r)

				current, err = r.authority.TrustPool()
				if err != nil || current.Equal(accepted) {
					t.Fatalf("fresh successful reconciliation did not install staged trust: %v", err)
				}

				currentBundle, bundleErr = r.authority.Keyring()
				if bundleErr != nil || currentBundle.Generation() != 2 {
					t.Fatalf("committed delivery not installed with trust: %v", bundleErr)
				}
			})
		}
	}
}

func TestKeyringCancellationOverridesPostReconcileReadFailure(t *testing.T) {
	for _, failure := range []string{"outage", "conflict", "success"} {
		t.Run(failure, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reads := 0
			d := fixtureDependencies[r.authority]
			d.reader = interceptor.NewClient(d.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == r.Config.CredentialsSecretName {
					reads++
					if reads == 2 {
						defer cancel()

						switch failure {
						case "outage":
							return errors.New("post-reconcile API outage")
						case "conflict":
							return apierrors.NewConflict(corev1.Resource("secrets"), key.Name, wire.Conflict)
						}
					}
				}

				return c.Get(ctx, key, obj, opts...)
			}})

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if reads != 2 || !errors.Is(err, context.Canceled) || !errors.Is(err, reconcile.TerminalError(nil)) || result != (ctrl.Result{}) {
				t.Fatalf("post-reconcile cancellation: reads=%d result=%v err=%v", reads, result, err)
			}

			if err := r.authority.TrustReady(); err == nil {
				t.Fatal("cancellation after admission retained trust or issuer readiness")
			}

			if _, err := r.authority.Keyring(); err == nil {
				t.Fatal("cancellation after admission retained delivery")
			}

			d.reader = d.Client

			_, staged, _, _ := keyState(t, r)
			if staged.Generation != 2 || len(staged.PeerTrustRoots) != 2 {
				t.Fatal("cancellation preceded successful rotation publication")
			}
		})
	}
}

func TestReconcilerAlreadyExistsHandling(t *testing.T) {
	for _, operation := range []string{"keyring", "topology"} {
		t.Run(operation, func(t *testing.T) {
			r, now := testKeyring(t)
			runKeys(t, r)
			_, _, initial, _ := keyState(t, r)
			*now = initial.NextRotation

			accepted, err := r.authority.TrustPool()
			if err != nil {
				t.Fatal(err)
			}

			writes := 0
			d := fixtureDependencies[r.authority]
			writer := interceptor.NewClient(d.Client.(client.WithWatch), interceptor.Funcs{Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				writes++
				return apierrors.NewAlreadyExists(corev1.Resource("secrets"), obj.GetName())
			}})

			var result ctrl.Result

			if operation == "keyring" {
				d.Client = writer

				result, err = r.Reconcile(t.Context(), ctrl.Request{})
				if err != nil || result.RequeueAfter != retryConflictDelay {
					t.Fatalf("keyring AlreadyExists not requeued: %v %v", result, err)
				}

				if err := r.authority.TrustReady(); err == nil {
					t.Fatal("keyring write failure retained trust or issuer readiness")
				}
			} else {
				topology := Assemble(r.Config, writer, d.reader).Topology

				result, err = topology.Reconcile(t.Context(), ctrl.Request{})
				if !apierrors.IsAlreadyExists(err) || result != (ctrl.Result{}) {
					t.Fatalf("topology AlreadyExists treated as Conflict: %v %v", result, err)
				}

				if current, err := r.authority.TrustPool(); err != nil || !current.Equal(accepted) {
					t.Fatalf("topology publication write failure changed trust: %v", err)
				}
			}

			if writes != 1 {
				t.Fatalf("expected one failed write, got %d", writes)
			}
		})
	}
}

func TestTopologyAnnotationDoesNotBlockObserver(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				r := f.a.Topology

				var node corev1.Node
				require.NoError(t, r.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
				node.Annotations["racer.unbounded-cloud.io/shares"] = "7"
				require.NoError(t, r.Update(f.ctx, &node))

				entered, release := make(chan struct{}), make(chan struct{})
				patchError := errors.New("patch failed")
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						close(entered)

						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-release:
						}

						if outcome == "failure" {
							return patchError
						}

						return c.Patch(ctx, obj, patch, opts...)
					},
				})

				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()

				done := make(chan error, 1)

				go func() { _, err := r.Reconcile(ctx, ctrl.Request{}); done <- err }()

				<-entered
				require.EqualValues(t, 7, acceptedMembers(t, r)[testNodeUID].Shares)
				// Cross the original freshness deadline while the actual Node patch
				// remains blocked. The real observer must renew both accepted states.
				for range 3 {
					time.Sleep(20 * time.Second)

					observed := make(chan struct{})

					go func() { f.a.Replication.observe(f.ctx); close(observed) }()

					synctest.Wait()

					select {
					case <-observed:
					default:
						t.Fatal("observer blocked behind annotation patch")
					}

					require.NoError(t, f.a.Server.Ready(nil))
				}

				if outcome == "cancellation" {
					cancel()
				} else {
					close(release)
				}

				err := <-done

				switch outcome {
				case "success":
					require.NoError(t, err)
				case "failure":
					require.ErrorIs(t, err, patchError)
				case "cancellation":
					require.ErrorIs(t, err, context.Canceled)
				}

				gateCtx, stop := context.WithTimeout(f.ctx, time.Second)
				defer stop()

				require.NoError(t, f.a.authority.Observe(gateCtx))
			})
		})
	}
}

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

func TestReplicaObservedHighWaterWithoutImage(t *testing.T) {
	for _, initial := range []string{"installed10", "no image", "suspended"} {
		for _, mutation := range []string{"rollback10", "conflicting11", "membership hash", "membership rollback", "membership jump"} {
			t.Run(initial+"/"+mutation, func(t *testing.T) {
				f := newServingFixture(t)
				r := f.a.Replication
				base := *capturePublication(t, f.a.authority)
				base.record.Sequence, base.record.MembershipVersion = 10, 5

				newer := base.record
				newer.Sequence = 11
				newer.ContentHash = strings.Repeat("a", 64)
				setVersion := func(record VersionRecord) {
					cm := &corev1.ConfigMap{}

					err := r.APIReader.Get(f.ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.VersionConfigMapName}, cm)
					if err != nil {
						t.Fatal(err)
					}

					cm.Data = versionData(record)
					if err := r.Client.Update(f.ctx, cm); err != nil {
						t.Fatal(err)
					}
				}
				setVersion(base.record)

				if initial == "no image" {
					d := fixtureDependencies[f.a.authority]
					a := authority.New(r.Config.authorityConfig(), authority.Dependencies{Reader: d, Writer: d})
					f.a.authority = a
					r.authority = a
					f.a.Lifecycle = server.NewLifecycle(a)
					f.a.Server = server.New(r.Config.serverConfig(), d, a, f.a.Lifecycle, r)
					fixtureTLS(t, f.a.Server, f.ctx, f.serverCertificate)
					f.a.Lifecycle.SetCacheSync(func(context.Context) bool { return true })

					go func() { _ = f.a.Lifecycle.Start(f.ctx) }()

					f.a.Lifecycle.SetServingReady(true)
					f.a.Topology.authority = a
					f.a.Keyring.authority = a
					fixtureDependencies[a] = d
				} else {
					image, err := wire.DecodePublication(strings.NewReader(base.encoded))
					if err != nil {
						t.Fatal(err)
					}

					image.Sequence = 10

					image.MembershipVersion = 5
					if err := r.installReplica(f.ctx, f.ctx, image); err != nil {
						t.Fatal(err)
					}

					if initial == "suspended" {
						restore := withdrawPublication(t, f.a.Topology)
						restore()
					}
				}

				setVersion(newer)
				r.observe(f.ctx)

				bad := newer

				switch mutation {
				case "rollback10":
					bad = base.record
				case "conflicting11":
					bad.ContentHash = strings.Repeat("b", 64)
				case "membership hash":
					bad.Sequence++
					bad.MembershipHash = strings.Repeat("b", 64)
				case "membership rollback":
					bad.Sequence++
					bad.MembershipVersion--
				case "membership jump":
					bad.Sequence++
					bad.MembershipVersion += 2
				}

				setVersion(bad)
				r.observe(f.ctx)

				if f.a.authority.PublicationReady() == nil || f.a.Server.Ready(nil) == nil {
					t.Fatal("invalid authority failed to suspend or erased high-water")
				}

				if err := r.authority.TrustReady(); err == nil {
					t.Fatal("invalid authority retained trust")
				}

				image, decodeErr := wire.DecodePublication(strings.NewReader(base.encoded))
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}

				image.Sequence = 10

				image.MembershipVersion = 5
				if err := r.installReplica(f.ctx, f.ctx, image); err == nil {
					t.Fatal("install bypassed observed high-water")
				}

				// Exercise the public serving boundary, not only store readiness:
				// rejected authority must not expose even the previously valid image.
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				response := httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(response, request)

				if response.Code != http.StatusServiceUnavailable {
					t.Fatalf("invalid authority still served snapshot: %d", response.Code)
				}

				// Restoring the last valid authority permits a new reconcile/CAS,
				// without forgetting the observed watermark or reusing revoked bytes.
				setVersion(newer)
				runKeys(t, f.a.Keyring)

				recovered := reconcileTopology(t, f.a.Topology, f.ctx)
				if recovered.record.Sequence != newer.Sequence+1 || recovered.record.MembershipVersion != newer.MembershipVersion {
					t.Fatal("recovery reset counters or changed unchanged membership")
				}

				response = httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(response, request.Clone(f.ctx))

				if response.Code != http.StatusOK || response.Body.String() != recovered.encoded {
					t.Fatalf("reconciled authority not served: %d", response.Code)
				}
			})
		}
	}
}

type firstChunkWriter struct {
	calls            int
	entered, unblock chan struct{}
}

func (w *firstChunkWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		close(w.entered)
		<-w.unblock
	}

	return len(b), nil
}

func TestPublicationWriteAuthorityRevocation(t *testing.T) {
	for _, change := range []string{"superseded", "suspend recover", "confirmation after write admission"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := initializedTopology(t)
				image := reconcileTopology(t, r, t.Context())
				copy := *image
				copy.encoded = strings.Repeat("x", 64*1024)
				w := &firstChunkWriter{entered: make(chan struct{}), unblock: make(chan struct{})}
				done := make(chan error, 1)

				writeCtx, stopWrite, err := copy.writeContext(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stopWrite()

				go func() { _, err := copy.ForBase("").writeTo(writeCtx, w); done <- err }()

				<-w.entered

				switch change {
				case "superseded":
					next := *image
					next.record.Sequence++

					next.record.ContentHash = strings.Repeat("a", 64)

					advanceFixturePublication(t, r)
				case "suspend recover":
					restore := withdrawPublication(t, r)
					restore()
					reconcileTopology(t, r, t.Context())
				case "confirmation after write admission":
					time.Sleep(20 * time.Second)

					reconcileTopology(t, r, t.Context())

					time.Sleep(11 * time.Second)
				}

				if r.authority.PublicationReady() != nil {
					t.Fatal("replacement or confirmed image unavailable")
				}

				close(w.unblock)

				if err := <-done; err == nil || w.calls != 1 {
					t.Fatalf("revoked response continued: calls=%d err=%v", w.calls, err)
				}
			})
		})
	}
}

func TestMemberSiteLabelsOverrideHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"unlabeled", nil, ""},
		{"canonical", map[string]string{machinav1.MachineSiteLabelKey: "site-a"}, "site-a"},
		{"deprecated ignored", map[string]string{"net.unbounded-cloud.io/site": "site-b"}, ""},
		{"canonical only", map[string]string{machinav1.MachineSiteLabelKey: "site-a", "net.unbounded-cloud.io/site": "site-b"}, "site-a"},
		{"empty canonical no fallback", map[string]string{machinav1.MachineSiteLabelKey: "", "net.unbounded-cloud.io/site": "site-b"}, ""},
		{"empty labels", map[string]string{machinav1.MachineSiteLabelKey: ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"cold", "memory", "restart"} {
				t.Run(mode, func(t *testing.T) {
					node := memberNode()
					node.Labels = tc.labels
					pods := map[string][]corev1.Pod{node.Name: {memberPod("a", 1, "192.0.2.1")}}

					var accepted AcceptedMembers

					previous := wire.Member{Node: testNodeUID, Shares: 8, PeerEndpoint: "192.0.2.1:7443", RDMANICs: []wire.RDMANIC{{Rail: 1, Device: "mlx5_0", Port: 1}}, Site: "stale-site"}

					if mode != "cold" {
						node.Annotations = map[string]string{wire.RDMANICsAnnotation: "malformed"}
						pods = nil

						if mode == "memory" {
							accepted = AcceptedMembers{testNodeUID: previous}
						} else {
							encoded, err := json.Marshal(previous)
							require.NoError(t, err)

							node.Annotations[admittedMemberAnnotation] = string(encoded)
						}
					}

					got, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
					require.NoError(t, err)
					require.Len(t, got, 1)
					require.Equal(t, tc.want, got[testNodeUID].Site)

					if mode == "cold" {
						require.Empty(t, diagnostics)
					} else {
						require.Len(t, diagnostics, 2)

						previous.Site = tc.want
						require.Equal(t, previous, got[testNodeUID])
					}
				})
			}
		})
	}
}

func TestTopologySiteChangesPersistAcrossRestart(t *testing.T) {
	node := memberNode()
	node.Labels = map[string]string{machinav1.MachineSiteLabelKey: "site-a"}
	node.Annotations = map[string]string{wire.RDMANICsAnnotation: `[{"rail":0,"device":"mlx5_0","port":1}]`}
	pod := memberPod("a", 1, "192.0.2.1")
	r := initializedTopology(t, &node, &pod, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: DataplaneDaemonSetName, Namespace: "racer", UID: testDaemonSetUID}})
	first := reconcileTopology(t, r, t.Context())
	base, err := wire.DecodePublication(strings.NewReader(first.encoded))
	require.NoError(t, err)
	require.Equal(t, "site-a", base.Members[0].Site)

	for _, site := range []string{"site-b", ""} {
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))

		node.Labels = nil
		if site != "" {
			node.Labels = map[string]string{machinav1.MachineSiteLabelKey: site}
		}

		node.Annotations[wire.RDMANICsAnnotation] = "malformed"
		require.NoError(t, r.Update(t.Context(), &node))
		// Drop all in-memory history before processing the changed boundary.
		r = Assemble(r.Config, r.Client, r.APIReader).Topology
		committed := reconcileTopology(t, r, t.Context())
		next, err := wire.DecodePublication(strings.NewReader(committed.encoded))
		require.NoError(t, err)
		require.Equal(t, site, next.Members[0].Site)
		require.Equal(t, base.Members[0].RDMANICs, next.Members[0].RDMANICs)
		require.Equal(t, base.Sequence+1, next.Sequence)
		require.Equal(t, base.MembershipVersion+1, next.MembershipVersion)
		oldContent, oldMembership, err := wire.ContentHashes(base)
		require.NoError(t, err)
		content, membership, err := wire.ContentHashes(next)
		require.NoError(t, err)
		require.NotEqual(t, oldContent, content)
		require.NotEqual(t, oldMembership, membership)

		delta, err := wire.EncodeDelta(base, next)
		require.NoError(t, err)
		applied, err := wire.ApplyDelta(base, strings.NewReader(string(delta)))
		require.NoError(t, err)
		require.Equal(t, next, applied)
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))

		var saved wire.Member
		require.NoError(t, json.Unmarshal([]byte(node.Annotations[admittedMemberAnnotation]), &saved))
		require.Equal(t, next.Members[0], saved)

		r = Assemble(r.Config, r.Client, r.APIReader).Topology
		require.Equal(t, committed.encoded, reconcileTopology(t, r, t.Context()).encoded)

		base = next
	}
}

func workloadConfig(t *testing.T) members.Config {
	t.Helper()

	return members.Config{
		Cluster: "11111111-1111-1111-1111-111111111111", Namespace: "racer",
		ControlURL: "https://racer-controller.racer.svc:8443", DataplaneImage: "racer:test",
		BootstrapTrustConfigMap: "racer-bootstrap-trust", PeerPort: 8082,
		DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane",
	}
}

func TestWorkloadPeerMembership(t *testing.T) {
	for _, port := range []uint16{8082, 7443, 9090, 9091, 65535} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.PeerPort = port

			ds, err := members.DesiredDaemonSet(cfg)
			if err != nil {
				t.Fatal(err)
			}

			assertWorkloadPeerMembership(t, ds, port)
		})
	}
}

// Exercise the builder's ordered downward-API expansion and membership contract together.
func assertWorkloadPeerMembership(t *testing.T, ds *appsv1.DaemonSet, peerPort uint16) {
	t.Helper()

	for _, ips := range [][]string{{"192.0.2.1"}, {"2001:db8::1"}, {"192.0.2.1", "2001:db8::1"}, {"2001:db8::1", "192.0.2.1"}} {
		pod := memberPod("peer", 1, ips[0])
		for _, ip := range ips {
			pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
		}

		podIP, listen := "", ""

		for _, env := range ds.Spec.Template.Spec.Containers[0].Env {
			switch env.Name {
			case "RACER_POD_IP":
				if podIP != "" || env.Value != "" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}) {
					t.Fatal("peer bind address must come from status.podIP")
				}

				podIP = pod.Status.PodIP
			case "RACER_PEER_LISTEN":
				if podIP == "" || listen != "" || env.ValueFrom != nil || env.Value != "[$(RACER_POD_IP)]:"+strconv.Itoa(int(peerPort)) {
					t.Fatal("peer listener must expand the preceding Pod IP and configured peer port")
				}

				listen = strings.ReplaceAll(env.Value, "$(RACER_POD_IP)", podIP)
			}
		}

		host, port, err := net.SplitHostPort(listen)
		if err != nil {
			t.Fatalf("expanded peer listener %q: %v", listen, err)
		}

		ip, err := netip.ParseAddr(host)
		if err != nil || ip.IsUnspecified() || port != strconv.Itoa(int(peerPort)) {
			t.Fatalf("peer listener must bind the exact Pod IP and peer port: %q", listen)
		}

		candidate, diagnostics, err := reconcileMembers([]corev1.Node{memberNode()}, map[string][]corev1.Pod{pod.Spec.NodeName: {pod}}, memberOwnership(t, testDaemonSetUID), nil, peerPort)
		if err != nil || len(diagnostics) != 0 || len(candidate) != 1 {
			t.Fatalf("unready Pod with IPs %v must be published: %v, %v", ips, diagnostics, err)
		}

		if endpoint := candidate[testNodeUID].PeerEndpoint; endpoint != netip.AddrPortFrom(ip, peerPort).String() {
			t.Fatalf("membership endpoint %q disagrees with listener %q for Pod IPs %v", endpoint, listen, ips)
		}
	}
}
