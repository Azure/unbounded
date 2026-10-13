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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	StateLabel       = "signal.unbounded-cloud.io/state"
	EligibilityLabel = "signal.unbounded-cloud.io/eligibility"
	ProfileLabel     = "signal.unbounded-cloud.io/profile"

	ProfileNameAnnotation        = "signal.unbounded-cloud.io/profile-name"
	ProfileGenerationAnnotation  = "signal.unbounded-cloud.io/profile-generation"
	LastRunAnnotation            = "signal.unbounded-cloud.io/last-run"
	LastRunAtAnnotation          = "signal.unbounded-cloud.io/last-run-at"
	LastSuccessfulRunAnnotation  = "signal.unbounded-cloud.io/last-successful-run"
	LastSuccessfulAtAnnotation   = "signal.unbounded-cloud.io/last-successful-at"
	EvidenceDigestAnnotation     = "signal.unbounded-cloud.io/evidence-digest"
	ScriptDigestAnnotation       = "signal.unbounded-cloud.io/script-digest"
	NextDueAtAnnotation          = "signal.unbounded-cloud.io/next-due-at"
	RecurringReasonAnnotation    = "signal.unbounded-cloud.io/recurring-reason"
	RecurringSuspendedAnnotation = "signal.unbounded-cloud.io/recurring-suspended"

	maxProjectionAnnotationValue = 1024
)

func projectPending(
	ctx context.Context,
	kube client.Client,
	participants []signalv1.Participant,
	profile signalv1.PinnedProfile,
) error {
	return projectParticipants(ctx, kube, participants, func(node *corev1.Node) error {
		setProfileProjection(node, profile)
		node.Labels[StateLabel] = "pending"
		node.Labels[EligibilityLabel] = "blocked"

		return nil
	})
}

func projectTesting(ctx context.Context, kube client.Client, run *signalv1.CertificationRun) error {
	return projectParticipants(ctx, kube, run.Spec.Participants, func(node *corev1.Node) error {
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

	return projectParticipants(ctx, kube, run.Spec.Participants, func(node *corev1.Node) error {
		if projectionTimeAfter(node.Annotations[LastRunAtAnnotation], run.Status.FinishedAt.Time) {
			return nil
		}

		setProfileProjection(node, run.Spec.Profile)
		node.Annotations[LastRunAnnotation] = boundedAnnotationValue(run.Name)
		node.Annotations[LastRunAtAnnotation] = finishedAt
		node.Annotations[EvidenceDigestAnnotation] = boundedAnnotationValue(run.Status.Evidence.Digest)
		node.Annotations[ScriptDigestAnnotation] = boundedAnnotationValue(run.Spec.Script.Digest)

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

func projectRecurring(
	ctx context.Context,
	kube client.Client,
	profile *signalv1.CertificationProfile,
	status signalv1.RecurringNodeStatus,
) error {
	participant := signalv1.Participant{NodeName: status.NodeName}
	pinned := signalv1.PinnedProfile{Name: profile.Name, UID: profile.UID, Generation: profile.Generation}

	return projectParticipants(ctx, kube, []signalv1.Participant{participant}, func(node *corev1.Node) error {
		selectedProfile := node.Annotations[ProfileNameAnnotation]

		active := node.Labels[StateLabel] == "pending" || node.Labels[StateLabel] == "testing"
		if active && selectedProfile != profile.Name {
			return nil
		}

		if status.LastSuccessfulAt != nil &&
			!projectionTimeAfter(node.Annotations[LastSuccessfulAtAnnotation], status.LastSuccessfulAt.Time) {
			setProfileProjection(node, pinned)
		} else {
			return nil
		}

		setOrDeleteAnnotation(node, NextDueAtAnnotation, formatMetaTime(status.NextDueAt))
		setOrDeleteAnnotation(node, RecurringReasonAnnotation, status.Reason)
		node.Annotations[RecurringSuspendedAnnotation] = strconv.FormatBool(status.Suspended)

		if status.LastSuccessfulAt != nil &&
			!projectionTimeAfter(node.Annotations[LastSuccessfulAtAnnotation], status.LastSuccessfulAt.Time) {
			node.Annotations[LastSuccessfulRunAnnotation] = boundedAnnotationValue(status.LastSuccessfulRun)
			node.Annotations[LastSuccessfulAtAnnotation] = status.LastSuccessfulAt.UTC().Format(time.RFC3339)

			if node.Labels[StateLabel] == "" {
				node.Labels[StateLabel] = "passed"
				node.Labels[EligibilityLabel] = "production"
			}
		}

		return nil
	})
}

func projectParticipants(
	ctx context.Context,
	kube client.Client,
	participants []signalv1.Participant,
	mutate func(*corev1.Node) error,
) error {
	for _, participant := range participants {
		if participant.NodeName == "" {
			continue
		}

		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var node corev1.Node
			if err := kube.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
				return client.IgnoreNotFound(err)
			}

			if participant.NodeUID != "" && node.UID != participant.NodeUID {
				return nil
			}

			before := node.DeepCopy()
			ensureProjectionMaps(&node)

			if err := mutate(&node); err != nil {
				return err
			}

			return kube.Patch(ctx, &node, client.MergeFrom(before))
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

func setOrDeleteAnnotation(node *corev1.Node, key, value string) {
	if value == "" {
		delete(node.Annotations, key)

		return
	}

	node.Annotations[key] = boundedAnnotationValue(value)
}

func formatMetaTime(value *metav1.Time) string {
	if value == nil {
		return ""
	}

	return value.UTC().Format(time.RFC3339)
}
