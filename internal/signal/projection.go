// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	StateLabel       = "signal.unbounded-cloud.io/state"
	EligibilityLabel = "signal.unbounded-cloud.io/eligibility"
	ProfileLabel     = "signal.unbounded-cloud.io/profile"

	ProfileNameAnnotation       = "signal.unbounded-cloud.io/profile-name"
	ProfileGenerationAnnotation = "signal.unbounded-cloud.io/profile-generation"
	LastRunAnnotation           = "signal.unbounded-cloud.io/last-run"
	LastRunAtAnnotation         = "signal.unbounded-cloud.io/last-run-at"
	LastSuccessfulRunAnnotation = "signal.unbounded-cloud.io/last-successful-run"
	LastSuccessfulAtAnnotation  = "signal.unbounded-cloud.io/last-successful-at"
	EvidenceDigestAnnotation    = "signal.unbounded-cloud.io/evidence-digest"
	ScriptDigestAnnotation      = "signal.unbounded-cloud.io/script-digest"

	maxProjectionAnnotationValue = 1024
)

func projectPending(
	ctx context.Context,
	kube client.Client,
	run *signalv1.CertificationRun,
) error {
	return projectParticipants(ctx, kube, run.Spec.Participants, true, func(node *corev1.Node) error {
		if node.Annotations[ReservationLabel] != run.Name {
			return fmt.Errorf("reservation is not owned by Run %s", run.Name)
		}

		setProfileProjection(node, run.Spec.Profile)
		node.Labels[StateLabel] = "pending"
		node.Labels[EligibilityLabel] = "blocked"

		return nil
	})
}

func projectTesting(ctx context.Context, kube client.Client, run *signalv1.CertificationRun) error {
	return projectParticipants(ctx, kube, run.Spec.Participants, false, func(node *corev1.Node) error {
		if node.Annotations[ReservationLabel] != run.Name {
			return nil
		}

		setProfileProjection(node, run.Spec.Profile)
		node.Labels[StateLabel] = "testing"
		node.Labels[EligibilityLabel] = "blocked"

		return nil
	})
}

func projectTerminal(ctx context.Context, kube client.Client, run *signalv1.CertificationRun) error {
	if run.Status.FinishedAt == nil {
		return nil
	}

	finishedAt := run.Status.FinishedAt.UTC().Format(time.RFC3339)

	return projectParticipants(ctx, kube, run.Spec.Participants, false, func(node *corev1.Node) error {
		if node.Annotations[ReservationLabel] != run.Name {
			return nil
		}

		if projectionTimeAfter(node.Annotations[LastRunAtAnnotation], run.Status.FinishedAt.Time) {
			return nil
		}

		setProfileProjection(node, run.Spec.Profile)
		node.Annotations[LastRunAnnotation] = boundedAnnotationValue(run.Name)
		node.Annotations[LastRunAtAnnotation] = finishedAt
		node.Annotations[EvidenceDigestAnnotation] = boundedAnnotationValue(run.Status.Evidence.Digest)
		node.Annotations[ScriptDigestAnnotation] = boundedAnnotationValue(runScriptDigest(run))

		switch run.Status.Phase {
		case signalv1.RunPhaseSucceeded:
			node.Labels[StateLabel] = "passed"
			node.Labels[EligibilityLabel] = "production"

			if !projectionTimeAfter(node.Annotations[LastSuccessfulAtAnnotation], run.Status.FinishedAt.Time) {
				node.Annotations[LastSuccessfulRunAnnotation] = boundedAnnotationValue(run.Name)
				node.Annotations[LastSuccessfulAtAnnotation] = finishedAt
			}
		case signalv1.RunPhaseFailed:
			node.Labels[StateLabel] = "failed"
			node.Labels[EligibilityLabel] = "blocked"
		default:
			node.Labels[StateLabel] = "inconclusive"
			node.Labels[EligibilityLabel] = "blocked"
		}

		return nil
	})
}

func runScriptDigest(run *signalv1.CertificationRun) string {
	hash := sha256.New()
	for index := range run.Spec.Checks {
		_, _ = hash.Write([]byte(run.Spec.Checks[index].ID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(run.Spec.Checks[index].Script.Digest))
		_, _ = hash.Write([]byte{0})
	}

	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func projectParticipants(
	ctx context.Context,
	kube client.Client,
	participants []signalv1.Participant,
	strict bool,
	mutate func(*corev1.Node) error,
) error {
	for _, participant := range participants {
		if participant.NodeName == "" {
			continue
		}

		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var node corev1.Node
			if err := kube.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
				if !strict {
					return client.IgnoreNotFound(err)
				}

				return err
			}

			if participant.NodeUID != "" && node.UID != participant.NodeUID {
				if strict {
					return fmt.Errorf(
						"node UID changed from %s to %s",
						participant.NodeUID,
						node.UID,
					)
				}

				return nil
			}

			before := node.DeepCopy()
			ensureProjectionMaps(&node)

			if err := mutate(&node); err != nil {
				return err
			}

			return kube.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}); err != nil {
			return fmt.Errorf("project Node %s status: %w", participant.NodeName, err)
		}
	}

	return nil
}

func setProfileProjection(node *corev1.Node, profile signalv1.PinnedProfile) {
	ensureProjectionMaps(node)
	node.Labels[ProfileLabel] = projectionLabelValue(profile.Name)
	node.Annotations[ProfileNameAnnotation] = boundedAnnotationValue(profile.Name)
	node.Annotations[ProfileGenerationAnnotation] = strconv.FormatInt(profile.Generation, 10)
}

func ensureProjectionMaps(node *corev1.Node) {
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
}

func projectionLabelValue(value string) string {
	if len(validation.IsValidLabelValue(value)) == 0 {
		return value
	}

	sum := sha256.Sum256([]byte(value))

	return "sha256-" + hex.EncodeToString(sum[:8])
}

func boundedAnnotationValue(value string) string {
	if len(value) <= maxProjectionAnnotationValue {
		return value
	}

	sum := sha256.Sum256([]byte(value))

	return "sha256:" + hex.EncodeToString(sum[:])
}

func projectionTimeAfter(value string, candidate time.Time) bool {
	existing, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return false
	}

	return existing.After(candidate)
}
