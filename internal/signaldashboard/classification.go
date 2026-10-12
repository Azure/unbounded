// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"strings"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func runPurpose(run *signalv1.CertificationRun) (string, string) {
	switch run.Spec.Purpose {
	case signalv1.PurposeFullCertification:
		return "Full certification", "Full"
	case signalv1.PurposeQuickCertification:
		return "Quick certification", "Quick"
	case signalv1.PurposePreflight:
		return "Preflight", "Preflight"
	case signalv1.PurposeTargetedVerification:
		return "Targeted verification", "Targeted"
	case signalv1.PurposeDiagnostic:
		return "Diagnostic", "Diagnostic"
	}

	identity := strings.ToLower(run.Spec.Profile.Name + " " + run.Spec.Script.Key)
	reason := strings.ToLower(run.Status.Reason)

	switch {
	case strings.Contains(identity, "diagnostic") || strings.Contains(reason, "diagnosticonly"):
		return "Diagnostic", "Diagnostic"
	case strings.Contains(identity, "verification"):
		return "Targeted verification", "Targeted"
	case strings.Contains(identity, "preflight"):
		return "Preflight", "Preflight"
	case strings.Contains(identity, "full"):
		return "Full certification", "Full"
	case strings.Contains(identity, "quick"):
		return "Quick certification", "Quick"
	default:
		return "Certification", "Certification"
	}
}

func diagnosticRun(run *signalv1.CertificationRun) bool {
	purpose, _ := runPurpose(run)
	return purpose == "Diagnostic" || purpose == "Preflight" || purpose == "Targeted verification"
}

func runClassification(run *signalv1.CertificationRun, runs []signalv1.CertificationRun) (string, bool) {
	if !run.Status.Phase.Terminal() {
		return "Current", false
	}
	if diagnosticRun(run) {
		return "Diagnostics", false
	}

	for i := range runs {
		newer := &runs[i]
		if !newer.CreationTimestamp.After(run.CreationTimestamp.Time) ||
			newer.Spec.Profile.Name != run.Spec.Profile.Name ||
			!sameParticipants(run, newer) ||
			!strings.EqualFold(newer.Status.Verdict, "Pass") ||
			newer.Status.Phase != signalv1.RunPhaseSucceeded {
			continue
		}

		return "Superseded", true
	}

	return "Historical", false
}

func sameParticipants(left, right *signalv1.CertificationRun) bool {
	if len(left.Spec.Participants) != len(right.Spec.Participants) {
		return false
	}

	names := map[string]struct{}{}
	for _, participant := range left.Spec.Participants {
		names[participant.NodeName] = struct{}{}
	}
	for _, participant := range right.Spec.Participants {
		if _, ok := names[participant.NodeName]; !ok {
			return false
		}
	}

	return true
}

func certificationRun(run *signalv1.CertificationRun) bool {
	return !diagnosticRun(run)
}
