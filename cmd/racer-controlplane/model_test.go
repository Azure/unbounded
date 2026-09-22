// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

// Listener allocation: deterministic batches and immutable history.

func allocationService(name, port string) corev1.Service {
	_, _, s := fixtures()

	s.Name = name
	if port != "" {
		s.Annotations[annotationPrefix+"listener-port"] = port
	}

	return *s
}

func TestListenerAllocationMixedBatchOrderIndependent(t *testing.T) {
	// Exercise every input permutation, with explicit reservations both before
	// and after automatic Services lexically. The later explicit reservation
	// must be known before either automatic assignment is made.
	for _, explicit := range [][]string{{"b", "d"}, {"a", "c"}} {
		t.Run(strings.Join(explicit, ","), func(t *testing.T) {
			services := []corev1.Service{allocationService("a", ""), allocationService("b", ""), allocationService("c", ""), allocationService("d", "")}
			want := map[string]int32{}
			next := int32(10003) // 10001 is management; 10000 and 10002 are explicit.

			for i := range services {
				s := &services[i]
				switch s.Name {
				case explicit[0]:
					s.Annotations[annotationPrefix+"listener-port"] = "10000"
					want["ns/"+s.Name] = 10000
				case explicit[1]:
					s.Annotations[annotationPrefix+"listener-port"] = "10002"
					want["ns/"+s.Name] = 10002
				default:
					want["ns/"+s.Name] = next
					next++
				}
			}

			n, p, _ := fixtures()

			var (
				first   *generation
				permute func(int)
			)

			permute = func(i int) {
				if i < len(services) {
					for j := i; j < len(services); j++ {
						services[i], services[j] = services[j], services[i]
						permute(i + 1)
						services[i], services[j] = services[j], services[i]
					}

					return
				}

				before, _ := json.Marshal(services)

				g, _, err := buildGenerationReserved("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, append(services, *originFixture()), reservedPorts{10001: true})
				if err != nil {
					t.Fatalf("feasible mixed batch rejected: %v", err)
				}

				if !reflect.DeepEqual(g.Ports, want) {
					t.Fatalf("ports = %v, want %v", g.Ports, want)
				}

				for _, v := range g.volumes() {
					if v.Volume.Port != want[v.Volume.ID] || len(v.Owners) != 8 {
						t.Fatalf("incorrect volume allocation/topology: %+v", v)
					}
				}

				if first == nil {
					first = g
				} else if !reflect.DeepEqual(first, g) {
					t.Fatal("input order changed generation")
				}

				after, _ := json.Marshal(services)
				if string(before) != string(after) {
					t.Fatal("build mutated input Services")
				}
			}
			permute(0)
		})
	}
}

func TestListenerAllocationMixedBatchReconcileAndRestart(t *testing.T) {
	ctx := context.Background()
	n, p, _ := fixtures()
	a, b := allocationService("a", ""), allocationService("b", "10000")
	c := fakeKube(n, p, &a, &b)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}

	var first *generation

	for restart := 0; restart < 2; restart++ {
		r := newTestReconciler(c)
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}

		g, _, err := r.store.load(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}

		want := map[string]int32{"ns/a": 10001, "ns/b": 10000}
		if !reflect.DeepEqual(g.Ports, want) {
			t.Fatalf("durable allocations = %v, want %v", g.Ports, want)
		}

		if first == nil {
			first = g
		} else if !reflect.DeepEqual(first, g) {
			t.Fatal("restart changed committed generation")
		}

		for _, s := range []corev1.Service{a, b} {
			var actual corev1.Service
			if err := c.Get(ctx, client.ObjectKeyFromObject(&s), &actual); err != nil {
				t.Fatal(err)
			}

			port := want[s.Namespace+"/"+s.Name]
			if actual.Spec.Ports[0].TargetPort.IntVal != port || actual.Annotations[annotationPrefix+"allocated-port"] != fmt.Sprint(port) {
				t.Fatalf("Service %s missed allocation %d: %+v", s.Name, port, actual)
			}
		}
	}
}

func TestListenerAllocationHistoricalControls(t *testing.T) {
	a := allocationService("a", "")

	initial, _, err := buildGeneration("default", nil, nil, nil, []corev1.Service{a})
	if err != nil {
		t.Fatal(err)
	}

	removed, _, err := buildGeneration("default", initial, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, history := range []*generation{initial, removed} {
		t.Run(fmt.Sprintf("removed=%t", history.Volume == nil), func(t *testing.T) {
			// Round-trip persisted state, including the removed Service tombstone.
			data, _ := json.Marshal(history)

			var previous generation
			if err := json.Unmarshal(data, &previous); err != nil {
				t.Fatal(err)
			}

			for _, tc := range []struct {
				name     string
				services []corev1.Service
				reserved reservedPorts
				want     map[string]int32
				err      string
				bad      string
			}{
				{"new-explicit-cannot-steal", []corev1.Service{allocationService("b", "10000"), a}, nil, nil, "already reserved", "b"},
				{"absent-history-cannot-be-stolen", []corev1.Service{allocationService("b", "10000")}, nil, nil, "already reserved", "b"},
				{"same-name-cannot-move", []corev1.Service{allocationService("a", "10005")}, nil, nil, "immutable", "a"},
				{"same-name-retains-port", []corev1.Service{a, allocationService("b", "10001"), allocationService("c", "")}, nil, map[string]int32{"ns/a": 10000, "ns/b": 10001, "ns/c": 10002}, "", ""},
				{"same-explicit-is-idempotent", []corev1.Service{allocationService("a", "10000")}, nil, map[string]int32{"ns/a": 10000}, "", ""},
				{"automatic-skips-tombstone", []corev1.Service{allocationService("b", "")}, nil, map[string]int32{"ns/a": 10000, "ns/b": 10001}, "", ""},
				{"historical-management-conflict", []corev1.Service{a}, reservedPorts{10000: true}, nil, "immutable", "a"},
				{"management-tombstone-survives", []corev1.Service{allocationService("b", "")}, reservedPorts{10000: true}, map[string]int32{"ns/a": 10000, "ns/b": 10001}, "", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// A new Kubernetes UID must not reset namespace/name allocation history.
					for i := range tc.services {
						tc.services[i].UID = "recreated-uid"
					}

					g, bad, err := buildGenerationReserved("default", &previous, nil, nil, append(tc.services, *originFixture()), tc.reserved)
					if tc.err != "" {
						if err == nil || !strings.Contains(err.Error(), tc.err) || g != nil || bad == nil || bad.Name != tc.bad {
							t.Fatalf("got generation=%v service=%v err=%v; want %s on %s", g, bad, err, tc.err, tc.bad)
						}
					} else if err != nil || !reflect.DeepEqual(g.Ports, tc.want) {
						t.Fatalf("generation=%v err=%v, want ports %v", g, err, tc.want)
					}

					after, _ := json.Marshal(&previous)
					if string(data) != string(after) {
						t.Fatal("build mutated immutable history")
					}
				})
			}
		})
	}
}

func TestListenerAllocationExplicitValidationAndSelection(t *testing.T) {
	for _, port := range []string{"0", "1023", "65536", "invalid", "9090", "9443", "10001"} {
		t.Run(port, func(t *testing.T) {
			g, bad, err := buildGenerationReserved("default", nil, nil, nil, []corev1.Service{allocationService("a", ""), allocationService("b", port)}, reservedPorts{10001: true})
			if err == nil || g != nil || bad == nil || bad.Name != "b" {
				t.Fatalf("invalid explicit request accepted: generation=%v service=%v err=%v", g, bad, err)
			}
		})
	}

	_, bad, err := buildGeneration("default", nil, nil, nil, []corev1.Service{allocationService("a", "10000"), allocationService("b", "10000")})
	if err == nil || !strings.Contains(err.Error(), "already reserved") || bad == nil || bad.Name != "b" {
		t.Fatalf("duplicate explicit request: service=%v err=%v", bad, err)
	}

	other := allocationService("other", "10000")
	other.Annotations[universeAnnotation] = "other"
	deleted := allocationService("deleted", "10000")
	now := metav1.Now()
	deleted.DeletionTimestamp = &now
	unmanaged := allocationService("unmanaged", "10000")
	delete(unmanaged.Annotations, originServiceAnnotation)

	g, _, err := buildGeneration("default", nil, nil, nil, []corev1.Service{other, deleted, unmanaged, allocationService("a", "")})
	if err != nil || !reflect.DeepEqual(g.Ports, map[string]int32{"ns/a": 10000}) {
		t.Fatalf("unselected Services reserved ports: generation=%v err=%v", g, err)
	}
}

func TestListenerAllocationRangeExhaustion(t *testing.T) {
	previous := &generation{Ports: map[string]int32{}}
	for port := int32(10000); port <= 29999; port++ {
		previous.Ports[fmt.Sprintf("deleted/%d", port)] = port
	}

	a := allocationService("a", "")
	// Exhaustion cannot bypass validation of a later explicit request.
	_, bad, err := buildGeneration("default", previous, nil, nil, []corev1.Service{a, allocationService("b", "10000")})
	if err == nil || !strings.Contains(err.Error(), "already reserved") || bad == nil || bad.Name != "b" {
		t.Fatalf("explicit conflict was not validated first: service=%v err=%v", bad, err)
	}

	_, bad, err = buildGeneration("default", previous, nil, nil, []corev1.Service{a})
	if err == nil || !strings.Contains(err.Error(), "range exhausted") || bad == nil || bad.Name != "a" {
		t.Fatalf("automatic range exhaustion: service=%v err=%v", bad, err)
	}
	// Explicit ports outside the automatic range remain valid at exhaustion.
	g, _, err := buildGeneration("default", previous, nil, nil, []corev1.Service{allocationService("b", "65535")})
	if err != nil || g.Ports["ns/b"] != 65535 || len(g.Ports) != len(previous.Ports)+1 {
		t.Fatalf("explicit port outside automatic range rejected: %v", err)
	}
}

// Management reservations and deployment port policy.

// A is already serving: aggregate Pod readiness cannot protect B if the
// controller points B's Service at the independently healthy management socket.
func TestB06ManagementReservationBeforeCommitAndServicePatch(t *testing.T) {
	ctx := context.Background()
	n, p, a := fixtures()
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	c := fakeKube(n, p, a)
	r := newTestReconciler(c)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	before, _, err := r.store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	b := a.DeepCopy()
	b.Name = "second"
	b.ResourceVersion = ""
	b.Annotations[annotationPrefix+"listener-port"] = "9090"

	b.Spec.Ports[0].TargetPort = intstr.FromInt32(80)
	if err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}

	_, err = r.Reconcile(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "management") {
		t.Errorf("B06 management listener must be rejected before durable allocation: %v", err)
	}

	after, _, err := r.store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(before, after) {
		t.Error("B06 invalid B changed durable generation")
	}

	var actual corev1.Service
	if err := c.Get(ctx, client.ObjectKeyFromObject(b), &actual); err != nil {
		t.Fatal(err)
	}

	if actual.Spec.Ports[0].TargetPort != intstr.FromInt32(80) || actual.Annotations[annotationPrefix+"allocated-port"] != "" {
		t.Errorf("B06 new Service exposes management while A is ready: %+v", actual)
	}
}

func TestB06ConfiguredPortsValidation(t *testing.T) {
	for _, text := range []string{"", "0", "65536", "-1", "+9090", "9090,", ",9090", "9090,9090", "9090, 10000", "metrics", "9090-9091"} {
		if _, err := parseReservedPorts(text); err == nil {
			t.Errorf("accepted invalid ports %q", text)
		}
	}

	for _, text := range []string{"9090", "10000,10001", "1,65535"} {
		ports, err := parseReservedPorts(text)
		if err != nil {
			t.Fatal(err)
		}

		if !ports.contains(9090) || !ports.contains(9443) {
			t.Fatal("default management or peer TLS reservation lost")
		}
	}
}

func TestB06ConfiguredAutomaticAllocationRestartAndImmutability(t *testing.T) {
	ctx := context.Background()
	n, p, s := fixtures()
	c := fakeKube(n, p, s)

	ports, err := parseReservedPorts("10000,10001")
	if err != nil {
		t.Fatal(err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}

	var first *generation

	for restart := 0; restart < 2; restart++ {
		r := newTestReconciler(c)

		r.reserved = ports
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}

		got := r.loaded["default"]
		if got.Volume.Port != 10002 || got.Ports["ns/volume"] != 10002 {
			t.Fatalf("allocated reserved port: %+v", got)
		}

		if restart == 0 {
			first = got
		} else if !reflect.DeepEqual(first, got) {
			t.Fatal("restart changed allocation/revision")
		}

		var actual corev1.Service
		if err := c.Get(ctx, client.ObjectKeyFromObject(s), &actual); err != nil {
			t.Fatal(err)
		}

		if actual.Spec.Ports[0].TargetPort.IntVal != 10002 {
			t.Fatal("Service missed safe allocation")
		}
	}

	var actual corev1.Service
	if err := c.Get(ctx, client.ObjectKeyFromObject(s), &actual); err != nil {
		t.Fatal(err)
	}

	actual.Annotations[annotationPrefix+"listener-port"] = "10003"
	if err := c.Update(ctx, &actual); err != nil {
		t.Fatal(err)
	}

	r := newTestReconciler(c)

	r.reserved = ports
	if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("allocation moved: %v", err)
	}

	after, _, err := r.store.load(ctx, "default")
	if err != nil || !reflect.DeepEqual(first, after) {
		t.Fatalf("immutable history changed: %v", err)
	}
}

func TestB06NewAndPersistedManagementReservations(t *testing.T) {
	for _, port := range []int32{9090, 9443, 10000, 12345} {
		for _, historical := range []bool{false, true} {
			t.Run(fmt.Sprintf("port=%d/historical=%t", port, historical), func(t *testing.T) {
				ctx := context.Background()
				n, p, s := fixtures()
				c := fakeKube(n, p, s)
				r := newTestReconciler(c)
				r.reserved = reservedPorts{port: true}

				var before *generation

				if historical {
					// Seed exact pre-B06 durable state, then exercise restart loading.
					g, _, err := buildGeneration("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, []corev1.Service{*s})
					if err != nil {
						t.Fatal(err)
					}

					g.Revision = 7

					g.Volume.Port, g.Ports["ns/volume"] = port, port
					if err := r.store.commit(ctx, g, nil); err != nil {
						t.Fatal(err)
					}

					before = g
				} else {
					s.Annotations[annotationPrefix+"listener-port"] = fmt.Sprint(port)
					if err := c.Update(ctx, s); err != nil {
						t.Fatal(err)
					}
				}

				req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
				if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "management") {
					t.Fatalf("accepted reserved port: %v", err)
				}

				after, _, err := r.store.load(ctx, "default")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("reserved history silently altered: %v", err)
				}

				var actual corev1.Service
				if err := c.Get(ctx, client.ObjectKeyFromObject(s), &actual); err != nil {
					t.Fatal(err)
				}

				if actual.Spec.Ports[0].TargetPort.IntVal != 0 || actual.Annotations[annotationPrefix+"allocated-port"] != "" {
					t.Fatal("invalid Service patched to management")
				}

				if historical {
					// Removal may proceed, but its tombstone must survive and recreation
					// (even with a different requested port) must never silently migrate it.
					if err := c.Delete(ctx, &actual); err != nil {
						t.Fatal(err)
					}

					if _, err := r.Reconcile(ctx, req); err != nil {
						t.Fatal(err)
					}

					if r.loaded["default"].Ports["ns/volume"] != port {
						t.Fatal("removal lost reservation")
					}

					s.ResourceVersion = ""

					s.Annotations[annotationPrefix+"listener-port"] = "15000"
					if err := c.Create(ctx, s); err != nil {
						t.Fatal(err)
					}

					r = newTestReconciler(c)

					r.reserved = reservedPorts{port: true}
					if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "immutable") {
						t.Fatalf("recreated reserved history moved: %v", err)
					}
				}
			})
		}
	}
}

// Origin Service admission and last-good generation preservation.

type originPortCase struct {
	Name  string
	Port  string
	Valid bool
}

func TestB12ProductionSnapshots(t *testing.T) {
	dir := os.Getenv("B12_EXPORT")
	if dir == "" {
		dir = t.TempDir()
	}

	for _, tc := range originPortCorpus() {
		if !tc.Valid {
			continue
		}

		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			n, p, s := fixtures()
			s.Annotations[originPortAnnotation] = tc.Port
			c := fakeKube(n, p, s)

			r := newTestReconciler(c)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
				t.Fatal(err)
			}

			g := r.loaded["default"]

			idx, err := indexGeneration(g)
			if err != nil {
				t.Fatal(err)
			}

			snap := idx.snapshot(g.Nodes[n.Name].ID)
			response := get(handler(r.server), target(snap), "", "")

			var envelope pb.Configuration
			if response.Code != 200 || proto.Unmarshal(response.Body.Bytes(), &envelope) != nil {
				t.Fatal("snapshot delivery failed")
			}

			if got := envelope.GetSnapshot().Volumes[0].OriginIdentity; got != "ns/origin:8080" {
				t.Fatalf("origin identity changed: %q", got)
			}

			data, err := protojson.Marshal(&envelope)
			if err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(dir, tc.Name+".json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestB12InvalidPreservesGenerationAndService(t *testing.T) {
	for _, tc := range originPortCorpus() {
		if tc.Valid {
			continue
		}

		for _, add := range []bool{false, true} {
			label := "update/"
			if add {
				label = "add/"
			}

			t.Run(label+tc.Name, func(t *testing.T) {
				ctx := context.Background()
				n, p, s := fixtures()
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				c := fakeKube(n, p, s)
				r := newTestReconciler(c)

				req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}

				before, _, err := r.store.load(ctx, "default")
				if err != nil {
					t.Fatal(err)
				}

				loaded := r.loaded["default"]

				idx, err := indexGeneration(before)
				if err != nil {
					t.Fatal(err)
				}

				path := target(idx.snapshot(before.Nodes[n.Name].ID))
				served := get(handler(r.server), path, "", "")

				var actual corev1.Service
				if err := c.Get(ctx, client.ObjectKeyFromObject(s), &actual); err != nil {
					t.Fatal(err)
				}

				if add {
					actual.Name, actual.ResourceVersion = "second", ""
					actual.Spec.Ports[0].TargetPort = intstr.FromInt32(80)
					delete(actual.Annotations, annotationPrefix+"allocated-port")
				}

				actual.Annotations[originPortAnnotation] = tc.Port

				wantSpec, wantAllocated := actual.Spec.DeepCopy(), actual.Annotations[annotationPrefix+"allocated-port"]
				if add {
					err = c.Create(ctx, &actual)
				} else {
					err = c.Update(ctx, &actual)
				}

				if err != nil {
					t.Fatal(err)
				}

				if _, err := r.Reconcile(ctx, req); err == nil {
					t.Fatalf("invalid origin port accepted: %q", tc.Port)
				}

				after, _, err := r.store.load(ctx, "default")
				if err != nil || !reflect.DeepEqual(before, after) || r.loaded["default"] != loaded {
					t.Fatalf("last-good generation changed: %v", err)
				}

				response := get(handler(r.server), path, "", "")
				if response.Code != 200 || !bytes.Equal(served.Body.Bytes(), response.Body.Bytes()) || served.Header().Get("ETag") != response.Header().Get("ETag") {
					t.Fatal("last-good published snapshot changed")
				}

				if err := c.Get(ctx, client.ObjectKeyFromObject(&actual), &actual); err != nil {
					t.Fatal(err)
				}

				if !reflect.DeepEqual(wantSpec, &actual.Spec) || actual.Annotations[annotationPrefix+"allocated-port"] != wantAllocated {
					t.Fatal("invalid Service was patched")
				}
				// A restart must serve the exact durable last-good snapshot as well.
				restarted := newTestReconciler(c)
				if _, err := restarted.Reconcile(ctx, req); err == nil {
					t.Fatal("restart admitted invalid URL")
				}

				response = get(handler(restarted.server), path, "", "")
				if response.Code != 200 || !bytes.Equal(served.Body.Bytes(), response.Body.Bytes()) {
					t.Fatal("restart lost last-good snapshot")
				}
			})
		}
	}
}

func originPortCorpus() []originPortCase {
	return []originPortCase{
		{"numeric", "8080", true},
		{"named", "http", true},
		{"missing", "", false},
		{"unknown-name", "missing", false},
		{"unknown-number", "80", false},
		{"zero", "0", false},
		{"overflow", "65536", false},
		{"url", "http://origin:8080", false},
	}
}

func TestB12OriginPortCorpus(t *testing.T) {
	for _, c := range originPortCorpus() {
		t.Run(c.Name, func(t *testing.T) {
			_, _, s := fixtures()
			s.Annotations[originPortAnnotation] = c.Port

			v, err := resolveOrigin(originFixture(), c.Port)
			if (err == nil) != c.Valid {
				t.Fatalf("port %q accepted=%t, want %t: %v", c.Port, err == nil, c.Valid, err)
			}

			if c.Valid && v.Identity != "ns/origin:8080" {
				t.Fatal("named and numeric port identities differ")
			}
		})
	}
}

// Self-backend rejection across equivalent Service names and IP spellings.

type selfBackendCase struct {
	name       string
	url        string
	clusterIP  string
	clusterIPs []string
	valid      bool
}

func selfBackendCases() []selfBackendCase {
	v4, v6 := "10.100.0.1", "fd00:abcd::1"
	dual := []string{v4, v6}

	return []selfBackendCase{
		{"self-v4", "volume", v4, nil, false},
		{"self-dual", "volume", v4, dual, false},
		{"separate", "origin", v4, dual, true},
	}
}

func (tc selfBackendCase) service() *corev1.Service {
	_, _, s := fixtures()
	s.Spec.ClusterIP, s.Spec.ClusterIPs = tc.clusterIP, tc.clusterIPs
	s.Annotations[originServiceAnnotation] = tc.url

	return s
}

func TestSelfBackendModel(t *testing.T) {
	for _, tc := range selfBackendCases() {
		t.Run(tc.name, func(t *testing.T) {
			n, p, _ := fixtures()
			s := tc.service()
			before := s.DeepCopy()

			g, _, err := buildGeneration("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, []corev1.Service{*s})
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}

				if g.Volume.Origin.Identity != "ns/origin:8080" {
					t.Fatalf("origin identity changed: %q", g.Volume.Origin.Identity)
				}
			} else if err == nil || !strings.Contains(err.Error(), "separate") || g != nil {
				t.Fatalf("self backend admitted: generation=%v err=%v", g, err)
			}

			if !reflect.DeepEqual(before, s) {
				t.Fatal("model mutated Service input")
			}
		})
	}
}

func TestSelfBackendReconcile(t *testing.T) {
	for _, tc := range selfBackendCases() {
		for _, mode := range []string{"initial", "update", "add"} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				n, p, base := fixtures()
				c := fakeKube(n, p)
				r := newTestReconciler(c)
				req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
				path := "/" + identity("universe", "default") + "/" + identity("node", string(n.UID))

				if mode != "initial" {
					if mode == "add" {
						base.Name = "existing"
					}

					if err := c.Create(ctx, base); err != nil {
						t.Fatal(err)
					}

					if _, err := r.Reconcile(ctx, req); err != nil {
						t.Fatal(err)
					}
				}

				before, _, err := r.store.load(ctx, "default")
				if err != nil {
					t.Fatal(err)
				}

				loaded := r.loaded["default"]
				served := get(handler(r.server), path, "", "")
				s := tc.service()

				if mode == "update" {
					var current corev1.Service
					if err := c.Get(ctx, client.ObjectKeyFromObject(s), &current); err != nil {
						t.Fatal(err)
					}

					s.ResourceVersion = current.ResourceVersion
					err = c.Update(ctx, s)
				} else {
					err = c.Create(ctx, s)
				}

				if err != nil {
					t.Fatal(err)
				}

				wantSpec := s.Spec.DeepCopy()

				_, err = r.Reconcile(ctx, req)
				if tc.valid {
					if err != nil {
						t.Fatal(err)
					}

					response := get(handler(r.server), path, "", "")

					var envelope pb.Configuration
					if response.Code != 200 || proto.Unmarshal(response.Body.Bytes(), &envelope) != nil {
						t.Fatal("snapshot delivery failed")
					}

					found := false

					for _, v := range envelope.GetSnapshot().Volumes {
						if v.OriginIdentity == "ns/origin:8080" {
							found = true
						}
					}

					if !found {
						t.Fatalf("exact backend bytes not published: %q", tc.url)
					}

					return
				}

				if err == nil || !strings.Contains(err.Error(), "separate") {
					t.Fatalf("self backend not rejected: %v", err)
				}

				after, _, err := r.store.load(ctx, "default")
				if err != nil || !reflect.DeepEqual(before, after) || r.loaded["default"] != loaded {
					t.Fatalf("rejection changed durable/loaded generation: %v", err)
				}

				if err := c.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
					t.Fatal(err)
				}

				if !reflect.DeepEqual(wantSpec, &s.Spec) || s.Annotations[annotationPrefix+"allocated-port"] != "" || s.Annotations[annotationPrefix+"universe-id"] != "" {
					t.Fatal("self backend caused a Service routing patch")
				}

				if !strings.Contains(s.Annotations[annotationPrefix+"status"], "separate") {
					t.Fatal("Service missing rejection status")
				}
				// Verify the actual HTTP publication, both now and after reload.
				for _, controller := range []*reconciler{r, newTestReconciler(c)} {
					if _, err := controller.Reconcile(ctx, req); err == nil {
						t.Fatal("retry/restart admitted self backend")
					}

					response := get(handler(controller.server), path, "", "")
					if response.Code != served.Code || !bytes.Equal(response.Body.Bytes(), served.Body.Bytes()) || response.Header().Get("ETag") != served.Header().Get("ETag") {
						t.Fatal("rejection changed published snapshot")
					}
				}
			})
		}
	}
}

func TestBootstrapSiteUniverse(t *testing.T) {
	for _, tc := range []struct {
		deprecated, label, pod string
		valid                  bool
	}{
		{"", "default", "default", true},
		{"default", "default", "default", true},
		{"other", "other", "other", true},
		{"other", "default", "default", true},
		{"default", "other", "default", false},
		{"", "", "default", false},
		{"other", "other", "default", false},
		{"default", "", "default", false},
		{"", "default", "", false},
	} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node", Labels: map[string]string{racer.DeprecatedSiteLabelKey: tc.deprecated, racer.SiteLabelKey: tc.label}}}
		if err := validateBootstrapNode(n, tc.pod); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}

		n.UID = ""
		if validateBootstrapNode(n, tc.pod) == nil {
			t.Fatal("missing UID accepted")
		}
	}

	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node", Labels: map[string]string{racer.DeprecatedSiteLabelKey: "default"}}}
	if err := validateBootstrapNode(n, "default"); err != nil {
		t.Fatalf("absent canonical Site must permit fallback: %v", err)
	}

	n.Labels[racer.ExcludeLabelKey] = "true"
	if validateBootstrapNode(n, "default") == nil {
		t.Fatal("excluded fallback Node accepted")
	}

	delete(n.Labels, racer.ExcludeLabelKey)

	n.DeletionTimestamp = new(metav1.Now())
	if validateBootstrapNode(n, "default") == nil {
		t.Fatal("deleting Node accepted")
	}

	if validateBootstrapNode(nil, "default") == nil {
		t.Fatal("nil Node accepted")
	}
}
