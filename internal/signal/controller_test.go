// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestAdmissionCreatesImmutableSnapshotAndPreventsDuplicate(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.Script.RequiresRuntimeStage = true
	profile.Spec.RuntimeStage = &signalv1.RuntimeStage{Name: "collective-bandwidth"}
	profile.Spec.Backend.Queue = "certification"
	profile.Spec.WorkloadPolicy = signalv1.WorkloadPolicy{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
		},
		HostNetwork: true,
		KueueTopology: &signalv1.KueueTopologyPolicy{
			RequiredLevel: corev1.LabelHostname,
		},
		Volumes: []signalv1.WorkloadVolume{{
			Name:      "work",
			MountPath: "/work",
			EmptyDir:  &corev1.EmptyDirVolumeSource{SizeLimit: quantity("8Gi")},
		}},
	}

	const payload = "test payload\n"

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": payload},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	now := time.Unix(100, 0)
	r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal", Now: func() time.Time { return now }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}}); err != nil {
		t.Fatalf("duplicate Reconcile: %v", err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs.Items) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs.Items))
	}

	run := runs.Items[0]
	if run.Spec.Script.Digest != digestString(payload) {
		t.Fatalf("script digest = %q", run.Spec.Script.Digest)
	}

	if len(run.Spec.StagePlan) != 1 || run.Spec.StagePlan[0].ID != "workload" {
		t.Fatalf("stage plan = %#v, want immutable profile plan", run.Spec.StagePlan)
	}

	if !run.Spec.Script.RequiresRuntimeStage ||
		run.Spec.RuntimeStage == nil ||
		run.Spec.RuntimeStage.Name != "collective-bandwidth" ||
		run.Spec.BackendActiveDeadline.Duration != 9*time.Minute {
		t.Fatalf("runtime contract = script %#v stage %#v backend deadline %s",
			run.Spec.Script, run.Spec.RuntimeStage, run.Spec.BackendActiveDeadline.Duration)
	}

	if run.Spec.Purpose != signalv1.PurposeQuickCertification {
		t.Fatalf("purpose = %q, want %q", run.Spec.Purpose, signalv1.PurposeQuickCertification)
	}

	if !run.Spec.WorkloadPolicy.HostNetwork ||
		run.Spec.WorkloadPolicy.KueueTopology == nil ||
		run.Spec.WorkloadPolicy.KueueTopology.RequiredLevel != corev1.LabelHostname ||
		run.Spec.WorkloadPolicy.Resources.Limits.Cpu().String() != "8" ||
		len(run.Spec.WorkloadPolicy.Volumes) != 1 ||
		run.Spec.WorkloadPolicy.Volumes[0].EmptyDir.SizeLimit.String() != "8Gi" {
		t.Fatalf("workload policy = %#v, want immutable profile policy", run.Spec.WorkloadPolicy)
	}

	if run.Spec.Parameters["observed-gpu-capacity"] != "8" ||
		run.Spec.Parameters["observed-gpu-allocatable"] != "8" {
		t.Fatalf("observed GPU inventory = %#v", run.Spec.Parameters)
	}

	if run.Annotations[RunTriggerAnnotation] != "manual" {
		t.Fatalf("trigger = %q, want manual", run.Annotations[RunTriggerAnnotation])
	}

	var snapshot corev1.ConfigMap
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "signal", Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}

	if snapshot.Immutable == nil || !*snapshot.Immutable {
		t.Fatal("snapshot is mutable")
	}

	if snapshot.Data["payload.txt"] != payload {
		t.Fatalf("snapshot payload = %q", snapshot.Data["payload.txt"])
	}

	var locked corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &locked); err != nil {
		t.Fatalf("get locked node: %v", err)
	}

	if locked.Annotations[ReservationLabel] != run.Name {
		t.Fatalf("reservation = %q, want %q", locked.Annotations[ReservationLabel], run.Name)
	}
}

func TestAdmissionRejectsMissingRequiredRuntimeStageBeforeReservation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.Script.RequiresRuntimeStage = true
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	_, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	)
	if err == nil || !strings.Contains(err.Error(), "runtime stage is required") {
		t.Fatalf("Reconcile error = %v", err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Annotations[ReservationLabel] != "" {
		t.Fatalf("node was reserved after rejected runtime contract: %#v", got.Annotations)
	}
}

func TestAdmissionRejectsInsufficientBackendDeadlineReserveBeforeReservation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.BackendActiveDeadline = metav1.Duration{
		Duration: profile.Spec.Deadline.Duration - 59*time.Second,
	}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	_, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	)
	if err == nil || !strings.Contains(err.Error(), "leave at least 1m0s") {
		t.Fatalf("Reconcile error = %v", err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Annotations[ReservationLabel] != "" {
		t.Fatalf("node was reserved after rejected deadlines: %#v", got.Annotations)
	}
}

func TestAdmissionRejectsMutableRunnerImageBeforeReservation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.RunnerImage = "registry.example/runner:latest"
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	_, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	)
	if err == nil || !strings.Contains(err.Error(), "immutable sha256 digest") {
		t.Fatalf("Reconcile error = %v", err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Annotations[ReservationLabel] != "" {
		t.Fatalf("node was reserved after rejected profile: %#v", got.Annotations)
	}
}

func TestAdmissionRejectsInvalidWorkloadPolicyBeforeReservation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.WorkloadPolicy.Volumes = []signalv1.WorkloadVolume{{
		Name:      "replacement",
		MountPath: "/results",
		EmptyDir:  &corev1.EmptyDirVolumeSource{},
	}}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	_, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	)
	if err == nil || !strings.Contains(err.Error(), "reserved or existing mountPath") {
		t.Fatalf("Reconcile error = %v", err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Annotations[ReservationLabel] != "" {
		t.Fatalf("node was reserved after rejected workload policy: %#v", got.Annotations)
	}
}

func TestAdmissionRejectsKueueTopologyWithoutQueueBeforeReservation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.Backend = signalv1.ExecutionBackend{APIVersion: "v1", Kind: "Pod"}
	profile.Spec.WorkloadPolicy.KueueTopology = &signalv1.KueueTopologyPolicy{
		RequiredLevel: corev1.LabelHostname,
	}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	_, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	)
	if err == nil || !strings.Contains(err.Error(), "kueueTopology requires a backend queue") {
		t.Fatalf("Reconcile error = %v", err)
	}

	var got corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Annotations[ReservationLabel] != "" {
		t.Fatalf("node was reserved after rejected topology policy: %#v", got.Annotations)
	}
}

func TestParticipantSelectionIsDeterministicAndSkipsLockedNodes(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	request := readyNode("gpu-0", "H200")
	peer := readyNode("gpu-1", "H200")
	locked := readyNode("gpu-2", "H200")
	locked.Annotations = map[string]string{ReservationLabel: "other-run"}
	wrong := readyNode("gpu-3", "A100")
	profile := testProfile("h200-pair", "H200")
	profile.Spec.Participants.Count = 2
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, peer, locked, wrong).Build()
	r := &AdmissionReconciler{Client: kube}

	participants, err := r.selectParticipants(context.Background(), request, profile)
	if err != nil {
		t.Fatalf("selectParticipants: %v", err)
	}

	if len(participants) != 2 ||
		participants[0].Index != 0 ||
		participants[0].NodeName != request.Name ||
		participants[1].Index != 1 ||
		participants[1].NodeName != peer.Name {
		t.Fatalf("participants = %#v", participants)
	}

	gpu := corev1.ResourceName("nvidia.com/gpu")
	capacity := participants[1].Capacity[gpu]

	allocatable := participants[1].Allocatable[gpu]
	if capacity.Cmp(resource.MustParse("8")) != 0 ||
		allocatable.Cmp(resource.MustParse("8")) != 0 {
		t.Fatalf("participant inventory = %#v", participants[1])
	}
}

func TestParticipantSelectionRejectsInsufficientEligibleNodes(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	request := readyNode("gpu-0", "H200")
	profile := testProfile("h200-pair", "H200")
	profile.Spec.Participants.Count = 2
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request).Build()
	r := &AdmissionReconciler{Client: kube}

	if _, err := r.selectParticipants(context.Background(), request, profile); err == nil ||
		!strings.Contains(err.Error(), "requires 2 eligible unlocked participants, found 1") {
		t.Fatalf("selectParticipants error = %v", err)
	}
}

func TestMultiNodeAdmissionReservesEveryParticipant(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	request := readyNode("gpu-0", "H200")
	request.Labels[RequestLabel] = "h200-pair"
	peer := readyNode("gpu-1", "H200")
	profile := testProfile("h200-pair", "H200")
	profile.Spec.Participants.Count = 2
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, peer, profile, source).Build()
	r := &AdmissionReconciler{
		Client:          kube,
		Namespace:       "signal",
		ScriptNamespace: "signal",
		Now:             func() time.Time { return time.Unix(100, 0) },
	}

	if _, err := r.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: request.Name}},
	); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(context.Background(), &runs); err != nil {
		t.Fatal(err)
	}

	if len(runs.Items) != 1 || len(runs.Items[0].Spec.Participants) != 2 {
		t.Fatalf("runs = %#v", runs.Items)
	}

	for index, name := range []string{"gpu-0", "gpu-1"} {
		participant := runs.Items[0].Spec.Participants[index]
		if participant.Index != int32(index) || participant.NodeName != name {
			t.Fatalf("participant %d = %#v", index, participant)
		}

		var node corev1.Node
		if err := kube.Get(context.Background(), client.ObjectKey{Name: name}, &node); err != nil {
			t.Fatal(err)
		}

		if node.Annotations[ReservationLabel] != runs.Items[0].Name {
			t.Fatalf("Node %s reservation = %q", name, node.Annotations[ReservationLabel])
		}
	}
}

func TestMultiNodeReservationRollsBackPartialFailure(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	first := readyNode("gpu-0", "H200")
	second := readyNode("gpu-1", "H200")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(first, second).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				node, ok := obj.(*corev1.Node)
				if ok && node.Name == second.Name && node.Annotations[ReservationLabel] == "run-pair" {
					return errors.New("reservation conflict")
				}

				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &AdmissionReconciler{Client: kube}
	participants := []signalv1.Participant{
		participantSnapshot(first, 0),
		participantSnapshot(second, 1),
	}

	if err := r.reserveParticipants(context.Background(), participants, "run-pair"); err == nil {
		t.Fatal("reserveParticipants error = nil")
	}

	var recovered corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: first.Name}, &recovered); err != nil {
		t.Fatal(err)
	}

	if recovered.Annotations[ReservationLabel] != "" {
		t.Fatalf("partial reservation was not rolled back: %#v", recovered.Annotations)
	}
}

func TestAdmissionFailureMakesCreatedRunInertAndReleasesReservation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		failSnapshot   bool
		failProjection bool
	}{
		{name: "snapshot binding", failSnapshot: true},
		{name: "pending projection", failProjection: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scheme := testScheme(t)
			node := readyNode("gpu-0", "example-gpu")
			node.Labels[RequestLabel] = "gpu-smoke"
			profile := testProfile("gpu-smoke", "example-gpu")
			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
				Data:       map[string]string{"payload.txt": "test payload\n"},
			}
			kube := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(node, profile, source).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if snapshot, ok := obj.(*corev1.ConfigMap); ok &&
							tc.failSnapshot && snapshot.Name != source.Name {
							return errors.New("snapshot update failed")
						}

						return c.Update(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if projected, ok := obj.(*corev1.Node); ok &&
							tc.failProjection && projected.Labels[StateLabel] == "pending" {
							return errors.New("projection patch failed")
						}

						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			r := &AdmissionReconciler{
				Client:          kube,
				Namespace:       "signal",
				ScriptNamespace: "signal",
				Now:             func() time.Time { return time.Unix(100, 0) },
			}

			if _, err := r.Reconcile(
				context.Background(),
				reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
			); err == nil {
				t.Fatal("Reconcile error = nil, want failed admission")
			}

			var runs signalv1.CertificationRunList
			if err := kube.List(context.Background(), &runs); err != nil {
				t.Fatalf("list runs: %v", err)
			}

			for i := range runs.Items {
				if runs.Items[i].DeletionTimestamp.IsZero() {
					t.Fatalf("run %s remained active after failed admission", runs.Items[i].Name)
				}
			}

			var cleaned corev1.Node
			if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &cleaned); err != nil {
				t.Fatalf("get cleaned node: %v", err)
			}

			if cleaned.Annotations[ReservationLabel] != "" {
				t.Fatalf("reservation remained after inerting failed run: %q", cleaned.Annotations[ReservationLabel])
			}
		})
	}
}

func TestAdmissionRecoversOrphanedReservationAndSnapshot(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "gpu-smoke"
	node.Annotations = map[string]string{ReservationLabel: "orphaned-run"}
	peer := readyNode("gpu-1", "example-gpu")
	peer.Annotations = map[string]string{ReservationLabel: "orphaned-run"}
	profile := testProfile("gpu-smoke", "example-gpu")
	profile.Spec.Participants.Count = 2
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	immutable := true
	orphanedSnapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orphaned-run-scripts",
			Namespace: "signal",
			Labels:    map[string]string{SnapshotRunLabel: "orphaned-run"},
		},
		Immutable: &immutable,
		Data:      map[string]string{"payload.txt": "orphaned\n"},
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, peer, profile, source, orphanedSnapshot).
		Build()
	r := &AdmissionReconciler{
		Client:          kube,
		APIReader:       kube,
		Namespace:       "signal",
		ScriptNamespace: "signal",
		Now:             func() time.Time { return time.Unix(100, 0) },
	}

	if _, err := r.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if err := kube.Get(
		context.Background(),
		client.ObjectKeyFromObject(orphanedSnapshot),
		&corev1.ConfigMap{},
	); !apierrors.IsNotFound(err) {
		t.Fatalf("orphaned snapshot get error = %v, want NotFound", err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs.Items) != 1 {
		t.Fatalf("runs = %d, want recovered admission", len(runs.Items))
	}

	for _, nodeName := range []string{node.Name, peer.Name} {
		var recovered corev1.Node
		if err := kube.Get(context.Background(), client.ObjectKey{Name: nodeName}, &recovered); err != nil {
			t.Fatalf("get recovered node %s: %v", nodeName, err)
		}

		if recovered.Annotations[ReservationLabel] != runs.Items[0].Name {
			t.Fatalf(
				"Node %s reservation = %q, want %q",
				nodeName,
				recovered.Annotations[ReservationLabel],
				runs.Items[0].Name,
			)
		}
	}
}

func TestDeletingRunClearsReservationAndRequest(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("deleting-run")
	run.Finalizers = []string{RunFinalizer}
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run, node).Build()
	reconciler := &RunReconciler{Client: kube}

	if _, err := reconciler.reconcileDeleting(context.Background(), run); err != nil {
		t.Fatalf("reconcileDeleting: %v", err)
	}

	var cleaned corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &cleaned); err != nil {
		t.Fatalf("get cleaned node: %v", err)
	}

	if cleaned.Annotations[ReservationLabel] != "" || cleaned.Labels[RequestLabel] != "" {
		t.Fatalf("deletion cleanup left trigger metadata: labels=%v annotations=%v", cleaned.Labels, cleaned.Annotations)
	}
}

func TestCancellationWaitsForWorkflowDeletionBeforeReleasingNode(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("canceling-run")
	run.Spec.Cancel = true
	run.Status.Phase = signalv1.RunPhaseRunning
	run.Status.Workflow = signalv1.WorkflowStatus{Name: "canceling-run-a1", Attempt: 1}
	node := readyNode(run.Spec.RequestNodeName, "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	workflow := workflowObject(run, "Running", "", "")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, workflow).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(
				_ context.Context,
				_ client.WithWatch,
				_ client.Object,
				_ ...client.DeleteOption,
			) error {
				return nil
			},
		}).
		Build()
	reconciler := &RunReconciler{Client: kube}

	result, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)},
	)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Fatal("cancellation did not wait for Workflow deletion")
	}

	var gotRun signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &gotRun); err != nil {
		t.Fatal(err)
	}

	if gotRun.Status.Phase.Terminal() {
		t.Fatalf("run became terminal before Workflow deletion: %#v", gotRun.Status)
	}

	var gotNode corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &gotNode); err != nil {
		t.Fatal(err)
	}

	if gotNode.Annotations[ReservationLabel] != run.Name {
		t.Fatalf("reservation was released before Workflow deletion: %v", gotNode.Annotations)
	}
}

func TestWorkflowPinsAttemptInputs(t *testing.T) {
	t.Parallel()

	run := testRun("run-1")
	run.UID = types.UID("run-uid")
	run.Spec.Parameters = map[string]string{
		"expected-gpu-count": "8",
		"health-mode":        "snapshot",
		"mpi-payload-mode":   "script",
	}
	run.Spec.Script.RequiresRuntimeStage = true
	run.Spec.RuntimeStage = &signalv1.RuntimeStage{Name: "collective-bandwidth"}
	run.Spec.Tolerations = []corev1.Toleration{{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	run.Spec.WorkloadPolicy = signalv1.WorkloadPolicy{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
		},
		HostNetwork: true,
		KueueTopology: &signalv1.KueueTopologyPolicy{
			RequiredLevel: corev1.LabelHostname,
		},
		Volumes: []signalv1.WorkloadVolume{{
			Name:      "work",
			MountPath: "/work",
			EmptyDir:  &corev1.EmptyDirVolumeSource{SizeLimit: quantity("8Gi")},
		}},
	}

	workflow, err := buildWorkflow(run, 2)
	if err != nil {
		t.Fatal(err)
	}

	if workflow.GetName() != "run-1-a2" {
		t.Fatalf("workflow name = %q", workflow.GetName())
	}

	if got, _, _ := unstructured.NestedInt64(workflow.Object, "spec", "ttlStrategy", "secondsAfterCompletion"); got != 21600 {
		t.Fatalf("workflow completion TTL = %d, want 21600", got)
	}

	if got, _, _ := unstructured.NestedInt64(workflow.Object, "spec", "activeDeadlineSeconds"); got != 600 {
		t.Fatalf("workflow active deadline = %d, want outer deadline 600", got)
	}

	if got, _, _ := unstructured.NestedString(workflow.Object, "spec", "podGC", "deleteDelayDuration"); got != "2h" {
		t.Fatalf("workflow pod GC delay = %q, want 2h", got)
	}

	if len(workflow.GetOwnerReferences()) != 1 || workflow.GetOwnerReferences()[0].UID != run.UID {
		t.Fatalf("owner references = %#v", workflow.GetOwnerReferences())
	}

	parameters, found, err := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
	if err != nil || !found {
		t.Fatalf("workflow parameters: found=%t err=%v", found, err)
	}

	values := map[string]string{}

	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		values[parameter["name"].(string)] = parameter["value"].(string)
	}

	if values["script-digest"] != run.Spec.Script.Digest || values["backend-kind"] != "MPIJob" || values["attempt"] != "2" {
		t.Fatalf("workflow values = %#v", values)
	}

	if values["script-requires-runtime-stage"] != "true" ||
		values["runtime-stage-name"] != "collective-bandwidth" ||
		values["backend-active-deadline-seconds"] != "540" {
		t.Fatalf("workflow runtime contract = %#v", values)
	}

	if values["participant-count"] != "1" || values["request-node-name"] != run.Spec.RequestNodeName {
		t.Fatalf("workflow participant metadata = %#v", values)
	}

	var participants []signalv1.Participant
	if err := json.Unmarshal([]byte(values["participants-json"]), &participants); err != nil {
		t.Fatalf("decode participants: %v", err)
	}

	if len(participants) != 1 ||
		participants[0].NodeName != run.Spec.Participants[0].NodeName ||
		participants[0].NodeUID != run.Spec.Participants[0].NodeUID {
		t.Fatalf("workflow participants = %#v", participants)
	}

	if values["script-configmap"] != run.Spec.Script.ConfigMapName || values["script-configmap"] == "scripts" {
		t.Fatalf("workflow must mount the immutable snapshot, values = %#v", values)
	}

	if !strings.Contains(values["tolerations"], `"key":"nvidia.com/gpu"`) {
		t.Fatalf("workflow tolerations = %q", values["tolerations"])
	}

	var environment map[string]string
	if err := json.Unmarshal([]byte(values["script-environment"]), &environment); err != nil {
		t.Fatalf("decode script environment: %v", err)
	}

	if environment["EXPECTED_GPU_COUNT"] != "8" ||
		environment["HEALTH_MODE"] != "snapshot" ||
		values["mpi-payload-mode"] != "script" {
		t.Fatalf("workflow script policy = %#v, parameters = %#v", environment, values)
	}

	var tolerations []corev1.Toleration
	if err := json.Unmarshal([]byte(values["tolerations"]), &tolerations); err != nil {
		t.Fatalf("decode workflow tolerations: %v", err)
	}

	if len(tolerations) != 1 {
		t.Fatalf("workflow tolerations = %#v, want one deduplicated GPU toleration", tolerations)
	}

	var policy signalv1.WorkloadPolicy
	if err := json.Unmarshal([]byte(values["workload-policy"]), &policy); err != nil {
		t.Fatalf("decode workload policy: %v", err)
	}

	if !policy.HostNetwork ||
		policy.KueueTopology == nil ||
		policy.KueueTopology.RequiredLevel != corev1.LabelHostname ||
		policy.Resources.Requests.Cpu().String() != "4" ||
		policy.Resources.Limits.Cpu().String() != "8" ||
		len(policy.Volumes) != 1 ||
		policy.Volumes[0].EmptyDir.SizeLimit.String() != "8Gi" {
		t.Fatalf("workflow workload policy = %#v", policy)
	}
}

func TestBuildWorkflowRejectsParameterThatCannotBecomeEnvironmentVariable(t *testing.T) {
	t.Parallel()

	run := testRun("run-invalid-parameter")
	run.Spec.Parameters = map[string]string{"invalid.parameter": "value"}

	if _, err := buildWorkflow(run, 1); err == nil ||
		!strings.Contains(err.Error(), "cannot be mapped to a script environment variable") {
		t.Fatalf("buildWorkflow error = %v", err)
	}
}

func TestScriptEnvironmentRejectsCollidingParameterNames(t *testing.T) {
	t.Parallel()

	_, err := scriptEnvironment(map[string]string{
		"gpu-devices": "0,1",
		"gpu_devices": "2,3",
	})
	if err == nil || !strings.Contains(err.Error(), "both map to GPU_DEVICES") {
		t.Fatalf("scriptEnvironment error = %v", err)
	}
}

func TestScriptEnvironmentRejectsReservedNames(t *testing.T) {
	t.Parallel()

	for _, parameter := range []string{"path", "bash-env", "project-signal-run-uid"} {
		_, err := scriptEnvironment(map[string]string{parameter: "value"})
		if err == nil || !strings.Contains(err.Error(), "reserved environment variable") {
			t.Fatalf("scriptEnvironment(%q) error = %v", parameter, err)
		}
	}
}

func TestWorkflowParametersRejectMalformedEvidence(t *testing.T) {
	t.Parallel()

	tests := map[string][]any{
		"duplicate": {
			map[string]any{"name": "verdict", "value": "Pass"},
			map[string]any{"name": "verdict", "value": "Fail"},
		},
		"unexpected": {
			map[string]any{"name": "unbounded", "value": "value"},
		},
		"oversized": {
			map[string]any{"name": "measurements", "value": strings.Repeat("x", 16*1024+1)},
		},
		"oversized cleanup failure": {
			map[string]any{"name": "cleanup-failure", "value": strings.Repeat("x", 65)},
		},
		"wrong type": {
			map[string]any{"name": "verdict", "value": true},
		},
	}

	for name, parameters := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			workflow := &unstructured.Unstructured{Object: map[string]any{
				"status": map[string]any{
					"outputs": map[string]any{"parameters": parameters},
				},
			}}

			if _, err := workflowParameters(workflow); err == nil {
				t.Fatal("workflowParameters succeeded")
			}
		})
	}
}

func TestBackendResourceReferencesRequireCompleteMultiNodeCoverage(t *testing.T) {
	t.Parallel()

	value := `[
		{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","namespace":"signal","name":"node-0","uid":"uid-0","participantIndexes":[0]},
		{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","namespace":"signal","name":"node-1","uid":"uid-1","participantIndexes":[1]}
	]`

	references, err := backendResourceReferences(value, 2)
	if err != nil {
		t.Fatalf("backendResourceReferences: %v", err)
	}

	if len(references) != 2 || references[1].ParticipantIndexes[0] != 1 {
		t.Fatalf("references = %#v", references)
	}

	for name, malformed := range map[string]string{
		"missing participant": `[{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","name":"node-0","uid":"uid-0","participantIndexes":[0]}]`,
		"out of range":        `[{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","name":"node-0","uid":"uid-0","participantIndexes":[0,2]}]`,
		"empty coverage":      `[{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","name":"node-0","uid":"uid-0"}]`,
		"missing uid":         `[{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","name":"node-0","participantIndexes":[0,1]}]`,
		"unknown field":       `[{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","name":"node-0","uid":"uid-0","participantIndexes":[0,1],"extra":true}]`,
		"trailing value":      `[{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","name":"node-0","uid":"uid-0","participantIndexes":[0,1]}] {}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := backendResourceReferences(malformed, 2); err == nil {
				t.Fatalf("backendResourceReferences(%s) error = nil", malformed)
			}
		})
	}
}

func TestMultiNodePassWithoutBackendIdentityBecomesInconclusive(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-multi")
	run.Spec.Participants = []signalv1.Participant{
		participantSnapshot(readyNode("gpu-0", "H200"), 0),
		participantSnapshot(readyNode("gpu-1", "H200"), 1),
	}
	run.Status = signalv1.CertificationRunStatus{
		Phase:     signalv1.RunPhaseRunning,
		Workflow:  signalv1.WorkflowStatus{Name: "run-multi-a1", Attempt: 1},
		StartedAt: ptrTime(metav1.NewTime(time.Unix(100, 0))),
	}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	workflow := workflowObject(run, "Succeeded", "Pass", "Healthy")
	store := &memoryEvidenceStore{}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, snapshot, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: store, Now: func() time.Time { return time.Unix(200, 0) }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}

	if got.Status.Phase != signalv1.RunPhaseInconclusive ||
		got.Status.Verdict != "Inconclusive" ||
		!strings.Contains(got.Status.Reason, "did not identify backend resources") {
		t.Fatalf("status = %#v", got.Status)
	}
}

func TestMultiNodePassRetainsCoveredBackendIdentities(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-multi-pass")
	run.Spec.Participants = []signalv1.Participant{
		participantSnapshot(readyNode("gpu-0", "H200"), 0),
		participantSnapshot(readyNode("gpu-1", "H200"), 1),
	}
	run.Status = signalv1.CertificationRunStatus{
		Phase:     signalv1.RunPhaseRunning,
		Workflow:  signalv1.WorkflowStatus{Name: "run-multi-pass-a1", Attempt: 1},
		StartedAt: ptrTime(metav1.NewTime(time.Unix(100, 0))),
	}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	workflow := workflowObject(run, "Succeeded", "Pass", "Healthy")

	outputs, _, err := unstructured.NestedSlice(workflow.Object, "status", "outputs", "parameters")
	if err != nil {
		t.Fatal(err)
	}

	outputs = append(outputs, map[string]any{
		"name": "backend-resources",
		"value": `[
			{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","namespace":"signal","name":"node-0","uid":"uid-0","participantIndexes":[0]},
			{"apiVersion":"kubeflow.org/v2beta1","kind":"MPIJob","namespace":"signal","name":"node-1","uid":"uid-1","participantIndexes":[1]}
		]`,
	})
	if err := unstructured.SetNestedSlice(workflow.Object, outputs, "status", "outputs", "parameters"); err != nil {
		t.Fatal(err)
	}

	store := &memoryEvidenceStore{}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, snapshot, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: store, Now: func() time.Time { return time.Unix(200, 0) }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}

	if got.Status.Phase != signalv1.RunPhaseSucceeded ||
		len(got.Status.Workflow.BackendResources) != 2 ||
		got.Status.Workflow.BackendResources[1].ParticipantIndexes[0] != 1 {
		t.Fatalf("status = %#v", got.Status)
	}
}

func TestStartAttemptRecoversExistingOwnedWorkflow(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-recovered")

	workflow, err := buildWorkflow(run, 1)
	if err != nil {
		t.Fatal(err)
	}

	workflow.SetUID("workflow-uid")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.startAttempt(context.Background(), run, 1); err != nil {
		t.Fatalf("startAttempt: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Workflow.Name != workflow.GetName() ||
		got.Status.Workflow.UID != string(workflow.GetUID()) ||
		got.Status.Workflow.Attempt != 1 {
		t.Fatalf("workflow status = %#v", got.Status.Workflow)
	}
}

func TestWorkflowParameterFallsBackToEntrypointNodeOutputs(t *testing.T) {
	t.Parallel()

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"nodes": map[string]any{
				"root": map[string]any{
					"outputs": map[string]any{
						"parameters": []any{map[string]any{"name": "verdict", "value": "Pass"}},
					},
				},
			},
		},
	}}

	if got := workflowParameter(workflow, "verdict"); got != "Pass" {
		t.Fatalf("verdict = %q", got)
	}
}

func TestValidateStagePlanRejectsInvalidAndDuplicateStages(t *testing.T) {
	t.Parallel()

	if err := validateStagePlan([]signalv1.CertificationStage{{DisplayName: "Missing ID"}}); err == nil {
		t.Fatal("expected empty stage ID to fail validation")
	}

	if err := validateStagePlan([]signalv1.CertificationStage{
		{ID: "workload", DisplayName: "First"},
		{ID: "workload", DisplayName: "Duplicate"},
	}); err == nil {
		t.Fatal("expected duplicate stage IDs to fail validation")
	}
}

func TestTerminalVerdictPersistsEvidenceAndCleanupReleasesNode(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-pass")
	run.Spec.RuntimeStage = &signalv1.RuntimeStage{Name: "collective-bandwidth"}
	run.Spec.Backend.Queue = "certification"
	run.Spec.WorkloadPolicy.KueueTopology = &signalv1.KueueTopologyPolicy{
		RequiredLevel: corev1.LabelHostname,
	}
	run.Status = signalv1.CertificationRunStatus{
		Phase:     signalv1.RunPhaseRunning,
		Workflow:  signalv1.WorkflowStatus{Name: "run-pass-a1", Attempt: 1},
		StartedAt: ptrTime(metav1.NewTime(time.Unix(100, 0))),
	}
	node := readyNode("gpu-0", "A100")
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	workflow := workflowObject(run, "Succeeded", "Pass", "Healthy")
	store := &memoryEvidenceStore{}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, snapshot, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: store, Now: func() time.Time { return time.Unix(200, 0) }}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("terminal Reconcile: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("cleanup Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Phase != signalv1.RunPhaseSucceeded ||
		got.Status.Verdict != "Pass" ||
		got.Status.Workflow.Phase != "Succeeded" {
		t.Fatalf("status = %#v", got.Status)
	}

	if got.Status.Workflow.Outputs["measurements"] != "checked=14" {
		t.Fatalf("retained workflow outputs = %#v", got.Status.Workflow.Outputs)
	}

	if got.Status.Evidence.URI == "" || len(store.data) == 0 {
		t.Fatal("evidence was not persisted")
	}

	var evidence struct {
		Workflow              signalv1.WorkflowStatus `json:"workflow"`
		WorkloadPolicy        signalv1.WorkloadPolicy `json:"workloadPolicy"`
		RuntimeStage          *signalv1.RuntimeStage  `json:"runtimeStage"`
		BackendActiveDeadline metav1.Duration         `json:"backendActiveDeadline"`
	}
	if err := json.Unmarshal(store.data, &evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}

	if evidence.Workflow.Phase != "Succeeded" ||
		evidence.Workflow.Outputs["measurements"] != "checked=14" {
		t.Fatalf("evidence workflow = %#v", evidence.Workflow)
	}

	if !reflect.DeepEqual(evidence.WorkloadPolicy, run.Spec.WorkloadPolicy) {
		t.Fatalf("evidence workload policy = %#v, want %#v", evidence.WorkloadPolicy, run.Spec.WorkloadPolicy)
	}

	if !reflect.DeepEqual(evidence.RuntimeStage, run.Spec.RuntimeStage) ||
		evidence.BackendActiveDeadline != run.Spec.BackendActiveDeadline {
		t.Fatalf("evidence runtime contract = stage %#v deadline %s",
			evidence.RuntimeStage, evidence.BackendActiveDeadline.Duration)
	}

	if !got.Status.Cleanup.ReservationsReleased || !got.Status.Cleanup.SnapshotDeleted {
		t.Fatalf("cleanup status = %#v", got.Status.Cleanup)
	}

	if containsString(got.Finalizers, RunFinalizer) {
		t.Fatal("cleanup finalizer remains")
	}

	var released corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &released); err != nil {
		t.Fatalf("get released node: %v", err)
	}

	if released.Annotations[ReservationLabel] != "" || released.Labels[RequestLabel] != "" {
		t.Fatalf("node was not released: labels=%v annotations=%v", released.Labels, released.Annotations)
	}

	if released.Labels[StateLabel] != "passed" ||
		released.Labels[EligibilityLabel] != "production" ||
		released.Annotations[LastSuccessfulRunAnnotation] != run.Name {
		t.Fatalf("terminal projection was not retained: labels=%v annotations=%v", released.Labels, released.Annotations)
	}
}

func TestTerminalFinalizationRetriesWithIdenticalEvidence(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-retry-finalization")
	run.Status = signalv1.CertificationRunStatus{
		Phase:     signalv1.RunPhaseRunning,
		Workflow:  signalv1.WorkflowStatus{Name: "run-retry-finalization-a1", Attempt: 1},
		StartedAt: ptrTime(metav1.NewTime(time.Unix(100, 0))),
	}
	node := readyNode("gpu-0", "A100")
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	workflow := workflowObject(run, "Succeeded", "Pass", "Healthy")
	statusPatches := 0
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, snapshot, workflow).
		Build()
	kube := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			if subResource == "status" {
				statusPatches++
				if statusPatches == 2 {
					return errors.New("status patch interrupted after evidence persistence")
				}
			}

			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	store := &immutableMemoryEvidenceStore{}
	r := &RunReconciler{
		Client:   kube,
		Evidence: store,
		Now:      func() time.Time { return time.Unix(200+int64(statusPatches), 0) },
	}
	key := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}

	if _, err := r.Reconcile(context.Background(), key); err == nil {
		t.Fatal("first Reconcile error = nil, want interrupted status patch")
	}

	var interrupted signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &interrupted); err != nil {
		t.Fatalf("get interrupted run: %v", err)
	}

	beforeCancel := interrupted.DeepCopy()
	interrupted.Spec.Cancel = true

	if err := kube.Patch(context.Background(), &interrupted, client.MergeFrom(beforeCancel)); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), key); err != nil {
		t.Fatalf("retry Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Phase != signalv1.RunPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}

	if store.puts != 2 {
		t.Fatalf("evidence puts = %d, want 2 identical attempts", store.puts)
	}
}

func TestWorkflowReplacementIsRejected(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-workflow-replaced")
	run.Status = signalv1.CertificationRunStatus{
		Phase: signalv1.RunPhaseRunning,
		Workflow: signalv1.WorkflowStatus{
			Name:    "run-workflow-replaced-a1",
			UID:     "original-workflow-uid",
			Attempt: 1,
		},
	}
	replacement := workflowObject(run, "Succeeded", "Pass", "forged")
	replacement.SetUID("replacement-workflow-uid")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, replacement).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)},
	); err == nil || !strings.Contains(err.Error(), "refusing concurrent retry") {
		t.Fatalf("Reconcile error = %v, want identity rejection", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Workflow.Attempt != 1 ||
		got.Status.Workflow.Name != "run-workflow-replaced-a1" {
		t.Fatalf("replacement workflow triggered a retry: %#v", got.Status.Workflow)
	}

	var workflows unstructured.UnstructuredList
	workflows.SetGroupVersionKind(workflowGVK.GroupVersion().WithKind("WorkflowList"))

	if err := kube.List(context.Background(), &workflows, client.InNamespace(run.Namespace)); err != nil {
		t.Fatalf("list Workflows: %v", err)
	}

	if len(workflows.Items) != 1 {
		t.Fatalf("workflows = %d, want only mismatched original name", len(workflows.Items))
	}
}

func TestCancellationRefusesToDeleteReplacementWorkflow(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-cancel-replacement")
	run.Spec.Cancel = true
	run.Status = signalv1.CertificationRunStatus{
		Phase: signalv1.RunPhaseRunning,
		Workflow: signalv1.WorkflowStatus{
			Name:    "run-cancel-replacement-a1",
			UID:     "original-workflow-uid",
			Attempt: 1,
		},
	}
	replacement := workflowObject(run, "Running", "", "")
	replacement.SetUID("replacement-workflow-uid")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, replacement).
		Build()
	r := &RunReconciler{Client: kube}

	if _, err := r.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)},
	); err == nil || !strings.Contains(err.Error(), "refusing to delete Workflow") {
		t.Fatalf("Reconcile error = %v, want identity rejection", err)
	}

	var remaining unstructured.Unstructured
	remaining.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(replacement), &remaining); err != nil {
		t.Fatalf("replacement Workflow was deleted: %v", err)
	}
}

func TestWorkflowFailureRetriesWithoutSwitchingPinnedInputs(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-retry")
	run.Spec.MaxAttempts = 2
	run.Status = signalv1.CertificationRunStatus{
		Phase:    signalv1.RunPhaseRunning,
		Workflow: signalv1.WorkflowStatus{Name: "run-retry-a1", Attempt: 1},
	}
	workflow := workflowObject(run, "Failed", "", "")
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, workflow).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if got.Status.Workflow.Attempt != 2 || got.Status.Workflow.Name != "run-retry-a2" {
		t.Fatalf("workflow status = %#v", got.Status.Workflow)
	}

	var retry unstructured.Unstructured
	retry.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: run.Namespace, Name: "run-retry-a2"}, &retry); err != nil {
		t.Fatalf("get retry workflow: %v", err)
	}

	if value := workflowArgument(&retry, "script-digest"); value != run.Spec.Script.Digest {
		t.Fatalf("retry script digest = %q", value)
	}

	if value := workflowArgument(&retry, "backend-kind"); value != run.Spec.Backend.Kind {
		t.Fatalf("retry backend = %q", value)
	}
}

func TestCanceledRunCleansUpDespiteCancelFlag(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := testRun("run-canceled")
	run.Spec.Cancel = true
	run.Status.Phase = signalv1.RunPhaseCancelled
	node := readyNode("gpu-0", "A100")
	node.Labels[RequestLabel] = run.Spec.Profile.Name
	node.Annotations = map[string]string{ReservationLabel: run.Name}
	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node, snapshot).
		Build()
	r := &RunReconciler{Client: kube, Evidence: &memoryEvidenceStore{}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}

	if containsString(got.Finalizers, RunFinalizer) || !got.Status.Cleanup.ReservationsReleased {
		t.Fatalf("run was not cleaned up: %#v", got)
	}
}

type memoryEvidenceStore struct {
	data []byte
}

func (s *memoryEvidenceStore) Put(_ context.Context, path string, data []byte) (string, string, error) {
	s.data = append([]byte(nil), data...)

	return "memory://" + path, digestString(string(data)), nil
}

type immutableMemoryEvidenceStore struct {
	data []byte
	puts int
}

func (s *immutableMemoryEvidenceStore) Put(_ context.Context, path string, data []byte) (string, string, error) {
	s.puts++
	if s.data != nil && string(s.data) != string(data) {
		return "", "", errors.New("evidence changed across retry")
	}

	s.data = append([]byte(nil), data...)

	return "memory://" + path, digestString(string(data)), nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	scheme.AddKnownTypeWithName(workflowGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(workflowGVK.GroupVersion().WithKind("WorkflowList"), &unstructured.UnstructuredList{})

	return scheme
}

func readyNode(name, sku string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
			Labels: map[string]string{
				"node.kubernetes.io/instance-type": sku,
			},
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8")},
			Allocatable: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func testProfile(name, sku string) *signalv1.CertificationProfile {
	return &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Generation: 3},
		Spec: signalv1.CertificationProfileSpec{
			Participants: signalv1.ParticipantPolicy{
				Count: 1,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{
					"node.kubernetes.io/instance-type": sku,
				}},
				RequiredResources: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
			},
			Script:                signalv1.ScriptReference{Name: "scripts", Key: "payload.txt"},
			Workflow:              signalv1.WorkflowReference{Name: "gpu-cert"},
			Backend:               signalv1.ExecutionBackend{APIVersion: "kubeflow.org/v2beta1", Kind: "MPIJob"},
			RunnerImage:           "runner@sha256:" + strings.Repeat("a", 64),
			Purpose:               signalv1.PurposeQuickCertification,
			StagePlan:             []signalv1.CertificationStage{{ID: "workload", DisplayName: "Workload", Category: "compute", Required: true}},
			EvidencePath:          "signal/" + sku,
			Deadline:              metav1.Duration{Duration: 10 * time.Minute},
			BackendActiveDeadline: metav1.Duration{Duration: 9 * time.Minute},
			MaxAttempts:           2,
		},
	}
}

func testRun(name string) *signalv1.CertificationRun {
	return &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "signal",
			UID:        types.UID(name + "-uid"),
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile:               signalv1.PinnedProfile{Name: "gpu-profile", UID: "profile-uid", Generation: 3},
			Participants:          []signalv1.Participant{{NodeName: "gpu-0", NodeUID: "gpu-0-uid"}},
			Script:                signalv1.ScriptSnapshot{ConfigMapName: name + "-scripts", Key: "payload.txt", Digest: digestString("test payload\n")},
			Workflow:              signalv1.WorkflowReference{Name: "gpu-cert"},
			Backend:               signalv1.ExecutionBackend{APIVersion: "kubeflow.org/v2beta1", Kind: "MPIJob"},
			RunnerImage:           "runner@sha256:" + strings.Repeat("a", 64),
			EvidencePath:          "signal/" + name + ".json",
			Deadline:              metav1.Duration{Duration: 10 * time.Minute},
			BackendActiveDeadline: metav1.Duration{Duration: 9 * time.Minute},
			MaxAttempts:           2,
			RequestNodeName:       "gpu-0",
		},
	}
}

func workflowObject(run *signalv1.CertificationRun, phase, verdict, reason string) *unstructured.Unstructured {
	const workflowUID = "workflow-uid"

	run.Status.Workflow.UID = workflowUID
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workflowGVK.GroupVersion().String(),
		"kind":       workflowGVK.Kind,
		"metadata": map[string]any{
			"name":      run.Status.Workflow.Name,
			"namespace": run.Namespace,
			"uid":       workflowUID,
			"ownerReferences": []any{
				map[string]any{
					"apiVersion":         signalv1.GroupVersion.String(),
					"kind":               "CertificationRun",
					"name":               run.Name,
					"uid":                string(run.UID),
					"controller":         true,
					"blockOwnerDeletion": true,
				},
			},
		},
		"status": map[string]any{
			"phase":   phase,
			"message": "test workflow message",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": verdict},
				map[string]any{"name": "reason", "value": reason},
				map[string]any{"name": "measurements", "value": "checked=14"},
			}},
		},
	}}

	return workflow
}

func workflowArgument(workflow *unstructured.Unstructured, name string) string {
	parameters, _, _ := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		if parameter["name"] == name {
			return fmt.Sprint(parameter["value"])
		}
	}

	return ""
}

func ptrTime(value metav1.Time) *metav1.Time {
	return &value
}

func quantity(value string) *resource.Quantity {
	result := resource.MustParse(value)

	return &result
}
