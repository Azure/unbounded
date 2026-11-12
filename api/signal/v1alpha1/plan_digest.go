// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ExecutionPlanDigestSchema         = "signal.unbounded-cloud.io/execution-plan/v1"
	ResolvedExecutionPlanDigestSchema = "signal.unbounded-cloud.io/execution-plan/v2"
)

type executionPlanDigestInput struct {
	Schema             string                       `json:"schema"`
	ParticipantCount   int32                        `json:"participantCount"`
	ScriptBundleDigest string                       `json:"scriptBundleDigest"`
	Checks             []CertificationCheckSnapshot `json:"checks"`
	Provenance         ExecutionPlanProvenance      `json:"provenance"`
}

type resolvedExecutionPlanDigestInput struct {
	Schema           string                            `json:"schema"`
	ParticipantCount int32                             `json:"participantCount"`
	Environment      *CertificationEnvironmentBindings `json:"environment,omitempty"`
	Plan             ResolvedCertificationPlan         `json:"plan"`
	Provenance       ExecutionPlanProvenance           `json:"provenance"`
}

// DeriveExecutionPlanDigest returns the canonical digest of the resolved
// execution semantics. Object identity is intentionally excluded so equivalent
// resolved plans have the same digest.
func DeriveExecutionPlanDigest(
	participantCount int32,
	scriptBundleDigest string,
	checks []CertificationCheckSnapshot,
	provenance ExecutionPlanProvenance,
) (string, error) {
	if participantCount < 1 {
		return "", errors.New("participant count must be positive")
	}

	if !IsSHA256Digest(scriptBundleDigest) {
		return "", errors.New("script bundle digest is invalid")
	}

	if len(checks) == 0 {
		return "", errors.New("execution plan must contain at least one check")
	}

	if !IsDigestPinnedImage(provenance.AdapterImage) {
		return "", errors.New("adapter image must be digest-pinned")
	}

	if !IsSHA256Digest(provenance.WorkflowTemplateDigest) {
		return "", errors.New("workflow template digest is invalid")
	}

	for index := range checks {
		if !IsSHA256Digest(checks[index].Script.Digest) {
			return "", fmt.Errorf("check %d script digest is invalid", index)
		}

		if !IsDigestPinnedImage(checks[index].RunnerImage) {
			return "", fmt.Errorf("check %d runner image must be digest-pinned", index)
		}

		if err := ValidateCheckExecutionPolicy(
			checks[index].BackendActiveDeadline.Duration,
			checks[index].Execution,
		); err != nil {
			return "", fmt.Errorf("check %d execution policy is invalid: %w", index, err)
		}
	}

	canonicalChecks := make([]CertificationCheckSnapshot, len(checks))
	for index := range checks {
		checks[index].DeepCopyInto(&canonicalChecks[index])
		sort.Slice(canonicalChecks[index].WorkloadPolicy.HostPaths, func(left, right int) bool {
			return canonicalChecks[index].WorkloadPolicy.HostPaths[left].MountPath <
				canonicalChecks[index].WorkloadPolicy.HostPaths[right].MountPath
		})
		sort.Slice(canonicalChecks[index].WorkloadPolicy.Resources.Claims, func(left, right int) bool {
			return canonicalChecks[index].WorkloadPolicy.Resources.Claims[left].Name <
				canonicalChecks[index].WorkloadPolicy.Resources.Claims[right].Name
		})
	}

	canonicalProvenance := provenance.DeepCopy()
	sort.Strings(canonicalProvenance.ResolverImages)

	content, err := json.Marshal(executionPlanDigestInput{
		Schema:             ExecutionPlanDigestSchema,
		ParticipantCount:   participantCount,
		ScriptBundleDigest: scriptBundleDigest,
		Checks:             canonicalChecks,
		Provenance:         *canonicalProvenance,
	})
	if err != nil {
		return "", fmt.Errorf("encode execution plan: %w", err)
	}

	return sha256Digest(content), nil
}

// DeriveResolvedExecutionPlanDigest returns the canonical digest for a
// phase-based immutable Run plan.
func DeriveResolvedExecutionPlanDigest(
	participantCount int32,
	environment *CertificationEnvironmentBindings,
	plan ResolvedCertificationPlan,
	provenance ExecutionPlanProvenance,
) (string, error) {
	if participantCount < 1 {
		return "", errors.New("participant count must be positive")
	}

	if len(plan.Phases) == 0 {
		return "", errors.New("resolved execution plan must contain at least one phase")
	}

	if !IsDigestPinnedImage(provenance.ControllerImage) {
		return "", errors.New("controller image must be digest-pinned")
	}

	if !IsDigestPinnedImage(provenance.AdapterImage) {
		return "", errors.New("adapter image must be digest-pinned")
	}

	if !IsSHA256Digest(provenance.WorkflowTemplateDigest) {
		return "", errors.New("workflow template digest is invalid")
	}

	for index, image := range provenance.ResolverImages {
		if !IsDigestPinnedImage(image) {
			return "", fmt.Errorf("resolver image %d must be digest-pinned", index)
		}
	}

	if _, err := resolvedStageDependencyGraph(plan.Phases); err != nil {
		return "", fmt.Errorf("resolved stage dependencies are invalid: %w", err)
	}

	for phaseIndex := range plan.Phases {
		if len(plan.Phases[phaseIndex].Stages) == 0 {
			return "", fmt.Errorf("phase %d must contain at least one stage", phaseIndex)
		}

		for stageIndex := range plan.Phases[phaseIndex].Stages {
			stage := &plan.Phases[phaseIndex].Stages[stageIndex]
			if plan.Phases[phaseIndex].Role != CheckStageRoleValidation &&
				stage.Schedule.Mode != CheckExecutionModeRunOnce {
				return "", fmt.Errorf(
					"phase %d stage %d must use RunOnce outside Validation",
					phaseIndex,
					stageIndex,
				)
			}

			if err := ValidateStageSchedule(stage.Schedule); err != nil {
				return "", fmt.Errorf(
					"phase %d stage %d schedule is invalid: %w",
					phaseIndex,
					stageIndex,
					err,
				)
			}

			if err := validateResolvedStage(*stage, participantCount); err != nil {
				return "", fmt.Errorf(
					"phase %d stage %d is invalid: %w",
					phaseIndex,
					stageIndex,
					err,
				)
			}

			if !IsSHA256Digest(stage.Test.Identity.ContentDigest) {
				return "", fmt.Errorf(
					"phase %d stage %d test content digest is invalid",
					phaseIndex,
					stageIndex,
				)
			}

			for name, digest := range map[string]string{
				"parameter schema":      stage.Test.ParameterSchemaDigest,
				"metric schema":         stage.Test.MetricSchemaDigest,
				"security requirements": stage.Test.SecurityRequirementsDigest,
			} {
				if !IsSHA256Digest(digest) {
					return "", fmt.Errorf(
						"phase %d stage %d %s digest is invalid",
						phaseIndex,
						stageIndex,
						name,
					)
				}
			}

			if stage.QualificationEligible &&
				(stage.Test.Identity.Trust != CertificationTestTrustApproved || stage.DiagnosticOnly) {
				return "", fmt.Errorf(
					"phase %d stage %d is not eligible for qualification",
					phaseIndex,
					stageIndex,
				)
			}

			if stage.Test.Identity.Trust == CertificationTestTrustRejected ||
				stage.Test.Identity.Trust == CertificationTestTrustRevoked {
				return "", fmt.Errorf(
					"phase %d stage %d uses a rejected or revoked test",
					phaseIndex,
					stageIndex,
				)
			}

			if stage.Test.Identity.Trust != CertificationTestTrustApproved &&
				(!stage.DiagnosticOnly || stage.QualificationEligible) {
				return "", fmt.Errorf(
					"phase %d stage %d unapproved test is not diagnostic-only",
					phaseIndex,
					stageIndex,
				)
			}

			if err := validateResolvedDriver(stage.Test.Driver); err != nil {
				return "", fmt.Errorf(
					"phase %d stage %d driver is invalid: %w",
					phaseIndex,
					stageIndex,
					err,
				)
			}

			for inputIndex := range stage.ResolvedInputs {
				input := stage.ResolvedInputs[inputIndex]
				if input.State == ResolvedInputStateIncomplete {
					if strings.TrimSpace(input.IncompleteReason) == "" || input.Digest != "" {
						return "", fmt.Errorf(
							"phase %d stage %d resolved input %d has an invalid incomplete marker",
							phaseIndex,
							stageIndex,
							inputIndex,
						)
					}

					if stage.QualificationEligible {
						return "", fmt.Errorf(
							"phase %d stage %d resolved input %d is incomplete",
							phaseIndex,
							stageIndex,
							inputIndex,
						)
					}

					if !stage.DiagnosticOnly {
						return "", fmt.Errorf(
							"phase %d stage %d resolved input %d requires diagnostic-only execution",
							phaseIndex,
							stageIndex,
							inputIndex,
						)
					}

					continue
				}

				if input.State != ResolvedInputStateComplete ||
					strings.TrimSpace(input.Resolved) == "" ||
					!IsSHA256Digest(input.Digest) ||
					input.IncompleteReason != "" {
					return "", fmt.Errorf(
						"phase %d stage %d resolved input %d is not completely resolved",
						phaseIndex,
						stageIndex,
						inputIndex,
					)
				}
			}
		}
	}

	canonicalEnvironment := canonicalizeEnvironmentBindings(environment)
	canonicalPlan := canonicalizeResolvedPlan(plan)
	canonicalProvenance := provenance.DeepCopy()
	sort.Strings(canonicalProvenance.ResolverImages)

	content, err := json.Marshal(resolvedExecutionPlanDigestInput{
		Schema:           ResolvedExecutionPlanDigestSchema,
		ParticipantCount: participantCount,
		Environment:      canonicalEnvironment,
		Plan:             *canonicalPlan,
		Provenance:       *canonicalProvenance,
	})
	if err != nil {
		return "", fmt.Errorf("encode resolved execution plan: %w", err)
	}

	return sha256Digest(content), nil
}

func canonicalizeEnvironmentBindings(
	environment *CertificationEnvironmentBindings,
) *CertificationEnvironmentBindings {
	if environment == nil {
		return nil
	}

	canonical := environment.DeepCopy()
	sort.Slice(canonical.Storage, func(left, right int) bool {
		return canonical.Storage[left].Name < canonical.Storage[right].Name
	})

	for index := range canonical.Storage {
		sort.Slice(canonical.Storage[index].AccessModes, func(left, right int) bool {
			return canonical.Storage[index].AccessModes[left] < canonical.Storage[index].AccessModes[right]
		})
	}

	sort.Slice(canonical.Networks, func(left, right int) bool {
		return canonical.Networks[left].Name < canonical.Networks[right].Name
	})
	sort.Slice(canonical.SecretRefs, func(left, right int) bool {
		return canonical.SecretRefs[left].Name < canonical.SecretRefs[right].Name
	})
	sort.Slice(canonical.Resolvers, func(left, right int) bool {
		return canonical.Resolvers[left].Name < canonical.Resolvers[right].Name
	})
	sort.Slice(canonical.ModelCaches, func(left, right int) bool {
		return canonical.ModelCaches[left].Name < canonical.ModelCaches[right].Name
	})
	sort.Slice(canonical.ObjectStores, func(left, right int) bool {
		return canonical.ObjectStores[left].Name < canonical.ObjectStores[right].Name
	})
	sort.Slice(canonical.Resources.Claims, func(left, right int) bool {
		return canonical.Resources.Claims[left].Name < canonical.Resources.Claims[right].Name
	})

	return canonical
}

func canonicalizeResolvedPlan(plan ResolvedCertificationPlan) *ResolvedCertificationPlan {
	canonical := plan.DeepCopy()
	for phaseIndex := range canonical.Phases {
		for stageIndex := range canonical.Phases[phaseIndex].Stages {
			stage := &canonical.Phases[phaseIndex].Stages[stageIndex]
			sort.Slice(stage.Test.Metrics, func(left, right int) bool {
				return stage.Test.Metrics[left].Name < stage.Test.Metrics[right].Name
			})
			sort.Slice(stage.Test.Topology.Roles, func(left, right int) bool {
				return stage.Test.Topology.Roles[left].Name < stage.Test.Topology.Roles[right].Name
			})
			sort.Strings(stage.Test.Security.Capabilities)
			sort.Slice(stage.Test.Security.HostPaths, func(left, right int) bool {
				return stage.Test.Security.HostPaths[left].MountPath <
					stage.Test.Security.HostPaths[right].MountPath
			})

			if stage.Test.Driver.Container != nil {
				sort.Slice(stage.Test.Driver.Container.Resources.Claims, func(left, right int) bool {
					return stage.Test.Driver.Container.Resources.Claims[left].Name <
						stage.Test.Driver.Container.Resources.Claims[right].Name
				})
			}

			sort.Slice(stage.Parameters, func(left, right int) bool {
				return stage.Parameters[left].Name < stage.Parameters[right].Name
			})
			sort.Slice(stage.ParticipantRoles, func(left, right int) bool {
				return stage.ParticipantRoles[left].Role < stage.ParticipantRoles[right].Role
			})

			for roleIndex := range stage.ParticipantRoles {
				sort.Slice(stage.ParticipantRoles[roleIndex].ParticipantIndexes, func(left, right int) bool {
					return stage.ParticipantRoles[roleIndex].ParticipantIndexes[left] <
						stage.ParticipantRoles[roleIndex].ParticipantIndexes[right]
				})
			}

			if stage.Retry != nil {
				sort.Slice(stage.Retry.RetryableFailures, func(left, right int) bool {
					return stage.Retry.RetryableFailures[left] < stage.Retry.RetryableFailures[right]
				})
			}

			sort.Slice(stage.DependsOn, func(left, right int) bool {
				return stage.DependsOn[left].Stage < stage.DependsOn[right].Stage
			})
			sort.Slice(stage.Assertions, func(left, right int) bool {
				return stage.Assertions[left].Name < stage.Assertions[right].Name
			})
			sort.Slice(stage.ResolvedInputs, func(left, right int) bool {
				return stage.ResolvedInputs[left].Name < stage.ResolvedInputs[right].Name
			})
		}
	}

	return canonical
}

func validateResolvedStage(stage ResolvedStagePlan, participantCount int32) error {
	if stage.PerDeviceCoverageInterval != nil {
		if stage.Schedule.Mode == CheckExecutionModeRunOnce {
			return errors.New("per-device coverage requires repeated execution")
		}

		interval := stage.PerDeviceCoverageInterval.Duration
		if interval <= 0 || interval%time.Second != 0 {
			return errors.New("per-device coverage interval must be a positive whole number of seconds")
		}

		if stage.Schedule.CoverageWindow == nil ||
			interval > stage.Schedule.CoverageWindow.Duration {
			return errors.New("per-device coverage interval must not exceed coverageWindow")
		}
	}

	for _, hostPath := range stage.Test.Security.HostPaths {
		if !isCleanAbsolutePath(hostPath.MountPath) ||
			isReservedSignalMount(hostPath.MountPath) {
			return fmt.Errorf("host path mount %q is invalid", hostPath.MountPath)
		}

		if !isCleanAbsolutePath(hostPath.Path) ||
			!isAllowedSignalHostPath(hostPath.Path) {
			return fmt.Errorf("host path %q is outside approved roots", hostPath.Path)
		}

		if isReadOnlySignalHostPath(hostPath.Path) && !hostPath.ReadOnly {
			return fmt.Errorf("host path %q must be read-only", hostPath.Path)
		}
	}

	topology := stage.Test.Topology
	if len(topology.Roles) == 0 {
		if len(stage.ParticipantRoles) != 0 {
			return errors.New("participant role assignments require role-aware test topology")
		}

		if participantCount < topology.MinParticipants ||
			participantCount > topology.MaxParticipants {
			return errors.New("run participant count does not satisfy test topology")
		}

		return nil
	}

	roleDefinitions := make(map[string]TestParticipantRole, len(topology.Roles))
	for _, role := range topology.Roles {
		roleDefinitions[role.Name] = role
	}

	roleCounts := make(map[string]int32, len(stage.ParticipantRoles))
	participants := make(map[int32]string, participantCount)

	var assignedCount int32

	for _, binding := range stage.ParticipantRoles {
		role, found := roleDefinitions[binding.Role]
		if !found {
			return fmt.Errorf("participant role %q is not defined by the test", binding.Role)
		}

		count := int32(len(binding.ParticipantIndexes))
		if count < role.MinCount || count > role.MaxCount {
			return fmt.Errorf("participant role %q count does not satisfy test topology", binding.Role)
		}

		roleCounts[binding.Role] = count
		assignedCount += count

		for _, participantIndex := range binding.ParticipantIndexes {
			if participantIndex < 0 || participantIndex >= participantCount {
				return fmt.Errorf("participant index %d is outside the Run participants", participantIndex)
			}

			if previousRole, found := participants[participantIndex]; found {
				return fmt.Errorf(
					"participant index %d is assigned to both %q and %q",
					participantIndex,
					previousRole,
					binding.Role,
				)
			}

			participants[participantIndex] = binding.Role
		}
	}

	for _, role := range topology.Roles {
		if roleCounts[role.Name] < role.MinCount {
			return fmt.Errorf("participant role %q does not satisfy its minimum count", role.Name)
		}
	}

	if assignedCount < topology.MinParticipants || assignedCount > topology.MaxParticipants {
		return errors.New("resolved participant assignments do not satisfy test topology")
	}

	return nil
}

func isCleanAbsolutePath(value string) bool {
	return strings.HasPrefix(value, "/") &&
		!strings.Contains(value, "//") &&
		!strings.Contains(value, "/./") &&
		!strings.Contains(value, "/../") &&
		!strings.HasSuffix(value, "/.") &&
		!strings.HasSuffix(value, "/..")
}

func isAllowedSignalHostPath(value string) bool {
	return value == "/proc/cpuinfo" ||
		value == "/proc/meminfo" ||
		value == "/proc/version" ||
		value == "/proc/sys/kernel/random/boot_id" ||
		value == "/sys" ||
		strings.HasPrefix(value, "/sys/") ||
		value == "/var/log" ||
		value == "/run/log" ||
		value == "/etc/machine-id" ||
		value == "/var/lib/project-signal" ||
		strings.HasPrefix(value, "/var/lib/project-signal/")
}

func isReservedSignalMount(value string) bool {
	return value == "/scripts" ||
		strings.HasPrefix(value, "/scripts/") ||
		value == "/results" ||
		strings.HasPrefix(value, "/results/")
}

func isReadOnlySignalHostPath(value string) bool {
	return strings.HasPrefix(value, "/proc/") ||
		value == "/sys" ||
		strings.HasPrefix(value, "/sys/") ||
		value == "/var/log" ||
		value == "/run/log" ||
		value == "/etc/machine-id"
}

func validateResolvedDriver(driver CertificationTestDriver) error {
	switch driver.Type {
	case TestDriverTypeScript:
		if driver.Script == nil {
			return errors.New("script driver is missing")
		}

		if !IsDigestPinnedImage(driver.Script.RunnerImage) {
			return errors.New("script runner image must be digest-pinned")
		}

		if strings.TrimSpace(driver.Script.Artifact) == "" {
			return errors.New("script artifact is missing")
		}
	case TestDriverTypeContainer:
		if driver.Container == nil || !IsDigestPinnedImage(driver.Container.Image) {
			return errors.New("container image must be digest-pinned")
		}
	case TestDriverTypeWorkflowTemplate:
		if driver.WorkflowTemplate == nil || !IsSHA256Digest(driver.WorkflowTemplate.Digest) {
			return errors.New("workflow template digest is invalid")
		}
	default:
		return errors.New("driver type is invalid")
	}

	return nil
}
