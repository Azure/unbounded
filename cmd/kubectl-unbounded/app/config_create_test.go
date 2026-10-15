// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestRunConfigCreate_NVIDIAIMEXDRA(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	var out bytes.Buffer

	err := runConfigCreate(context.Background(), kubeClient, "gb200-dra", configCreateOpts{
		updateStrategy:    "OnDelete",
		nvidiaIMEXMode:    string(v1alpha3.NVIDIAIMEXModeDRA),
		nvidiaHostProfile: "hillsboro-gb200-gpu-operator-ready",
		nvidiaProfileVer:  "v1",
		nvidiaContractVer: "hillsboro.nvidia-host-capabilities/v1",
		nvidiaArtifact:    "/etc/unbounded/host-capabilities/nvidia-gpu-operator.json",
		nvidiaProfileHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, &out)
	require.NoError(t, err)

	var got v1alpha3.MachineConfiguration
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "gb200-dra"}, &got))
	require.NotNil(t, got.Spec.Template.Agent)
	require.Empty(t, got.Spec.Template.Agent.Image)
	require.NotNil(t, got.Spec.Template.Agent.NVIDIA)
	require.NotNil(t, got.Spec.Template.Agent.NVIDIA.IMEX)
	require.Equal(t, v1alpha3.NVIDIAIMEXModeDRA, got.Spec.Template.Agent.NVIDIA.IMEX.Mode)
	require.Equal(t, "hillsboro-gb200-gpu-operator-ready", got.Spec.Template.Agent.NVIDIA.IMEX.HostProfile)
	require.Equal(t, "v1", got.Spec.Template.Agent.NVIDIA.IMEX.HostProfileVersion)
	require.Equal(t, "hillsboro.nvidia-host-capabilities/v1", got.Spec.Template.Agent.NVIDIA.IMEX.HostContractVersion)
	require.Equal(t, "/etc/unbounded/host-capabilities/nvidia-gpu-operator.json", got.Spec.Template.Agent.NVIDIA.IMEX.HostArtifactPath)
	require.Equal(t, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", got.Spec.Template.Agent.NVIDIA.IMEX.HostProfileDigest)
	require.Contains(t, out.String(), "nvidia-imex-mode")
	require.Contains(t, out.String(), "DRA")
}

func TestRunConfigCreate_RequiresNVIDIAHostQualification(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))

	tests := []struct {
		name    string
		opts    configCreateOpts
		wantErr string
	}{
		{
			name: "profile",
			opts: configCreateOpts{
				updateStrategy: "OnDelete",
				nvidiaIMEXMode: string(v1alpha3.NVIDIAIMEXModeDRA),
			},
			wantErr: "--nvidia-host-profile is required",
		},
		{
			name: "contract version",
			opts: configCreateOpts{
				updateStrategy:    "OnDelete",
				nvidiaIMEXMode:    string(v1alpha3.NVIDIAIMEXModeDRA),
				nvidiaHostProfile: "profile",
				nvidiaProfileVer:  "v1",
			},
			wantErr: "--nvidia-host-contract-version is required",
		},
		{
			name: "artifact path",
			opts: configCreateOpts{
				updateStrategy:    "OnDelete",
				nvidiaIMEXMode:    string(v1alpha3.NVIDIAIMEXModeDRA),
				nvidiaHostProfile: "profile",
				nvidiaProfileVer:  "v1",
				nvidiaContractVer: "contract/v1",
			},
			wantErr: "--nvidia-host-artifact-path is required",
		},
		{
			name: "qualification without DRA",
			opts: configCreateOpts{
				updateStrategy:    "OnDelete",
				nvidiaContractVer: "contract/v1",
			},
			wantErr: "NVIDIA host qualification flags require --nvidia-imex-mode=DRA",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			err := runConfigCreate(context.Background(), kubeClient, "invalid", tt.opts, &bytes.Buffer{})
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestRunConfigCreate_RejectsUnknownNVIDIAIMEXMode(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	err := runConfigCreate(context.Background(), kubeClient, "invalid", configCreateOpts{
		updateStrategy: "OnDelete",
		nvidiaIMEXMode: "legacy",
	}, &bytes.Buffer{})
	require.ErrorContains(t, err, "invalid NVIDIA IMEX mode")
}
