// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const rackTopologyKey = "topology.example.com/rack"

func TestExplicitMembershipSelectsOnlyDeclaredNodesInOneNonemptyTopology(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*signalv1.CertificationProfile, []*corev1.Node, *corev1.Node)
		want   string
	}{
		{"success", nil, ""},
		{"missing names", func(p *signalv1.CertificationProfile, _ []*corev1.Node, _ *corev1.Node) {
			p.Spec.Participants.NodeNames = nil
		}, "require explicit"},
		{"duplicate names", func(p *signalv1.CertificationProfile, _ []*corev1.Node, _ *corev1.Node) {
			p.Spec.Participants.NodeNames = []string{"gpu-0", "gpu-0"}
		}, "duplicate"},
		{"wrong count", func(p *signalv1.CertificationProfile, _ []*corev1.Node, _ *corev1.Node) {
			p.Spec.Participants.Count = 3
		}, "exactly count"},
		{"missing topology key", func(p *signalv1.CertificationProfile, _ []*corev1.Node, _ *corev1.Node) {
			p.Spec.Participants.TopologyKey = ""
		}, "require explicit"},
		{"invalid topology key", func(p *signalv1.CertificationProfile, _ []*corev1.Node, _ *corev1.Node) {
			p.Spec.Participants.TopologyKey = "invalid key/rack"
		}, "invalid participant topologyKey"},
		{"missing topology label", func(_ *signalv1.CertificationProfile, n []*corev1.Node, _ *corev1.Node) {
			delete(n[1].Labels, rackTopologyKey)
		}, "same nonempty"},
		{"different topology label", func(_ *signalv1.CertificationProfile, n []*corev1.Node, _ *corev1.Node) {
			n[1].Labels[rackTopologyKey] = "rack-b"
		}, "same nonempty"},
		{"empty topology labels", func(_ *signalv1.CertificationProfile, n []*corev1.Node, _ *corev1.Node) {
			n[0].Labels[rackTopologyKey], n[1].Labels[rackTopologyKey] = "", ""
		}, "same nonempty"},
		{"nonmember request", func(_ *signalv1.CertificationProfile, _ []*corev1.Node, request *corev1.Node) {
			request.Name = "gpu-2"
		}, "not a declared"},
		{"recreated request node", func(_ *signalv1.CertificationProfile, _ []*corev1.Node, request *corev1.Node) {
			request.UID = "old-request-uid"
		}, "identity changed"},
		{"peer ineligible", func(_ *signalv1.CertificationProfile, n []*corev1.Node, _ *corev1.Node) {
			n[1].Spec.Unschedulable = true
		}, "does not satisfy"},
		{"peer selector mismatch", func(_ *signalv1.CertificationProfile, n []*corev1.Node, _ *corev1.Node) {
			n[1].Labels["node.kubernetes.io/instance-type"] = "other"
		}, "does not satisfy"},
		{"native workflow cannot take a group", func(p *signalv1.CertificationProfile, _ []*corev1.Node, _ *corev1.Node) {
			p.Spec.Workflow.Name = FullNodeCertificationWorkflow
		}, "require workflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			profile, nodes := rackProfileAndNodes()

			request := nodes[0].DeepCopy()
			if tc.change != nil {
				tc.change(profile, nodes, request)
			}

			extra := readyNode("gpu-2", "example-gpu")
			extra.Labels[rackTopologyKey] = "rack-a"
			kube := membershipClientBuilder(t, nodes[0], nodes[1], extra).Build()
			r := &AdmissionReconciler{Client: kube}

			participants, err := r.selectParticipants(t.Context(), request, profile)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("selection error = %v, want %q", err, tc.want)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			want := []signalv1.Participant{{NodeName: "gpu-0", NodeUID: "gpu-0-uid"}, {NodeName: "gpu-1", NodeUID: "gpu-1-uid"}}
			if !reflect.DeepEqual(participants, want) {
				t.Fatalf("participants = %#v, want %#v; same-label nonmember must not be selected", participants, want)
			}
		})
	}
}

func TestExplicitAdmissionPersistsIntentBeforeReservationsAndIgnoresLaterProfileMutation(t *testing.T) {
	t.Parallel()

	profile, nodes := rackProfileAndNodes()
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	extra := readyNode("gpu-2", "example-gpu")
	extra.Labels[rackTopologyKey] = "rack-a"
	kube := membershipClientBuilder(t, profile, nodes[0], nodes[1], extra, source).Build()

	admission := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal", Now: membershipNow}
	for _, node := range nodes {
		if err := admission.admitNode(t.Context(), node, profile.Name); err != nil {
			t.Fatal(err)
		}
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(t.Context(), &runs); err != nil {
		t.Fatal(err)
	}

	if len(runs.Items) != 1 {
		t.Fatalf("duplicate member requests created %d runs", len(runs.Items))
	}

	run := &runs.Items[0]
	if run.Spec.Admission == nil || run.Spec.Admission.TopologyValue != "rack-a" ||
		run.Spec.Admission.Participants.TopologyKey != rackTopologyKey ||
		!reflect.DeepEqual(run.Spec.Admission.Participants.NodeNames, []string{"gpu-0", "gpu-1"}) ||
		len(run.Spec.TestPlan) != 8 {
		t.Fatalf("immutable group contract = %#v", run.Spec)
	}

	assertRackReservations(t, kube, nodes, "")
	assertRackWorkflowCount(t, kube, 0)

	copy := run.DeepCopy()
	copy.Spec.Admission.Participants.NodeNames[0] = "unapproved"

	copy.Spec.Admission.Participants.Selector.MatchLabels["node.kubernetes.io/instance-type"] = "changed"
	if run.Spec.Admission.Participants.NodeNames[0] != "gpu-0" ||
		run.Spec.Admission.Participants.Selector.MatchLabels["node.kubernetes.io/instance-type"] != "example-gpu" {
		t.Fatal("deepcopy aliases the immutable admission policy")
	}

	profile.Spec.Participants.NodeNames = []string{"unapproved"}
	profile.Spec.Participants.Count = 1
	profile.Spec.Participants.TopologyKey = "changed"

	profile.Spec.RunnerImage = "changed"
	if err := kube.Update(t.Context(), profile); err != nil {
		t.Fatal(err)
	}

	r := membershipReconciler(t, kube)
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	got := getTestRun(t, kube, run)
	if !got.Status.AdmissionReady || got.Status.Workflow.Attempt != 1 ||
		got.Spec.RunnerImage != "runner@sha256:abc" || len(got.Spec.Participants) != 2 {
		t.Fatalf("admitted status = %#v", got)
	}

	assertRackReservations(t, kube, nodes, run.Name)
	assertRackWorkflowCount(t, kube, 1)

	var untouched corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKey{Name: extra.Name}, &untouched); err != nil {
		t.Fatal(err)
	}

	if untouched.Annotations[ReservationLabel] != "" || untouched.Labels[StateLabel] != "" {
		t.Fatal("same-label nonmember was mutated")
	}
}

func TestExplicitPendingAdmissionRevalidatesEveryMemberAndRejectsInvalidRunScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*signalv1.CertificationRun, []*corev1.Node) []client.Object
		want   string
	}{
		{"UID changed", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			n[1].UID = "replacement-uid"
			n[1].Annotations = map[string]string{ReservationLabel: "replacement-owner"}

			return nil
		}, "UID changed"},
		{"topology drift", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			n[1].Labels[rackTopologyKey] = "rack-b"
			return nil
		}, "topology label"},
		{"missing topology", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			delete(n[1].Labels, rackTopologyKey)
			return nil
		}, "topology label"},
		{"inventory changed", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			n[1].Status.Capacity["nvidia.com/gpu"] = resource.MustParse("7")
			return nil
		}, "inventory or identity"},
		{"not ready", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			n[1].Status.Conditions[0].Status = corev1.ConditionFalse
			return nil
		}, "eligibility"},
		{"unschedulable", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			n[1].Spec.Unschedulable = true
			return nil
		}, "eligibility"},
		{"selector changed", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			n[1].Labels["node.kubernetes.io/instance-type"] = "other"
			return nil
		}, "eligibility"},
		{"peer busy", func(_ *signalv1.CertificationRun, n []*corev1.Node) []client.Object {
			return []client.Object{&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "other"},
				Spec:       corev1.PodSpec{NodeName: n[1].Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}}
		}, "WorkloadActive"},
		{"nonmember trigger", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.RequestNodeName = "gpu-2"
			return nil
		}, "not a declared run member"},
		{"nonmember participant", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.Participants[1].NodeName = "gpu-2"
			return nil
		}, "undeclared identity"},
		{"duplicate UID", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.Participants[1].NodeUID = run.Spec.Participants[0].NodeUID
			return nil
		}, "duplicate"},
		{"missing admission snapshot", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.Admission = nil
			return nil
		}, "immutable admission snapshot"},
		{"native single-node workflow", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.Workflow.Name = FullNodeCertificationWorkflow
			return nil
		}, "rack workflow"},
		{"missing group test plan", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.TestPlan = nil
			return nil
		}, "immutable per-node test plan"},
		{"unsupported fabric test", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.Spec.Parameters["rack-tests"] = `["fabric"]`
			return nil
		}, "UnsupportedRackTest"},
		{"expired run deadline", func(run *signalv1.CertificationRun, _ []*corev1.Node) []client.Object {
			run.CreationTimestamp = metav1.NewTime(membershipNow().Add(-time.Hour))
			return nil
		}, "deadline expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "pending-group", FullNodeCertificationWorkflow)
			extra := tc.change(run, nodes)
			objects := append(rackRunObjects(run, nodes), extra...)
			kube := membershipClientBuilder(t, objects...).Build()
			r := membershipReconciler(t, kube)

			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
				t.Fatal(err)
			}

			got := getTestRun(t, kube, run)
			if got.Status.Phase != signalv1.RunPhaseInconclusive || got.Status.AdmissionReady ||
				!strings.Contains(got.Status.Reason, tc.want) {
				t.Fatalf("rejected admission = %#v, want %q", got.Status, tc.want)
			}

			assertRackWorkflowCount(t, kube, 0)

			var peer corev1.Node
			if err := kube.Get(t.Context(), client.ObjectKey{Name: nodes[1].Name}, &peer); err != nil {
				t.Fatal(err)
			}

			if tc.name == "UID changed" && peer.Annotations[ReservationLabel] != "replacement-owner" {
				t.Fatal("replacement Node ownership was changed")
			}
		})
	}
}

func TestExplicitPartialReservationRollbackOnContentionConflictAndFinalValidation(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"competing owner", "optimistic conflict", "topology drift after acquisition", "UID drift after acquisition"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "partial-group", FullNodeCertificationWorkflow)
			firstReserved := false
			kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: createMembershipObject,
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						node, ok := obj.(*corev1.Node)
						if !ok || node.Annotations[ReservationLabel] != run.Name {
							return c.Patch(ctx, obj, patch, opts...)
						}

						if node.Name == nodes[0].Name {
							firstReserved = true
							return c.Patch(ctx, obj, patch, opts...)
						}

						if !firstReserved {
							t.Fatal("fixture did not reach partial acquisition")
						}

						var current corev1.Node
						if err := c.Get(ctx, client.ObjectKeyFromObject(node), &current); err != nil {
							return err
						}

						switch mode {
						case "competing owner":
							current.Annotations = map[string]string{ReservationLabel: "other-run"}
							if err := c.Update(ctx, &current); err != nil {
								return err
							}

							return c.Patch(ctx, obj, patch, opts...)
						case "optimistic conflict":
							return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name, errors.New("injected conflict"))
						default:
							if err := c.Patch(ctx, obj, patch, opts...); err != nil {
								return err
							}

							if err := c.Get(ctx, client.ObjectKeyFromObject(node), &current); err != nil {
								return err
							}

							if mode == "topology drift after acquisition" {
								current.Labels[rackTopologyKey] = "rack-b"
							} else {
								current.UID = "replacement-uid"
								current.Annotations[ReservationLabel] = "replacement-owner"
							}

							return c.Update(ctx, &current)
						}
					},
				}).Build()

			r := membershipReconciler(t, kube)
			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
				t.Fatal(err)
			}

			got := getTestRun(t, kube, run)
			if got.Status.Phase != signalv1.RunPhaseInconclusive || got.Status.AdmissionReady || !firstReserved {
				t.Fatalf("partial admission was not rejected: %#v", got.Status)
			}

			assertRackWorkflowCount(t, kube, 0)
			assertRackNodeReservation(t, kube, nodes[0].Name, "")

			expected := ""

			switch mode {
			case "competing owner":
				expected = "other-run"
			case "UID drift after acquisition":
				expected = "replacement-owner"
			}

			assertRackNodeReservation(t, kube, nodes[1].Name, expected)
		})
	}
}

func TestExplicitRollbackFailureRetainsDurableCleanupAndPreservesRacingOwners(t *testing.T) {
	t.Parallel()

	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("competing owner during rollback=%t", race), func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "rollback-group", FullNodeCertificationWorkflow)
			failRelease := true
			raced := false
			kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: createMembershipObject,
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						node, ok := obj.(*corev1.Node)
						if !ok {
							return c.Patch(ctx, obj, patch, opts...)
						}

						if node.Name == nodes[1].Name && node.Annotations[ReservationLabel] == run.Name {
							return errors.New("second reservation failed")
						}

						if node.Name == nodes[0].Name && node.Annotations[ReservationLabel] == "" && failRelease {
							if !race {
								return errors.New("rollback failed")
							}

							if !raced {
								var current corev1.Node
								if err := c.Get(ctx, client.ObjectKeyFromObject(node), &current); err != nil {
									return err
								}

								current.Annotations[ReservationLabel] = "racing-owner"
								if err := c.Update(ctx, &current); err != nil {
									return err
								}

								raced = true
							}
						}

						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			r := membershipReconciler(t, kube)

			_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			if !race && (err == nil || !strings.Contains(err.Error(), "rollback failed")) {
				t.Fatalf("rollback error = %v", err)
			}

			got := getTestRun(t, kube, run)
			if got.Status.Phase != signalv1.RunPhaseInconclusive || !containsString(got.Finalizers, RunFinalizer) {
				t.Fatalf("rollback lost its durable cleanup anchor: %#v", got)
			}

			if !strings.Contains(got.Status.Reason, "second reservation failed") {
				t.Fatalf("rollback failure was not retained: %#v", got.Status)
			}

			assertRackWorkflowCount(t, kube, 0)

			expected := run.Name
			if race {
				expected = "racing-owner"
			}

			assertRackNodeReservation(t, kube, nodes[0].Name, expected)

			failRelease = false

			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
				t.Fatal(err)
			}

			got = getTestRun(t, kube, run)
			if containsString(got.Finalizers, RunFinalizer) || !got.Status.Cleanup.ReservationsReleased {
				t.Fatalf("cleanup did not recover after rollback failure: %#v", got.Status)
			}

			expected = ""
			if race {
				expected = "racing-owner"
			}

			assertRackNodeReservation(t, kube, nodes[0].Name, expected)
		})
	}
}

func TestExplicitPendingRollbackAndStatusFailureRevalidatesHeldMembers(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"resume", "workload", "topology", "UID", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "pending-rollback", FullNodeCertificationWorkflow)
			faults := true
			kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: createMembershipObject,
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						node, ok := obj.(*corev1.Node)
						if faults && ok {
							if node.Name == nodes[1].Name && node.Annotations[ReservationLabel] == run.Name {
								return errors.New("second reservation failed")
							}

							if node.Name == nodes[0].Name && node.Annotations[ReservationLabel] == "" {
								return errors.New("rollback failed")
							}
						}

						return c.Patch(ctx, obj, patch, opts...)
					},
					SubResourcePatch: func(
						ctx context.Context, c client.Client, subresource string, obj client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						if faults && subresource == "status" {
							return errors.New("rejection status unavailable")
						}

						return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			r := membershipReconciler(t, kube)
			key := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}

			if _, err := r.Reconcile(t.Context(), key); err == nil || !strings.Contains(err.Error(), "rejection status unavailable") {
				t.Fatalf("rejection status failure = %v", err)
			}

			got := getTestRun(t, kube, run)
			if got.Status.Phase.Terminal() || got.Status.AdmissionReady || !containsString(got.Finalizers, RunFinalizer) {
				t.Fatalf("pending rollback lost its durable cleanup anchor: %#v", got.Status)
			}

			assertRackNodeReservation(t, kube, nodes[0].Name, run.Name)
			assertRackNodeReservation(t, kube, nodes[1].Name, "")
			assertRackWorkflowCount(t, kube, 0)

			faults = false
			reason := ""

			switch scenario {
			case "workload":
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "new-workload", Namespace: run.Namespace},
					Spec:       corev1.PodSpec{NodeName: nodes[0].Name},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				}
				if err := kube.Create(t.Context(), pod); err != nil {
					t.Fatal(err)
				}

				reason = "WorkloadActive"
			case "topology", "UID":
				var node corev1.Node
				if err := kube.Get(t.Context(), client.ObjectKeyFromObject(nodes[0]), &node); err != nil {
					t.Fatal(err)
				}

				if scenario == "topology" {
					node.Labels[rackTopologyKey] = "rack-b"
					reason = "topology label"
				} else {
					node.UID = "replacement-uid"
					node.Annotations[ReservationLabel] = "replacement-owner"
					reason = "UID changed"
				}

				if err := kube.Update(t.Context(), &node); err != nil {
					t.Fatal(err)
				}
			case "deadline":
				r.Now = func() time.Time { return run.CreationTimestamp.Add(run.Spec.Deadline.Duration + time.Second) }
				reason = "deadline expired"
			}

			if _, err := r.Reconcile(t.Context(), key); err != nil {
				t.Fatal(err)
			}

			got = getTestRun(t, kube, run)
			if scenario == "resume" {
				if !got.Status.AdmissionReady || got.Status.Workflow.Attempt != 1 {
					t.Fatalf("pending rollback did not safely resume: %#v", got.Status)
				}

				assertRackReservations(t, kube, nodes, run.Name)
				assertRackWorkflowCount(t, kube, 1)

				return
			}

			if got.Status.Phase != signalv1.RunPhaseInconclusive || got.Status.AdmissionReady ||
				!strings.Contains(got.Status.Reason, reason) {
				t.Fatalf("held member revalidation = %#v, want %q", got.Status, reason)
			}

			assertRackWorkflowCount(t, kube, 0)
			assertRackNodeReservation(t, kube, nodes[1].Name, "")

			expectedOwner := ""
			if scenario == "UID" {
				expectedOwner = "replacement-owner"
			}

			assertRackNodeReservation(t, kube, nodes[0].Name, expectedOwner)
		})
	}
}

func TestExplicitAdmissionResumesPartialReservationsAfterRestart(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "resumed-group", FullNodeCertificationWorkflow)
	nodes[0].Annotations = map[string]string{ReservationLabel: run.Name}
	kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).Build()

	r := membershipReconciler(t, kube)
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}

	got := getTestRun(t, kube, run)
	if !got.Status.AdmissionReady || got.Status.Workflow.Attempt != 1 {
		t.Fatalf("partial admission did not resume: %#v", got.Status)
	}

	assertRackReservations(t, kube, nodes, run.Name)
	assertRackWorkflowCount(t, kube, 1)
}

func TestExplicitRetryRevalidatesAllMembersAfterPreviousWorkloadCleanup(t *testing.T) {
	t.Parallel()

	for _, change := range []string{"UID", "topology", "reservation"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "retry-group", FullNodeCertificationWorkflow)
			setRackActive(run, nodes)

			workflow := builtinWorkflow(run, "Failed")
			for _, raw := range fixtureNodes(workflow) {
				raw.(map[string]any)["phase"] = "Failed"
			}

			objects := append(rackRunObjects(run, nodes), workflow)
			changed := false
			kube := membershipClientBuilder(t, objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: createMembershipObject,
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if err := c.Delete(ctx, obj, opts...); err != nil {
							return err
						}

						if obj.GetObjectKind().GroupVersionKind() != workflowGVK || changed {
							return nil
						}

						changed = true

						var peer corev1.Node
						if err := c.Get(ctx, client.ObjectKey{Name: nodes[1].Name}, &peer); err != nil {
							return err
						}

						switch change {
						case "UID":
							peer.UID = "replacement-uid"
							peer.Annotations[ReservationLabel] = "replacement-owner"
						case "topology":
							peer.Labels[rackTopologyKey] = "rack-b"
						case "reservation":
							peer.Annotations[ReservationLabel] = "other-run"
						}

						return c.Update(ctx, &peer)
					},
				}).Build()

			r := membershipReconciler(t, kube)
			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
				t.Fatal(err)
			}

			got := getTestRun(t, kube, run)
			if !changed || got.Status.Phase != signalv1.RunPhaseInconclusive || got.Status.Workflow.Attempt != 1 ||
				!strings.Contains(got.Status.Reason, "AdmissionRejected") || len(got.Status.Attempts) != 1 ||
				got.Status.Attempts[0].Evidence.URI == "" || got.Status.Evidence.URI == "" {
				t.Fatalf("retry did not fail closed with retained prior evidence: %#v", got.Status)
			}

			assertRackWorkflowCount(t, kube, 0)

			if !reflect.DeepEqual(got.Spec, run.Spec) {
				t.Fatal("retry modified the immutable contract")
			}

			doc := readTestEvidence(t, kube, got.Status.Evidence)
			if !strings.Contains(doc["reason"].(string), "AdmissionRejected") {
				t.Fatalf("final evidence lost admission rejection: %#v", doc)
			}
		})
	}
}

func TestExplicitCancellationWaitsForAllDescendantsAndRecoversUnrecordedWorkflow(t *testing.T) {
	t.Parallel()

	for _, unrecorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("unrecorded Workflow=%t", unrecorded), func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "cancel-group", FullNodeCertificationWorkflow)
			setRackActive(run, nodes)
			run.Spec.Cancel = true
			workflow := builtinWorkflow(run, "Running")
			backend := testOwnedBackend(run)
			backend.SetLabels(map[string]string{SnapshotRunLabel: run.Name})
			backend.SetFinalizers([]string{"test.signal.example/hold-backend"})

			if unrecorded {
				run.Status.Workflow = signalv1.WorkflowStatus{}
			}

			objects := append(rackRunObjects(run, nodes), workflow, backend)
			failDeletion := true
			kube := membershipClientBuilder(t, objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: createMembershipObject,
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if obj.GetObjectKind().GroupVersionKind() == workflowGVK && failDeletion {
							return errors.New("workflow deletion failed")
						}

						return c.Delete(ctx, obj, opts...)
					},
				}).Build()
			r := membershipReconciler(t, kube)

			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
			if _, err := r.Reconcile(t.Context(), req); err == nil || !strings.Contains(err.Error(), "deletion failed") {
				t.Fatalf("deletion error = %v", err)
			}

			assertRackReservations(t, kube, nodes, run.Name)

			got := getTestRun(t, kube, run)
			if got.Status.Phase.Terminal() || len(got.Status.Attempts) != 1 || got.Status.Attempts[0].Evidence.URI == "" {
				t.Fatalf("interrupted attempt not retained before failed deletion: %#v", got.Status)
			}

			failDeletion = false

			result, err := r.Reconcile(t.Context(), req)
			if err != nil || result.RequeueAfter == 0 {
				t.Fatalf("backend cleanup barrier = %#v, %v", result, err)
			}

			assertRackReservations(t, kube, nodes, run.Name)

			got = getTestRun(t, kube, run)
			if got.Status.Phase.Terminal() {
				t.Fatal("cancellation became terminal before backend cleanup")
			}

			if err := kube.Get(t.Context(), client.ObjectKeyFromObject(backend), backend); err != nil {
				t.Fatal(err)
			}

			backend.SetFinalizers(nil)

			if err := kube.Update(t.Context(), backend); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if _, err := r.Reconcile(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}

			got = getTestRun(t, kube, run)
			if got.Status.Phase != signalv1.RunPhaseCancelled || !got.Status.Cleanup.ReservationsReleased ||
				containsString(got.Finalizers, RunFinalizer) {
				t.Fatalf("group cancellation did not finish cleanup: %#v", got.Status)
			}

			assertRackReservations(t, kube, nodes, "")

			for _, node := range nodes {
				var current corev1.Node
				if err := kube.Get(t.Context(), client.ObjectKey{Name: node.Name}, &current); err != nil {
					t.Fatal(err)
				}

				if current.Labels[RequestLabel] != "" {
					t.Fatalf("member %s retained a duplicate manual trigger", node.Name)
				}
			}
		})
	}
}

func TestExplicitGroupMissingCoverageAndWrongMemberEvidenceNeverPass(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"missing peer", "missing test", "wrong UID", "peer failed", "unsupported fabric"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "results-group", FullNodeCertificationWorkflow)
			setRackActive(run, nodes)
			workflow := builtinWorkflow(run, "Succeeded")

			switch mode {
			case "missing peer":
				for id := range fixtureNodes(workflow) {
					if strings.HasPrefix(id, nodes[1].Name+"-") {
						delete(fixtureNodes(workflow), id)
					}
				}
			case "missing test":
				delete(fixtureNodes(workflow), fixtureLeafID(nodes[1].Name, "mpi-rank-access"))
			case "wrong UID":
				setTestOutput(workflow, fixtureLeafID(nodes[1].Name, "gpu-health"), "node-uid", string(nodes[0].UID))
			case "peer failed":
				setTestOutput(workflow, fixtureLeafID(nodes[1].Name, "gpu-health"), "verdict", "Fail")
				setTestOutput(workflow, fixtureLeafID(nodes[1].Name, "gpu-health"), "reason", "PeerGPUFailure")
			case "unsupported fabric":
				run.Spec.Parameters["rack-tests"] = `["fabric"]`
			}

			collection := collectTestResults(run, workflow, true)
			if collection.verdict == "Pass" || !collection.strict {
				t.Fatalf("incomplete or unsupported group passed: %#v", collection)
			}

			if mode == "peer failed" && collection.verdict != "Fail" {
				t.Fatalf("per-node failure did not fail the whole group: %#v", collection)
			}

			if mode != "unsupported fabric" && len(collection.results) != len(run.Spec.TestPlan) {
				t.Fatal("missing execution discarded declared per-node evidence")
			}
		})
	}
}

func TestExplicitGroupRetryRetainsDistinctNodeTestAttemptEvidenceAndCleansSuccess(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "evidence-group", FullNodeCertificationWorkflow)
	setRackActive(run, nodes)

	workflow := builtinWorkflow(run, "Failed")
	for _, raw := range fixtureNodes(workflow) {
		raw.(map[string]any)["phase"] = "Failed"
	}

	kube := membershipClientBuilder(t, append(rackRunObjects(run, nodes), workflow)...).Build()
	r := membershipReconciler(t, kube)
	retryAt := membershipNow().Add(run.Spec.Deadline.Duration * 3 / 4)
	r.Now = func() time.Time { return retryAt }

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	retried := getTestRun(t, kube, run)
	if retried.Status.Workflow.Attempt != 2 || len(retried.Status.Attempts) != 1 ||
		retried.Status.Attempts[0].Evidence.URI == "" || len(retried.Status.TestResults) != 8 {
		t.Fatalf("retry lost the first attempt: %#v", retried.Status)
	}

	assertRackReservations(t, kube, nodes, run.Name)

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: retried.Namespace, Name: retried.Status.Workflow.Name}, current); err != nil {
		t.Fatal(err)
	}

	deadlineSeconds, foundDeadline, deadlineErr := unstructured.NestedInt64(current.Object, "spec", "activeDeadlineSeconds")
	if deadlineErr != nil || !foundDeadline || deadlineSeconds != int64(run.Spec.Deadline.Seconds()/4) {
		t.Fatalf("retry deadline = %d, %v; pending and prior attempt time must not reset the pinned deadline", deadlineSeconds, deadlineErr)
	}

	current.Object["status"] = builtinWorkflow(retried, "Succeeded").Object["status"]
	if err := kube.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	got := getTestRun(t, kube, run)
	if got.Status.Phase != signalv1.RunPhaseSucceeded || got.Status.Verdict != "Pass" ||
		len(got.Status.Attempts) != 2 || len(got.Status.TestResults) != 16 {
		t.Fatalf("complete grouped result = %#v", got.Status)
	}

	keys := map[string]bool{}

	for _, result := range got.Status.TestResults {
		key := fmt.Sprintf("%d/%s/%s", result.Attempt, result.NodeUID, result.ID)
		if keys[key] || result.Evidence.URI == "" || result.WorkflowUID == "" {
			t.Fatalf("lost node/test/attempt provenance: %#v", result)
		}

		keys[key] = true
	}

	if got.Status.Attempts[0].Evidence.URI == got.Status.Attempts[1].Evidence.URI {
		t.Fatal("distinct attempts share one evidence object")
	}

	doc := readTestEvidence(t, kube, got.Status.Evidence)

	admission, ok := doc["admission"].(map[string]any)
	if !ok || admission["topologyValue"] != "rack-a" || len(doc["testResults"].([]any)) != 16 {
		t.Fatalf("final evidence omitted membership or prior attempts: %#v", doc)
	}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	got = getTestRun(t, kube, run)
	if !got.Status.Cleanup.ReservationsReleased || containsString(got.Finalizers, RunFinalizer) {
		t.Fatalf("successful group did not finish cleanup: %#v", got.Status)
	}

	assertRackWorkflowCount(t, kube, 0)
	assertRackReservations(t, kube, nodes, "")

	for _, node := range nodes {
		var projected corev1.Node
		if err := kube.Get(t.Context(), client.ObjectKey{Name: node.Name}, &projected); err != nil {
			t.Fatal(err)
		}

		if projected.Labels[StateLabel] != "passed" || projected.Labels[EligibilityLabel] != "production" {
			t.Fatalf("complete group was not projected on %s: %v", node.Name, projected.Labels)
		}
	}
}

func TestExplicitAdmissionRequiresCleanupAnchorAndPinnedImmutableScript(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"no finalizer", "snapshot unbound", "script changed", "snapshot mutable", "snapshot foreign owner"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "script-group", FullNodeCertificationWorkflow)
			objects := rackRunObjects(run, nodes)
			snapshot := objects[1].(*corev1.ConfigMap)

			switch mode {
			case "no finalizer":
				run.Finalizers = nil
			case "snapshot unbound":
				snapshot.OwnerReferences = nil
			case "script changed":
				snapshot.Data[run.Spec.Script.Key] = "unapproved script"
			case "snapshot mutable":
				snapshot.Immutable = nil
			case "snapshot foreign owner":
				snapshot.OwnerReferences[0].UID = "other-run-uid"
			}

			kube := membershipClientBuilder(t, objects...).Build()
			r := membershipReconciler(t, kube)

			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
			if _, err := r.Reconcile(t.Context(), req); err != nil {
				t.Fatal(err)
			}

			if mode == "no finalizer" {
				assertRackWorkflowCount(t, kube, 0)
				assertRackReservations(t, kube, nodes, "")

				got := getTestRun(t, kube, run)
				if !containsString(got.Finalizers, RunFinalizer) {
					t.Fatal("no durable cleanup anchor before reservation acquisition")
				}

				if _, err := r.Reconcile(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}

			got := getTestRun(t, kube, run)
			if strings.HasPrefix(mode, "script ") || mode == "snapshot mutable" || mode == "snapshot foreign owner" {
				if got.Status.Phase != signalv1.RunPhaseInconclusive {
					t.Fatalf("untrusted snapshot was admitted: %#v", got.Status)
				}

				assertRackWorkflowCount(t, kube, 0)
				assertRackReservations(t, kube, nodes, "")
			} else {
				if !got.Status.AdmissionReady {
					t.Fatalf("resumable setup failed: %#v", got.Status)
				}

				assertRackWorkflowCount(t, kube, 1)
			}
		})
	}
}

func TestExplicitSafetyStateUnavailableRejectsWholeSet(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"WorkloadStateUnavailable", "MaintenanceStateUnavailable"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run, nodes := rackTestRun(t, "unavailable-group", FullNodeCertificationWorkflow)
			kube := membershipClientBuilder(t, rackRunObjects(run, nodes)...).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: createMembershipObject,
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						_, pods := list.(*corev1.PodList)

						_, machines := list.(*machinav1.MachineList)
						if mode == "WorkloadStateUnavailable" && pods || mode == "MaintenanceStateUnavailable" && machines {
							return errors.New("safety state unavailable")
						}

						return c.List(ctx, list, opts...)
					},
				}).Build()

			r := membershipReconciler(t, kube)
			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
				t.Fatal(err)
			}

			got := getTestRun(t, kube, run)
			if got.Status.Phase != signalv1.RunPhaseInconclusive || !strings.Contains(got.Status.Reason, mode) {
				t.Fatalf("unavailable safety guard = %#v", got.Status)
			}

			assertRackReservations(t, kube, nodes, "")
			assertRackWorkflowCount(t, kube, 0)
		})
	}
}

func TestExplicitDeletingRunPreservesEvidenceAndDoesNotTouchRecreatedMember(t *testing.T) {
	t.Parallel()

	run, nodes := rackTestRun(t, "deleting-group", FullNodeCertificationWorkflow)
	setRackActive(run, nodes)
	workflow := builtinWorkflow(run, "Running")
	objects := append(rackRunObjects(run, nodes), workflow)
	failSnapshotDelete := true

	kube := membershipClientBuilder(t, objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: createMembershipObject,
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && obj.GetName() == run.Spec.Script.ConfigMapName && failSnapshotDelete {
					return errors.New("snapshot deletion failed")
				}

				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	if err := kube.Delete(t.Context(), run); err != nil {
		t.Fatal(err)
	}

	var peer corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKey{Name: nodes[1].Name}, &peer); err != nil {
		t.Fatal(err)
	}

	peer.UID = "replacement-uid"

	peer.Annotations[ReservationLabel] = "replacement-owner"
	if err := kube.Update(t.Context(), &peer); err != nil {
		t.Fatal(err)
	}

	r := membershipReconciler(t, kube)

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := r.Reconcile(t.Context(), req); err == nil || !strings.Contains(err.Error(), "snapshot deletion failed") {
		t.Fatalf("snapshot deletion error = %v", err)
	}

	got := getTestRun(t, kube, run)
	if !containsString(got.Finalizers, RunFinalizer) || got.Status.Evidence.URI == "" {
		t.Fatalf("deletion lost evidence or its retryable finalizer: %#v", got)
	}

	assertRackWorkflowCount(t, kube, 0)
	assertRackNodeReservation(t, kube, nodes[0].Name, "")
	assertRackNodeReservation(t, kube, nodes[1].Name, "replacement-owner")

	failSnapshotDelete = false

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(run), &signalv1.CertificationRun{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted group still has a finalizer: %v", err)
	}

	if err := kube.Get(t.Context(), client.ObjectKey{Name: nodes[1].Name}, &peer); err != nil {
		t.Fatal(err)
	}

	if peer.Annotations[ReservationLabel] != "replacement-owner" || peer.Labels[RequestLabel] != run.Spec.Profile.Name {
		t.Fatal("cleanup mutated the replacement Node's reservation or request")
	}
}

func rackProfileAndNodes() (*signalv1.CertificationProfile, []*corev1.Node) {
	profile := testProfile("rack-profile", "example-gpu")
	profile.Spec.Participants.Count = 2
	profile.Spec.Participants.NodeNames = []string{"gpu-1", "gpu-0"}
	profile.Spec.Participants.TopologyKey = rackTopologyKey
	profile.Spec.Workflow.Name = RackCertificationWorkflow
	profile.Spec.Parameters = map[string]string{NodeWorkflowTemplateParameter: FullNodeCertificationWorkflow, "rack-tests": "[]"}

	nodes := []*corev1.Node{readyNode("gpu-0", "example-gpu"), readyNode("gpu-1", "example-gpu")}
	for _, node := range nodes {
		node.Labels[rackTopologyKey] = "rack-a"
		node.Labels[RequestLabel] = profile.Name
		node.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	}

	return profile, nodes
}

func rackTestRun(t *testing.T, name, nodeWorkflow string) (*signalv1.CertificationRun, []*corev1.Node) {
	t.Helper()

	profile, nodes := rackProfileAndNodes()
	run := testRun(name)
	run.CreationTimestamp = metav1.NewTime(membershipNow())
	run.Spec.Profile.Name = profile.Name
	run.Spec.Workflow = profile.Spec.Workflow
	run.Spec.Parameters = map[string]string{NodeWorkflowTemplateParameter: nodeWorkflow, "rack-tests": "[]"}
	run.Spec.Admission = &signalv1.AdmissionSnapshot{
		Participants: *profile.Spec.Participants.DeepCopy(), TopologyValue: "rack-a", Trigger: "manual",
	}
	run.Spec.Participants = nil

	run.Spec.ParticipantPlan = nil
	for _, node := range nodes {
		run.Spec.Participants = append(run.Spec.Participants, signalv1.Participant{NodeName: node.Name, NodeUID: node.UID})
		run.Spec.ParticipantPlan = append(run.Spec.ParticipantPlan, participantSnapshot(node))
	}

	plan, _, err := makeTestPlan(&run.Spec)
	if err != nil {
		t.Fatal(err)
	}

	run.Spec.TestPlan = plan

	return run, nodes
}

func rackRunObjects(run *signalv1.CertificationRun, nodes []*corev1.Node) []client.Object {
	snapshot := runSnapshot(run)
	immutable := true
	snapshot.Immutable = &immutable
	snapshot.Labels = map[string]string{SnapshotRunLabel: run.Name}
	snapshot.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))}

	objects := []client.Object{run, snapshot}
	for _, node := range nodes {
		objects = append(objects, node)
	}

	return objects
}

func setRackActive(run *signalv1.CertificationRun, nodes []*corev1.Node) {
	run.Status.AdmissionReady = true
	run.Status.Phase = signalv1.RunPhaseRunning

	run.Status.Workflow = signalv1.WorkflowStatus{Name: run.Name + "-a1", UID: "workflow-uid", Attempt: 1, Phase: "Running"}
	for _, node := range nodes {
		node.Annotations = map[string]string{ReservationLabel: run.Name}
	}
}

func membershipClientBuilder(t *testing.T, objects ...client.Object) *fake.ClientBuilder {
	t.Helper()

	scheme := testScheme(t)
	if err := machinav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}, &signalv1.CertificationProfile{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{Create: createMembershipObject})
}

func createMembershipObject(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID(obj.GetName() + "-uid"))
	}

	created := obj.GetCreationTimestamp()
	if created.IsZero() {
		obj.SetCreationTimestamp(metav1.NewTime(membershipNow()))
	}

	return c.Create(ctx, obj, opts...)
}

func membershipNow() time.Time {
	return time.Unix(1000, 0).UTC()
}

func membershipReconciler(t *testing.T, kube client.Client) *RunReconciler {
	t.Helper()

	r := evidenceReconciler(t, kube)
	r.Now = membershipNow

	return r
}

func assertRackWorkflowCount(t *testing.T, kube client.Client, want int) {
	t.Helper()

	workflows := &unstructured.UnstructuredList{}
	workflows.SetGroupVersionKind(workflowGVK.GroupVersion().WithKind("WorkflowList"))

	if err := kube.List(t.Context(), workflows); err != nil {
		t.Fatal(err)
	}

	if len(workflows.Items) != want {
		t.Fatalf("Workflows = %d, want %d", len(workflows.Items), want)
	}
}

func assertRackReservations(t *testing.T, kube client.Client, nodes []*corev1.Node, owner string) {
	t.Helper()

	for _, node := range nodes {
		assertRackNodeReservation(t, kube, node.Name, owner)
	}
}

func assertRackNodeReservation(t *testing.T, kube client.Client, name, owner string) {
	t.Helper()

	var node corev1.Node
	if err := kube.Get(t.Context(), client.ObjectKey{Name: name}, &node); err != nil {
		t.Fatal(err)
	}

	if node.Annotations[ReservationLabel] != owner {
		t.Fatalf("Node %s reservation = %q, want %q", name, node.Annotations[ReservationLabel], owner)
	}
}
