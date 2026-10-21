// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalworkload

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestPolicyRoundTripPreservesBoundedExecutionSettings(t *testing.T) {
	t.Parallel()

	policy := signalv1.WorkloadPolicy{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("40Gi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: ptr(false),
			RunAsUser:  ptr(int64(0)),
			Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"IPC_LOCK"},
				Drop: []corev1.Capability{"NET_RAW"},
			},
		},
		WorkingDir: "/opt/diagnostic",
		Results:    &corev1.EmptyDirVolumeSource{SizeLimit: quantity("2Gi")},
		Volumes: []signalv1.WorkloadVolume{
			{
				Name:      "host-proc",
				MountPath: "/host/proc",
				ReadOnly:  true,
				HostPath:  &corev1.HostPathVolumeSource{Path: "/proc"},
			},
			{
				Name:      "work",
				MountPath: "/work",
				EmptyDir:  &corev1.EmptyDirVolumeSource{SizeLimit: quantity("8Gi")},
			},
			{
				Name:                  "scratch",
				MountPath:             "/scratch",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "diagnostic-scratch"},
			},
		},
	}

	content, err := Encode(policy)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Decode(content)
	if err != nil {
		t.Fatal(err)
	}

	if got.WorkingDir != policy.WorkingDir ||
		got.Resources.Limits.Cpu().Cmp(*policy.Resources.Limits.Cpu()) != 0 ||
		len(got.Volumes) != 3 ||
		got.Volumes[0].HostPath.Path != "/proc" ||
		got.Volumes[1].EmptyDir.SizeLimit.Cmp(*quantity("8Gi")) != 0 ||
		got.Volumes[2].PersistentVolumeClaim.ClaimName != "diagnostic-scratch" {
		t.Fatalf("decoded policy = %#v", got)
	}
}

func TestValidateRejectsUnsafeOrAmbiguousPolicy(t *testing.T) {
	t.Parallel()

	tests := map[string]signalv1.WorkloadPolicy{
		"reserved mount": {
			Volumes: []signalv1.WorkloadVolume{{
				Name:      "replacement",
				MountPath: "/results",
				EmptyDir:  &corev1.EmptyDirVolumeSource{},
			}},
		},
		"multiple sources": {
			Volumes: []signalv1.WorkloadVolume{{
				Name:      "scratch",
				MountPath: "/scratch",
				HostPath:  &corev1.HostPathVolumeSource{Path: "/var/lib/scratch"},
				EmptyDir:  &corev1.EmptyDirVolumeSource{},
			}},
		},
		"relative host path": {
			Volumes: []signalv1.WorkloadVolume{{
				Name:      "scratch",
				MountPath: "/scratch",
				HostPath:  &corev1.HostPathVolumeSource{Path: "scratch"},
			}},
		},
		"limit below request": {
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			},
		},
		"capability overlap": {
			SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"SYS_ADMIN"},
				Drop: []corev1.Capability{"SYS_ADMIN"},
			}},
		},
	}

	for name, policy := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := Validate(policy); err == nil {
				t.Fatal("Validate accepted invalid policy")
			}
		})
	}
}

func TestDecodeRejectsOversizedPolicy(t *testing.T) {
	t.Parallel()

	policy := signalv1.WorkloadPolicy{WorkingDir: "/" + strings.Repeat("a", MaxPolicyBytes)}
	if _, err := Encode(policy); err == nil {
		t.Fatal("Encode accepted oversized policy")
	}
}

func quantity(value string) *resource.Quantity {
	result := resource.MustParse(value)

	return &result
}

func ptr[T any](value T) *T {
	return &value
}
