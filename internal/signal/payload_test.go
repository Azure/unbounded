// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestExternalMPIPayloadKeepsStrictPlansAndPinnedParameters(t *testing.T) {
	t.Parallel()

	for _, workflow := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow} {
		for _, grouped := range []bool{false, true} {
			name := workflow
			if grouped {
				name += "/rack"
			}

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				run := builtinTestRun(t, workflow)
				run.Spec.Parameters[MPIPayloadModeParameter] = MPIPayloadModeScript
				run.Spec.Script.ConfigMapName = "release-script-snapshot"
				run.Spec.Script.Key = "selected-mpi.sh"

				if grouped {
					run.Spec.Workflow.Name = RackCertificationWorkflow
					run.Spec.Parameters[NodeWorkflowTemplateParameter] = workflow
					run.Spec.Participants = append(run.Spec.Participants, signalv1.Participant{
						NodeName: "gpu-1", NodeUID: "gpu-1-uid",
					})
					run.Spec.ParticipantPlan = append(run.Spec.ParticipantPlan, signalv1.ParticipantSnapshot{
						NodeName: "gpu-1", NodeUID: "gpu-1-uid", ObservedGPUCapacity: "8", ObservedGPUAllocatable: "8",
					})
				}

				plan, strict, err := makeTestPlan(&run.Spec)
				require.NoError(t, err)
				require.True(t, strict)
				require.Len(t, plan, len(workflowTests(workflow))*len(run.Spec.Participants))
				run.Spec.TestPlan = plan

				for _, invocation := range plan {
					require.Equal(t, MPIPayloadModeScript, invocation.Parameters[MPIPayloadModeParameter])
				}

				created, err := buildWorkflow(run, 2)
				require.NoError(t, err)
				require.Equal(t, MPIPayloadModeScript, workflowArgument(created, MPIPayloadModeParameter))
				require.Equal(t, run.Spec.Script.ConfigMapName, workflowArgument(created, "script-configmap"))
				require.Equal(t, run.Spec.Script.Key, workflowArgument(created, "script-key"))
				require.Equal(t, run.Spec.Script.Digest, workflowArgument(created, "script-digest"))
				require.Equal(t, "2", workflowArgument(created, "attempt"))

				var encodedPlan []signalv1.TestInvocation
				require.NoError(t, json.Unmarshal([]byte(workflowArgument(created, TestPlanParameter)), &encodedPlan))
				require.Equal(t, plan, encodedPlan)

				run.Spec.Parameters[MPIPayloadModeParameter] = MPIPayloadModeLegacy
				_, strict, err = effectiveTestPlan(run)
				require.True(t, strict)
				require.ErrorContains(t, err, "immutable test plan")
			})
		}
	}
}

func TestMPIPayloadModesPreserveDefaultsAndRejectUnsupportedValues(t *testing.T) {
	t.Parallel()

	for _, workflow := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		for _, mode := range []string{"", MPIPayloadModeLegacy, MPIPayloadModeScript, "SCRIPT", "true", "unknown"} {
			t.Run(workflow+"/"+mode, func(t *testing.T) {
				t.Parallel()

				_, strict, err := nodeWorkflow(workflow, map[string]string{MPIPayloadModeParameter: mode})
				require.True(t, strict)

				if mode == "" || mode == MPIPayloadModeLegacy || (mode == MPIPayloadModeScript && workflow != BurnCanaryWorkflow) {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "UnsupportedMPIPayloadMode")
				}
			})
		}
	}
}

func TestExternalMPIPayloadParameterDoesNotSynthesizeALegacyPlan(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	run.Spec.TestPlan = nil
	run.Spec.Parameters[MPIPayloadModeParameter] = MPIPayloadModeScript
	run.Spec.Script.Key = "original-opaque-key"
	original := run.Spec.DeepCopy()

	workflow, err := buildWorkflow(run, 2)
	require.NoError(t, err)
	require.Equal(t, "[]", workflowArgument(workflow, TestPlanParameter))
	require.Equal(t, run.Spec.Script.Key, workflowArgument(workflow, "script-key"))
	require.Equal(t, run.Spec.Script.Digest, workflowArgument(workflow, "script-digest"))
	require.Equal(t, *original, run.Spec)
}

func TestUnsupportedMPIPayloadIsRejectedBeforeSnapshotAndReservation(t *testing.T) {
	t.Parallel()

	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "external"
	profile := testProfile("external", "example-gpu")
	profile.Spec.Workflow.Name = FullNodeCertificationWorkflow
	profile.Spec.Parameters = map[string]string{MPIPayloadModeParameter: "unknown"}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, profile, source).Build()
	r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}})
	require.ErrorContains(t, err, "UnsupportedMPIPayloadMode")

	var runs signalv1.CertificationRunList
	require.NoError(t, kube.List(t.Context(), &runs))
	require.Empty(t, runs.Items)

	var snapshots corev1.ConfigMapList
	require.NoError(t, kube.List(t.Context(), &snapshots))
	require.Len(t, snapshots.Items, 1)
	require.Equal(t, source.Name, snapshots.Items[0].Name)

	var observed corev1.Node
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(node), &observed))
	require.Empty(t, observed.Annotations[ReservationLabel])
}

func TestReleaseConfigMapSnapshotsOnlySelectedDecodedText(t *testing.T) {
	t.Parallel()

	const content = "#!/usr/bin/env bash\nprintf 'reviewed payload\\n'\n\n"

	profile := testProfile("external", "example-gpu")
	profile.Spec.Script.Name = "certification-release"
	profile.Spec.Script.Key = "selected-mpi.sh"
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      profile.Spec.Script.Name,
			Namespace: "payload-releases",
			Labels:    map[string]string{"source-revision": "release"},
			Annotations: map[string]string{
				"source-commit": "not-a-runtime-provenance-field",
			},
		},
		Data:       map[string]string{profile.Spec.Script.Key: content, "helper.sh": "not snapshotted"},
		BinaryData: map[string][]byte{"binary-helper": []byte("not snapshotted")},
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(source).Build()
	r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: source.Namespace}

	snapshot, digest, err := r.createSnapshot(t.Context(), "external-run", profile)
	require.NoError(t, err)
	require.Equal(t, digestString(content), digest)
	require.Equal(t, "signal", snapshot.Namespace)
	require.Equal(t, "external-run-scripts", snapshot.Name)
	require.NotNil(t, snapshot.Immutable)
	require.True(t, *snapshot.Immutable)
	require.Equal(t, map[string]string{profile.Spec.Script.Key: content, "sha256": digest}, snapshot.Data)
	require.Equal(t, map[string]string{SnapshotRunLabel: "external-run"}, snapshot.Labels)
	require.Empty(t, snapshot.Annotations)
	require.Empty(t, snapshot.BinaryData)

	source.Data[profile.Spec.Script.Key] = "later release\n"
	require.NoError(t, kube.Update(t.Context(), source))

	var retained corev1.ConfigMap
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(snapshot), &retained))
	require.Equal(t, snapshot.Data, retained.Data)
}

func TestReleaseConfigMapRejectsMissingTextKeyWithoutFallback(t *testing.T) {
	t.Parallel()

	profile := testProfile("external", "example-gpu")
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: profile.Spec.Script.Name, Namespace: "payload-releases"},
		Data:       map[string]string{"helper.sh": "not the selected script"},
		BinaryData: map[string][]byte{profile.Spec.Script.Key: []byte("not text data")},
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(source).Build()
	r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: source.Namespace}

	snapshot, digest, err := r.createSnapshot(t.Context(), "external-run", profile)
	require.ErrorContains(t, err, "has no key")
	require.Nil(t, snapshot)
	require.Empty(t, digest)
}
