// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalworkload

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const MaxPolicyBytes = 16 * 1024

var capabilityPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func Encode(policy signalv1.WorkloadPolicy) (string, error) {
	if err := Validate(policy); err != nil {
		return "", err
	}

	content, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode workload policy: %w", err)
	}

	if len(content) > MaxPolicyBytes {
		return "", fmt.Errorf("workload policy exceeds %d bytes", MaxPolicyBytes)
	}

	return string(content), nil
}

func Decode(content string) (signalv1.WorkloadPolicy, error) {
	var policy signalv1.WorkloadPolicy

	if strings.TrimSpace(content) == "" {
		content = "{}"
	}

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&policy); err != nil {
		return policy, fmt.Errorf("decode workload policy: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return policy, errors.New("workload policy contains trailing JSON values")
		}

		return policy, fmt.Errorf("decode trailing workload policy value: %w", err)
	}

	if err := Validate(policy); err != nil {
		return policy, err
	}

	return policy, nil
}

func Validate(policy signalv1.WorkloadPolicy) error {
	if err := validateResources(policy.Resources); err != nil {
		return err
	}

	if err := validateSecurityContext(policy.SecurityContext); err != nil {
		return err
	}

	if policy.KueueTopology != nil {
		if errors := validation.IsQualifiedName(policy.KueueTopology.RequiredLevel); len(errors) > 0 {
			return fmt.Errorf(
				"workload policy kueueTopology requiredLevel is invalid: %s",
				strings.Join(errors, ", "),
			)
		}
	}

	if policy.WorkingDir != "" {
		if err := validateAbsolutePath("workingDir", policy.WorkingDir); err != nil {
			return err
		}
	}

	if len(policy.Volumes) > 16 {
		return errors.New("workload policy must not contain more than 16 volumes")
	}

	names := map[string]struct{}{"scripts": {}, "results": {}}
	mountPaths := map[string]struct{}{"/scripts": {}, "/results": {}}

	for index := range policy.Volumes {
		volume := &policy.Volumes[index]
		if errors := validation.IsDNS1123Label(volume.Name); len(errors) > 0 {
			return fmt.Errorf("workload policy volume %d name is invalid: %s", index, strings.Join(errors, ", "))
		}

		if _, exists := names[volume.Name]; exists {
			return fmt.Errorf("workload policy volume %d duplicates reserved or existing name %q", index, volume.Name)
		}

		names[volume.Name] = struct{}{}

		if err := validateAbsolutePath(fmt.Sprintf("workload policy volume %d mountPath", index), volume.MountPath); err != nil {
			return err
		}

		if _, exists := mountPaths[volume.MountPath]; exists {
			return fmt.Errorf(
				"workload policy volume %d duplicates reserved or existing mountPath %q",
				index,
				volume.MountPath,
			)
		}

		mountPaths[volume.MountPath] = struct{}{}

		sources := 0
		if volume.HostPath != nil {
			sources++

			if err := validateAbsolutePath(fmt.Sprintf("workload policy volume %d hostPath", index), volume.HostPath.Path); err != nil {
				return err
			}
		}

		if volume.EmptyDir != nil {
			sources++

			if err := validateQuantity(fmt.Sprintf("workload policy volume %d emptyDir sizeLimit", index), volume.EmptyDir.SizeLimit); err != nil {
				return err
			}
		}

		if volume.PersistentVolumeClaim != nil {
			sources++

			if errors := validation.IsDNS1123Subdomain(volume.PersistentVolumeClaim.ClaimName); len(errors) > 0 {
				return fmt.Errorf(
					"workload policy volume %d persistentVolumeClaim claimName is invalid: %s",
					index,
					strings.Join(errors, ", "),
				)
			}
		}

		if sources != 1 {
			return fmt.Errorf("workload policy volume %d must define exactly one source", index)
		}
	}

	return validateQuantity("workload policy results sizeLimit", sizeLimit(policy.Results))
}

func validateResources(resources corev1.ResourceRequirements) error {
	for name, quantity := range resources.Requests {
		if quantity.Sign() < 0 {
			return fmt.Errorf("workload policy resource request %q must not be negative", name)
		}
	}

	for name, quantity := range resources.Limits {
		if quantity.Sign() < 0 {
			return fmt.Errorf("workload policy resource limit %q must not be negative", name)
		}

		request, exists := resources.Requests[name]
		if exists && quantity.Cmp(request) < 0 {
			return fmt.Errorf("workload policy resource limit %q must not be less than its request", name)
		}
	}

	return nil
}

func validateSecurityContext(context *corev1.SecurityContext) error {
	if context == nil || context.Capabilities == nil {
		return nil
	}

	seen := map[corev1.Capability]string{}

	for listName, capabilities := range map[string][]corev1.Capability{
		"add":  context.Capabilities.Add,
		"drop": context.Capabilities.Drop,
	} {
		for _, capability := range capabilities {
			if !capabilityPattern.MatchString(string(capability)) {
				return fmt.Errorf("workload policy capability %q is invalid", capability)
			}

			if previous, exists := seen[capability]; exists {
				return fmt.Errorf(
					"workload policy capability %q appears in both or repeatedly within %s and %s",
					capability,
					previous,
					listName,
				)
			}

			seen[capability] = listName
		}
	}

	return nil
}

func validateAbsolutePath(name, value string) error {
	if value == "" || !strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}

	return nil
}

func validateQuantity(name string, quantity *resource.Quantity) error {
	if quantity != nil && quantity.Sign() < 0 {
		return fmt.Errorf("%s must not be negative", name)
	}

	return nil
}

func sizeLimit(source *corev1.EmptyDirVolumeSource) *resource.Quantity {
	if source == nil {
		return nil
	}

	return source.SizeLimit
}
