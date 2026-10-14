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
		updateStrategy: "OnDelete",
		nvidiaIMEXMode: string(v1alpha3.NVIDIAIMEXModeDRA),
	}, &out)
	require.NoError(t, err)

	var got v1alpha3.MachineConfiguration
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "gb200-dra"}, &got))
	require.NotNil(t, got.Spec.Template.Agent)
	require.Empty(t, got.Spec.Template.Agent.Image)
	require.NotNil(t, got.Spec.Template.Agent.NVIDIA)
	require.NotNil(t, got.Spec.Template.Agent.NVIDIA.IMEX)
	require.Equal(t, v1alpha3.NVIDIAIMEXModeDRA, got.Spec.Template.Agent.NVIDIA.IMEX.Mode)
	require.Contains(t, out.String(), "nvidia-imex-mode")
	require.Contains(t, out.String(), "DRA")
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
