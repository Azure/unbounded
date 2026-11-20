// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalworkload

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const MaxPolicyBytes = 16 * 1024

var allowedHostPathRoots = []string{"/proc", "/sys", "/var/lib/project-signal"}

func HostPathVolumeName(index int) string {
	return fmt.Sprintf("host-%02d", index)
}

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

	if len(policy.HostPaths) > 16 {
		return errors.New("workload policy must not contain more than 16 host paths")
	}

	mountPaths := map[string]struct{}{"/scripts": {}, "/results": {}}

	for index := range policy.HostPaths {
		hostPath := &policy.HostPaths[index]
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

		if !allowedHostPath(hostPath.Path) {
			return fmt.Errorf("workload policy host path %d source %q is not allowlisted", index, hostPath.Path)
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

func allowedHostPath(value string) bool {
	for _, root := range allowedHostPathRoots {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}

	return false
}

func validateAbsolutePath(name, value string) error {
	if value == "" || !strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}

	return nil
}
