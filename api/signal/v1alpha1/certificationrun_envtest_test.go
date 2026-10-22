// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtestCertificationRunOptionalImmutability(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API validation tests")
	}

	environment := &envtest.Environment{
		BinaryAssetsDirectory:    assets,
		CRDDirectoryPaths:        []string{"../../../deploy/signal/crd"},
		ErrorIfCRDPathMissing:    true,
		AttachControlPlaneOutput: false,
	}
	restConfig, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, AddToScheme(scheme))

	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, apiClient.Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "signal"},
	}))

	createRun := func(name string, stage *RuntimeStage) *CertificationRun {
		t.Helper()

		run := validEnvtestCertificationRun(name)
		run.Spec.Checks[0].RuntimeStage = stage
		run.Spec.Checks[0].Script.RequiresRuntimeStage = stage != nil
		require.NoError(t, apiClient.Create(t.Context(), run))

		return run
	}

	t.Run("omitted runtime stage allows terminal status update", func(t *testing.T) {
		run := createRun("runtime-stage-absent", nil)
		run.Status.Phase = RunPhaseInconclusive
		run.Status.ExecutionState = "Completed"
		run.Status.Verdict = "Inconclusive"
		run.Status.Reason = "DiagnosticUnavailable"
		require.NoError(t, apiClient.Status().Update(t.Context(), run))
	})

	t.Run("present equal remains valid", func(t *testing.T) {
		run := createRun("runtime-stage-equal", &RuntimeStage{Name: "collective-bandwidth"})
		run.Spec.Cancel = true
		require.NoError(t, apiClient.Update(t.Context(), run))
	})

	for name, test := range map[string]struct {
		initial *RuntimeStage
		updated *RuntimeStage
	}{
		"absent to present": {
			updated: &RuntimeStage{Name: "collective-bandwidth"},
		},
		"present to absent": {
			initial: &RuntimeStage{Name: "collective-bandwidth"},
		},
		"present unequal": {
			initial: &RuntimeStage{Name: "collective-bandwidth"},
			updated: &RuntimeStage{Name: "memory-bandwidth"},
		},
	} {
		t.Run(name+" is rejected", func(t *testing.T) {
			run := createRun("runtime-stage-"+strings.ReplaceAll(name, " ", "-"), test.initial)
			run.Spec.Checks[0].RuntimeStage = test.updated
			run.Spec.Checks[0].Script.RequiresRuntimeStage = test.updated != nil

			err := apiClient.Update(t.Context(), run)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "checks are immutable")
		})
	}

	for name, mutate := range map[string]func(*CertificationRun){
		"fourteen checks": func(run *CertificationRun) {
			run.Spec.Checks = run.Spec.Checks[:14]
		},
		"duplicate check id": func(run *CertificationRun) {
			run.Spec.Checks[1].ID = run.Spec.Checks[0].ID
		},
		"wrong ordered check id": func(run *CertificationRun) {
			run.Spec.Checks[4].ID = "R15"
			run.Spec.Checks[14].ID = "R05"
		},
		"noncontiguous order": func(run *CertificationRun) {
			run.Spec.Checks[7].Order = 8
		},
		"retry enabled": func(run *CertificationRun) {
			run.Spec.Checks[2].MaxAttempts = 2
		},
		"insufficient cleanup reserve": func(run *CertificationRun) {
			run.Spec.Checks[3].BackendActiveDeadline = metav1.Duration{Duration: 61 * time.Second}
		},
	} {
		t.Run(name+" is rejected", func(t *testing.T) {
			run := validEnvtestCertificationRun("invalid-" + strings.ReplaceAll(name, " ", "-"))
			mutate(run)

			err := apiClient.Create(t.Context(), run)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
		})
	}
}

func validEnvtestCertificationRun(name string) *CertificationRun {
	return &CertificationRun{
		TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "CertificationRun"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "signal",
		},
		Spec: CertificationRunSpec{
			Profile: PinnedProfile{
				Name:       "profile",
				UID:        types.UID("profile-uid"),
				Generation: 1,
			},
			Participants: []Participant{{
				Index:    0,
				NodeName: "node-a",
				NodeUID:  types.UID("node-a-uid"),
			}},
			Checks:          validEnvtestChecks(),
			ExecutionPolicy: SuiteExecutionPolicy{OnFailure: SuiteFailurePolicyStop},
			Workflow:        WorkflowReference{Name: "workflow"},
			EvidencePath:    "signal/" + name + ".json",
			Deadline:        metav1.Duration{Duration: 2 * time.Minute},
			RequestNodeName: "node-a",
		},
	}
}

func validEnvtestChecks() []CertificationCheckSnapshot {
	checks := make([]CertificationCheckSnapshot, 15)
	for index := range checks {
		checks[index] = CertificationCheckSnapshot{
			ID:    CheckID(fmt.Sprintf("R%02d", index+1)),
			Order: int32(index),
			Script: ScriptSnapshot{
				ConfigMapName: "scripts",
				Key:           "check.sh",
				Digest:        "sha256:" + strings.Repeat("a", 64),
			},
			RunnerImage:           "example.invalid/runner@sha256:" + strings.Repeat("b", 64),
			Backend:               ExecutionBackend{APIVersion: "v1", Kind: "Pod"},
			BackendActiveDeadline: metav1.Duration{Duration: time.Minute},
			ResultBounds: ResultBounds{
				ReasonBytes:           4096,
				MeasurementsBytes:     16384,
				BackendResourcesBytes: 16384,
				BackendEvidenceBytes:  65536,
			},
			MaxAttempts: 1,
		}
	}

	return checks
}
