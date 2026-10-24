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

	if err := validateCapabilities(policy.Capabilities); err != nil {
		return err
	}

	if policy.WorkingDir != "" {
		if err := validateAbsolutePath("workingDir", policy.WorkingDir); err != nil {
			return err
		}
	}

	if len(policy.HostPaths) > 16 {
		return errors.New("workload policy must not contain more than 16 host paths")
	}

	names := map[string]struct{}{"scripts": {}, "results": {}}
	mountPaths := map[string]struct{}{"/scripts": {}, "/results": {}}

	for index := range policy.HostPaths {
		hostPath := &policy.HostPaths[index]
		if errors := validation.IsDNS1123Label(hostPath.Name); len(errors) > 0 {
			return fmt.Errorf("workload policy host path %d name is invalid: %s", index, strings.Join(errors, ", "))
		}

		if _, exists := names[hostPath.Name]; exists {
			return fmt.Errorf("workload policy host path %d duplicates reserved or existing name %q", index, hostPath.Name)
		}

		names[hostPath.Name] = struct{}{}

		if err := validateAbsolutePath(fmt.Sprintf("workload policy host path %d mountPath", index), hostPath.MountPath); err != nil {
			return err
		}

		if _, exists := mountPaths[hostPath.MountPath]; exists {
			return fmt.Errorf(
				"workload policy host path %d duplicates reserved or existing mountPath %q",
				index,
				hostPath.MountPath,
			)
		}

		mountPaths[hostPath.MountPath] = struct{}{}
		if err := validateAbsolutePath(fmt.Sprintf("workload policy host path %d path", index), hostPath.Path); err != nil {
			return err
		}
	}

	return nil
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

func validateCapabilities(capabilities []corev1.Capability) error {
	seen := map[corev1.Capability]struct{}{}

	for _, capability := range capabilities {
		if !capabilityPattern.MatchString(string(capability)) {
			return fmt.Errorf("workload policy capability %q is invalid", capability)
		}

		if _, exists := seen[capability]; exists {
			return fmt.Errorf("workload policy capability %q is repeated", capability)
		}

		seen[capability] = struct{}{}
	}

	return nil
}

func validateAbsolutePath(name, value string) error {
	if value == "" || !strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}

	return nil
}
